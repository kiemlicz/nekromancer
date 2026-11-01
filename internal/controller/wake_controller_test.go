package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

func pendingPod(mut ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "plex", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeSelector: map[string]string{"workload": "plex"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			}},
		},
	}
	for _, m := range mut {
		m(p)
	}
	return p
}

type wakeEnv struct {
	t         *testing.T
	rec       *WakeReconciler
	clock     *clocktesting.FakeClock
	providers map[string]*fakeProvider
}

func newWakeEnv(t *testing.T, objs ...client.Object) *wakeEnv {
	t.Helper()
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	clk := clocktesting.NewFakeClock(t0)
	a, b, c3 := &fakeProvider{}, &fakeProvider{}, &fakeProvider{}
	return &wakeEnv{t: t, clock: clk, providers: map[string]*fakeProvider{"mini1": a, "mini2": b, "other": c3}, rec: &WakeReconciler{
		Client: c, Recorder: events.NewFakeRecorder(50), Clock: clk, Cooldown: 5 * time.Minute,
		Hosts: []config.Host{
			{Name: "mini1", Labels: map[string]string{"workload": "plex"}},
			{Name: "mini2", Labels: map[string]string{"workload": "plex"}},
			{Name: "other", Labels: map[string]string{"workload": "nas"}},
		},
		Providers: map[string]power.Provider{"mini1": a, "mini2": b, "other": c3},
	}}
}

func (e *wakeEnv) reconcile() (ctrl.Result, error) {
	return e.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "plex", Namespace: "default"}})
}

func (e *wakeEnv) wakes(host string) int { return e.providers[host].powerOns }

func TestWakesMatchingHostOnly(t *testing.T) {
	e := newWakeEnv(t, pendingPod())
	res, err := e.reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 1 || e.wakes("mini2") != 0 || e.wakes("other") != 0 {
		t.Fatalf("wakes = %d %d %d", e.wakes("mini1"), e.wakes("mini2"), e.wakes("other"))
	}
	if res.RequeueAfter == 0 {
		t.Fatal("pending pod must be rechecked")
	}
}

func TestWakesFromRequiredNodeAffinity(t *testing.T) {
	e := newWakeEnv(t, pendingPod(func(p *corev1.Pod) {
		p.Spec.NodeSelector = nil
		p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "workload", Operator: corev1.NodeSelectorOpIn, Values: []string{"nas"}}},
			}}},
		}}
	}))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("other") != 1 || e.wakes("mini1") != 0 {
		t.Fatalf("wakes other=%d mini1=%d", e.wakes("other"), e.wakes("mini1"))
	}
}

func TestPinnedHostnameSelectsThatHost(t *testing.T) {
	e := newWakeEnv(t, pendingPod(func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "mini2"}
	}))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini2") != 1 || e.wakes("mini1") != 0 {
		t.Fatalf("wakes mini2=%d mini1=%d", e.wakes("mini2"), e.wakes("mini1"))
	}
}

func TestDoesNotWakeFor(t *testing.T) {
	for name, pod := range map[string]*corev1.Pod{
		"no selector":                 pendingPod(func(p *corev1.Pod) { p.Spec.NodeSelector = nil }),
		"no match":                    pendingPod(func(p *corev1.Pod) { p.Spec.NodeSelector = map[string]string{"workload": "x"} }),
		"bound":                       pendingPod(func(p *corev1.Pod) { p.Spec.NodeName = "mini1" }),
		"running":                     pendingPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }),
		"not yet judged by scheduler": pendingPod(func(p *corev1.Pod) { p.Status.Conditions = nil }),
		"daemonset": pendingPod(func(p *corev1.Pod) {
			yes := true
			p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "d", UID: "1", Controller: &yes}}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, pod)
			if _, err := e.reconcile(); err != nil {
				t.Fatal(err)
			}
			for host := range e.providers {
				if e.wakes(host) != 0 {
					t.Fatalf("woke %s", host)
				}
			}
		})
	}
}

func TestLiveMatchingNodeSuppressesWake(t *testing.T) {
	e := newWakeEnv(t, pendingPod(), managedNode("mini1"))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 0 || e.wakes("mini2") != 0 {
		t.Fatalf("wakes %d %d", e.wakes("mini1"), e.wakes("mini2"))
	}
}

func TestNotReadyNodeIsWakeable(t *testing.T) {
	e := newWakeEnv(t, pendingPod(func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "mini1"}
	}), managedNode("mini1", notReadyFor(time.Hour)))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 1 {
		t.Fatalf("wakes = %d", e.wakes("mini1"))
	}
}

func TestReadyCandidateSuppressesWakingNotReadyOne(t *testing.T) {
	e := newWakeEnv(t, pendingPod(), managedNode("mini1", notReadyFor(time.Hour)), managedNode("mini2"))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 0 || e.wakes("mini2") != 0 {
		t.Fatalf("wakes %d %d", e.wakes("mini1"), e.wakes("mini2"))
	}
}

// power-state=off is still pending, waking would only have it shut down again.
func TestNotReadyHostWithPendingPowerOffIsNotWoken(t *testing.T) {
	e := newWakeEnv(t, pendingPod(func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": "mini1"}
	}), managedNode("mini1", notReadyFor(time.Hour), annotated(nodes.AnnotationPowerState, nodes.PowerStateOff)))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 0 {
		t.Fatalf("wakes = %d", e.wakes("mini1"))
	}
}

func TestHostShuttingDownIsNotWokenButOtherIs(t *testing.T) {
	e := newWakeEnv(t, pendingPod(), managedNode("mini1", annotated(nodes.AnnotationPowerState, nodes.PowerStateOff)))
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini1") != 0 || e.wakes("mini2") != 1 {
		t.Fatalf("wakes %d %d", e.wakes("mini1"), e.wakes("mini2"))
	}
}

func TestCooldownSuppressesRepeatWake(t *testing.T) {
	e := newWakeEnv(t, pendingPod())
	for range 3 {
		if _, err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	if e.wakes("mini1") != 1 || e.wakes("mini2") != 0 {
		t.Fatalf("wakes %d %d", e.wakes("mini1"), e.wakes("mini2"))
	}
}

func TestAfterCooldownTriesTheOtherCandidate(t *testing.T) {
	e := newWakeEnv(t, pendingPod())
	_, _ = e.reconcile()
	e.clock.Step(5 * time.Minute)
	_, _ = e.reconcile()
	if e.wakes("mini1") != 1 || e.wakes("mini2") != 1 {
		t.Fatalf("wakes %d %d", e.wakes("mini1"), e.wakes("mini2"))
	}
}

func TestFailedWakeFallsThroughWithoutCooldown(t *testing.T) {
	e := newWakeEnv(t, pendingPod())
	e.providers["mini1"].onErr = errors.New("router down")
	if _, err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if e.wakes("mini2") != 1 {
		t.Fatalf("mini2 wakes = %d", e.wakes("mini2"))
	}
}

func TestAllWakesFailingReturnsError(t *testing.T) {
	e := newWakeEnv(t, pendingPod())
	e.providers["mini1"].onErr = errors.New("router down")
	e.providers["mini2"].onErr = errors.New("router down")
	if _, err := e.reconcile(); err == nil {
		t.Fatal("want error so the request is retried")
	}
}
