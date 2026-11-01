package controller

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

// atomicProvider is the fakeProvider for concurrent access from a running manager.
type atomicProvider struct {
	offline, on, off chan struct{}
}

func newAtomicProvider() *atomicProvider {
	return &atomicProvider{offline: make(chan struct{}), on: make(chan struct{}, 100), off: make(chan struct{}, 100)}
}
func (p *atomicProvider) PowerOn(context.Context) error  { p.on <- struct{}{}; return nil }
func (p *atomicProvider) PowerOff(context.Context) error { p.off <- struct{}{}; return nil }
func (p *atomicProvider) Offline(context.Context) (bool, error) {
	select {
	case <-p.offline:
		return true, nil
	default:
		return false, nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func received(ch chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
}

// Runs the whole lifecycle against a real API server: idle, reap, delete, wake.
func TestLifecycleAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set, use make test")
	}
	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	provider := newAtomicProvider()
	providers := map[string]power.Provider{"mini1": provider}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := (&NodeReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("nekromancer"), Clock: clock.RealClock{},
		Providers: providers, DefaultIdleTimeout: 2 * time.Second, PowerOffRetry: time.Minute,
	}).SetupWithManager(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	if err := (&WakeReconciler{
		Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder("nekromancer"), Clock: clock.RealClock{},
		Cooldown: time.Minute, Providers: providers,
		Hosts: []config.Host{{Name: "mini1", Labels: map[string]string{"workload": "plex"}}},
	}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("manager: %v", err)
		}
	}()
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}

	createReadyNode := func(name string, labels map[string]string) {
		t.Helper()
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		n.Status.Conditions = []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
			LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now(),
		}}
		if err := c.Status().Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	getNode := func(name string) (*corev1.Node, bool) {
		var n corev1.Node
		err := c.Get(ctx, client.ObjectKey{Name: name}, &n)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return &n, err == nil
	}

	managed := map[string]string{nodes.LabelManaged: "true", "workload": "plex"}
	createReadyNode("mini1", managed)
	// Guardrail bystanders, with no way out of Ready and no provider either.
	createReadyNode("worker-unmanaged", map[string]string{"workload": "plex"})
	createReadyNode("control", map[string]string{nodes.LabelManaged: "true", nodes.LabelControlPlane: ""})

	// A pod keeps the node from idling. Grace 0: no kubelet here to finish termination.
	zero := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "mini1", TerminationGracePeriodSeconds: &zero,
			Containers: []corev1.Container{{Name: "c", Image: "x"}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	if n, _ := getNode("mini1"); nodes.PowerOffRequested(n) {
		t.Fatal("reaped a node with a running workload")
	}

	// Manual shutdown: cordon, evict, request power off, keep the Node until offline.
	n, _ := getNode("mini1")
	base := n.DeepCopy()
	setAnnotation(n, nodes.AnnotationPowerState, nodes.PowerStateOff)
	if err := c.Patch(ctx, n, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workload evicted", func() bool {
		return apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
	})
	eventually(t, "power off requested", received(provider.off))
	n, ok := getNode("mini1")
	if !ok || !n.Spec.Unschedulable || n.Annotations[nodes.AnnotationShutdownRequested] == "" {
		t.Fatalf("node after power off request: ok=%v %+v", ok, n)
	}

	close(provider.offline)
	eventually(t, "node deleted once offline", func() bool { _, ok := getNode("mini1"); return !ok })

	// Pending pod wakes the host.
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plex", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"workload": "plex"},
			Containers:   []corev1.Container{{Name: "c", Image: "x"}},
		},
	}
	if err := c.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}
	if err := c.Status().Update(ctx, pending); err != nil {
		t.Fatal(err)
	}
	// The unmanaged worker carries the label too but is no inventory host, so it does not suppress the wake.
	eventually(t, "wake dispatched", received(provider.on))

	// Guardrails held throughout.
	for _, name := range []string{"worker-unmanaged", "control"} {
		n, ok := getNode(name)
		if !ok || n.Spec.Unschedulable || len(n.Annotations) != 0 {
			t.Errorf("%s touched: ok=%v annotations=%v", name, ok, n.Annotations)
		}
	}
}
