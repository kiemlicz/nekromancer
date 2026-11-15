package controller

import (
	"context"
	"errors"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

const hostnameLabel = "kubernetes.io/hostname"

// pendingPoll also covers wakes lost to a host still shutting down.
const pendingPoll = 30 * time.Second

// WakeReconciler powers on a host for pods that nothing can schedule.
//
// Hosts with a Ready Node suppress waking: the pod may be blocked by something a
// second machine would not fix, which would otherwise flap wake and idle reap.
type WakeReconciler struct {
	client.Client
	Recorder  events.EventRecorder
	Clock     clock.Clock
	Cooldown  time.Duration
	Hosts     []config.Host
	Providers map[string]power.Provider

	// lastWake is safe unsynchronised at the default single worker. Losing it on
	// restart costs at most one redundant wake.
	lastWake map[string]time.Time
}

func (r *WakeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !wantsWake(&pod) {
		return ctrl.Result{}, nil
	}
	candidates, err := r.matchingHosts(ctx, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(candidates) == 0 {
		return ctrl.Result{}, nil
	}
	wakeable, err := r.wakeable(ctx, candidates)
	if err != nil || len(wakeable) == 0 {
		return ctrl.Result{RequeueAfter: pendingPoll}, err
	}

	now := r.Clock.Now()
	for _, h := range wakeable {
		if at, ok := r.lastWake[h.Name]; ok && now.Sub(at) < r.Cooldown {
			return ctrl.Result{RequeueAfter: pendingPoll}, nil
		}
	}
	if stalled := r.stalled(ctx, wakeable); stalled != "" {
		r.Recorder.Eventf(&pod, nil, corev1.EventTypeWarning, "WakeStalled", "Wake",
			"host %s was woken and is not confirmed down, but its Node is not Ready; not waking another host", stalled)
		return ctrl.Result{RequeueAfter: pendingPoll}, nil
	}
	// Least recently tried first, so a host that never came up does not starve the others.
	slices.SortStableFunc(wakeable, func(a, b config.Host) int {
		return r.lastWake[a.Name].Compare(r.lastWake[b.Name])
	})
	if r.lastWake == nil {
		r.lastWake = map[string]time.Time{}
	}
	var errs []error
	for _, h := range wakeable {
		// Recorded before the call: an ambiguous failure may still have woken the host.
		prev, had := r.lastWake[h.Name]
		r.lastWake[h.Name] = now
		err := r.Providers[h.Name].PowerOn(ctx)
		if power.IsNotSent(err) {
			if had {
				r.lastWake[h.Name] = prev
			} else {
				delete(r.lastWake, h.Name)
			}
			log.FromContext(ctx).Error(err, "waking host, trying the next one", "host", h.Name)
			errs = append(errs, err)
			continue
		}
		if err != nil {
			// Another host now would risk waking two, so wait out the cooldown.
			log.FromContext(ctx).Error(err, "waking host, outcome unknown", "host", h.Name)
			r.Recorder.Eventf(&pod, nil, corev1.EventTypeWarning, "WakeUncertain", "Wake", "waking host %s: %v", h.Name, err)
			return ctrl.Result{RequeueAfter: pendingPoll}, nil
		}
		log.FromContext(ctx).Info("woke host", "host", h.Name, "pod", client.ObjectKeyFromObject(&pod).String())
		r.Recorder.Eventf(&pod, nil, corev1.EventTypeNormal, "WakeDispatched", "Wake", "woke host %s", h.Name)
		return ctrl.Result{RequeueAfter: pendingPoll}, nil
	}
	return ctrl.Result{}, errors.Join(errs...)
}

// stalled returns a host woken earlier that is still not confirmed down. It booted,
// or may have, but never got a Ready Node: waking another would only repeat that.
// A probe error counts as up.
func (r *WakeReconciler) stalled(ctx context.Context, wakeable []config.Host) string {
	for _, h := range wakeable {
		if _, ok := r.lastWake[h.Name]; !ok {
			continue
		}
		off, err := r.Providers[h.Name].Offline(ctx)
		if err != nil {
			log.FromContext(ctx).Error(err, "probing woken host", "host", h.Name)
		}
		if err != nil || !off {
			return h.Name
		}
	}
	return ""
}

// wantsWake requires an explicit placement constraint: a pod that fits anywhere
// and is unschedulable has some other problem that no specific host would solve.
func wantsWake(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" || pod.Status.Phase != corev1.PodPending || !pod.DeletionTimestamp.IsZero() || nodes.IsDaemonSetPod(pod) {
		return false
	}
	if len(pod.Spec.NodeSelector) == 0 && !hasRequiredAffinity(pod) {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

func hasRequiredAffinity(pod *corev1.Pod) bool {
	a := pod.Spec.Affinity
	return a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil
}

func (r *WakeReconciler) matchingHosts(ctx context.Context, pod *corev1.Pod) ([]config.Host, error) {
	required := nodeaffinity.GetRequiredNodeAffinity(pod)
	var out []config.Host
	for _, h := range r.Hosts {
		labels := map[string]string{hostnameLabel: h.Name}
		for k, v := range h.Labels {
			labels[k] = v
		}
		ok, err := required.Match(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.Name, Labels: labels}})
		if err != nil {
			log.FromContext(ctx).Error(err, "pod has an invalid node affinity")
			return nil, nil
		}
		if ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// wakeable returns candidates without a Node or with a NotReady one, or nil when
// any candidate is Ready. Waking a host that is merely cut off does nothing.
// A Node mid power off is skipped: it can be woken once it is gone.
func (r *WakeReconciler) wakeable(ctx context.Context, candidates []config.Host) ([]config.Host, error) {
	var out []config.Host
	for _, h := range candidates {
		var node corev1.Node
		err := r.Get(ctx, client.ObjectKey{Name: h.Name}, &node)
		switch {
		case apierrors.IsNotFound(err):
			out = append(out, h)
		case err != nil:
			return nil, err
		case nodes.PowerOffRequested(&node):
		case nodes.Ready(&node):
			return nil, nil
		default:
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *WakeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("wake").
		For(&corev1.Pod{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return wantsWake(obj.(*corev1.Pod))
		}))).
		Complete(r)
}
