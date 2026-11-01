// Package controller implements the shutdown and wake reconcilers.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

const (
	// PodNodeNameIndex is the cache index of pods by the node they are bound to.
	PodNodeNameIndex = "spec.nodeName"

	shutdownPoll = 5 * time.Second
)

// NodeReconciler drives managed nodes through idle detection, drain, power off and deletion.
// All progress lives on the Node, so it resumes across restarts.
type NodeReconciler struct {
	client.Client
	Recorder           events.EventRecorder
	Clock              clock.Clock
	Providers          map[string]power.Provider
	DefaultIdleTimeout time.Duration
	PowerOffRetry      time.Duration
}

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !nodes.Managed(&node) || !node.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	// Without a way to power the host off or verify it, draining would only strand the node.
	provider, ok := r.Providers[node.Name]
	if !ok {
		log.FromContext(ctx).Info("managed node is missing from the inventory, ignoring")
		r.event(&node, corev1.EventTypeWarning, "NotInInventory", "Ignore", "no inventory entry for this node")
		return ctrl.Result{}, nil
	}
	ready := nodes.Ready(&node)
	switch {
	case nodes.PowerOffRequested(&node):
		return r.shutdown(ctx, &node, provider, ready)
	case !ready:
		return ctrl.Result{}, r.notReady(ctx, &node)
	default:
		return r.idle(ctx, &node)
	}
}

func (r *NodeReconciler) idle(ctx context.Context, node *corev1.Node) (ctrl.Result, error) {
	busy, err := r.blockingPods(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	stamp, stamped := node.Annotations[nodes.AnnotationIdleSince]
	if len(busy) > 0 {
		if stamped {
			return ctrl.Result{}, r.patch(ctx, node, func(n *corev1.Node) { delete(n.Annotations, nodes.AnnotationIdleSince) })
		}
		return ctrl.Result{}, nil
	}

	timeout, err := nodes.IdleTimeout(node, r.DefaultIdleTimeout)
	if err != nil {
		r.event(node, corev1.EventTypeWarning, "InvalidIdleTimeout", "Parse", "%v, using %s", err, timeout)
	}
	now := r.Clock.Now()
	since, err := time.Parse(time.RFC3339, stamp)
	if !stamped || err != nil {
		err := r.patch(ctx, node, func(n *corev1.Node) { setAnnotation(n, nodes.AnnotationIdleSince, format(now)) })
		return ctrl.Result{RequeueAfter: timeout}, err
	}
	if remaining := since.Add(timeout).Sub(now); remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	r.event(node, corev1.EventTypeNormal, "IdleTimeout", "Shutdown", "idle since %s, shutting down", stamp)
	return ctrl.Result{}, r.patch(ctx, node, func(n *corev1.Node) { setAnnotation(n, nodes.AnnotationPowerState, nodes.PowerStateOff) })
}

// notReady leaves the Node in place: a host that was switched off and one that is
// cut off look the same from here. It only stops the idle timer, which would
// otherwise reap the host as soon as it returns.
func (r *NodeReconciler) notReady(ctx context.Context, node *corev1.Node) error {
	if _, stamped := node.Annotations[nodes.AnnotationIdleSince]; !stamped {
		return nil
	}
	return r.patch(ctx, node, func(n *corev1.Node) { delete(n.Annotations, nodes.AnnotationIdleSince) })
}

func (r *NodeReconciler) shutdown(ctx context.Context, node *corev1.Node, provider power.Provider, ready bool) (ctrl.Result, error) {
	if !node.Spec.Unschedulable {
		if err := r.patch(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = true }); err != nil {
			return ctrl.Result{}, err
		}
	}

	requested, wasRequested := node.Annotations[nodes.AnnotationShutdownRequested]
	if !wasRequested {
		// The host can't see a request now, so its silence would prove nothing later.
		// The Node watch resumes this once Ready returns.
		if !ready {
			r.event(node, corev1.EventTypeWarning, "WaitingForReady", "PowerOff", "node is not ready, shutdown waits until it is")
			return ctrl.Result{}, nil
		}
		remaining, err := r.drain(ctx, node)
		if err != nil {
			return ctrl.Result{}, err
		}
		if remaining > 0 {
			return ctrl.Result{RequeueAfter: shutdownPoll}, nil
		}
		return r.requestPowerOff(ctx, node, provider)
	}

	if r.confirmedOffline(ctx, provider) {
		return ctrl.Result{}, r.deleteNode(ctx, node)
	}
	at, err := time.Parse(time.RFC3339, requested)
	if err == nil && r.Clock.Now().Sub(at) < r.PowerOffRetry {
		return ctrl.Result{RequeueAfter: shutdownPoll}, nil
	}
	r.event(node, corev1.EventTypeWarning, "StillOnline", "PowerOff", "host still online since %s, requesting power off again", requested)
	return r.requestPowerOff(ctx, node, provider)
}

// requestPowerOff writes the annotation first: it is what the node-agent acts on.
func (r *NodeReconciler) requestPowerOff(ctx context.Context, node *corev1.Node, provider power.Provider) (ctrl.Result, error) {
	now := format(r.Clock.Now())
	if err := r.patch(ctx, node, func(n *corev1.Node) { setAnnotation(n, nodes.AnnotationShutdownRequested, now) }); err != nil {
		return ctrl.Result{}, err
	}
	if err := provider.PowerOff(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("powering off: %w", err)
	}
	r.event(node, corev1.EventTypeNormal, "PowerOff", "PowerOff", "power off requested")
	return ctrl.Result{RequeueAfter: shutdownPoll}, nil
}

// drain evicts what it can and returns how many pods still block shutdown.
// PodDisruptionBudgets are honoured: a refused eviction is simply retried.
func (r *NodeReconciler) drain(ctx context.Context, node *corev1.Node) (int, error) {
	pods, err := r.blockingPods(ctx, node)
	if err != nil {
		return 0, err
	}
	for i := range pods {
		pod := &pods[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}}
		err := r.SubResource("eviction").Create(ctx, pod, eviction)
		if err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("eviction refused", "pod", client.ObjectKeyFromObject(pod), "reason", err.Error())
		}
	}
	return len(pods), nil
}

// confirmedOffline treats a probe error as online: uncertainty must never delete a Node.
func (r *NodeReconciler) confirmedOffline(ctx context.Context, provider power.Provider) bool {
	offline, err := provider.Offline(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "probing host")
		return false
	}
	return offline
}

func (r *NodeReconciler) deleteNode(ctx context.Context, node *corev1.Node) error {
	r.event(node, corev1.EventTypeNormal, "Offline", "Delete", "host confirmed offline, deleting node")
	uid := node.UID
	return client.IgnoreNotFound(r.Delete(ctx, node, client.Preconditions{UID: &uid}))
}

func (r *NodeReconciler) blockingPods(ctx context.Context, node *corev1.Node) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list, client.MatchingFields{PodNodeNameIndex: node.Name}); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for i := range list.Items {
		if nodes.BlocksIdle(&list.Items[i]) {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

func (r *NodeReconciler) patch(ctx context.Context, node *corev1.Node, mutate func(*corev1.Node)) error {
	base := node.DeepCopy()
	mutate(node)
	return r.Patch(ctx, node, client.MergeFrom(base))
}

func (r *NodeReconciler) event(node *corev1.Node, kind, reason, action, note string, args ...any) {
	r.Recorder.Eventf(node, nil, kind, reason, action, note, args...)
}

func setAnnotation(n *corev1.Node, key, value string) {
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[key] = value
}

func format(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func podNodeName(obj client.Object) []string {
	if name := obj.(*corev1.Pod).Spec.NodeName; name != "" {
		return []string{name}
	}
	return nil
}

// SetupWithManager registers the pod index and watches.
func (r *NodeReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, PodNodeNameIndex, podNodeName); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("node").
		For(&corev1.Node{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			var out []reconcile.Request
			for _, name := range podNodeName(obj) {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
			}
			return out
		})).
		Complete(r)
}
