package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
)

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// --- guardrails

func TestIgnoresUnmanagedNode(t *testing.T) {
	n := managedNode("mini1", annotated(nodes.AnnotationPowerState, nodes.PowerStateOff))
	delete(n.Labels, nodes.LabelManaged)
	e := newEnv(t, n)
	e.provider.offline = true
	e.reconcile()
	got := e.node()
	if got == nil || got.Spec.Unschedulable || e.provider.powerOffs != 0 {
		t.Fatalf("unmanaged node touched: %+v", got)
	}
}

func TestIgnoresMislabeledControlPlane(t *testing.T) {
	n := managedNode("mini1", annotated(nodes.AnnotationPowerState, nodes.PowerStateOff), notReadyFor(time.Hour))
	n.Labels[nodes.LabelControlPlane] = ""
	e := newEnv(t, n, workload("app", "mini1"))
	e.provider.offline = true
	e.reconcile()
	got := e.node()
	if got == nil || got.Spec.Unschedulable || e.provider.powerOffs != 0 || !e.podExists("app") {
		t.Fatalf("control plane touched: %+v", got)
	}
}

func TestLeavesNodeWithoutInventoryEntryAlone(t *testing.T) {
	n := managedNode("mini1", annotated(nodes.AnnotationPowerState, nodes.PowerStateOff))
	e := newEnv(t, n, workload("app", "mini1"))
	delete(e.rec.Providers, "mini1")
	e.reconcile()
	got := e.node()
	if got == nil || got.Spec.Unschedulable || !e.podExists("app") {
		t.Fatalf("node without inventory entry touched: %+v", got)
	}
}

// --- use case 1: idle shutdown

func TestStampsIdleSinceWhenOnlyDaemonSetsRemain(t *testing.T) {
	e := newEnv(t, managedNode("mini1"), daemon("ds", "mini1"))
	res := e.reconcile()
	if got := e.node().Annotations[nodes.AnnotationIdleSince]; got != stamp(t0) {
		t.Fatalf("idle-since = %q", got)
	}
	if res.RequeueAfter != 10*time.Minute {
		t.Fatalf("requeue = %v", res.RequeueAfter)
	}
}

func TestPodOnOtherNodeDoesNotCount(t *testing.T) {
	e := newEnv(t, managedNode("mini1"), workload("app", "mini2"))
	e.reconcile()
	if _, ok := e.node().Annotations[nodes.AnnotationIdleSince]; !ok {
		t.Fatal("not stamped")
	}
}

func TestCompletedPodDoesNotCount(t *testing.T) {
	p := workload("job", "mini1")
	p.Status.Phase = corev1.PodSucceeded
	e := newEnv(t, managedNode("mini1"), p)
	e.reconcile()
	if _, ok := e.node().Annotations[nodes.AnnotationIdleSince]; !ok {
		t.Fatal("not stamped")
	}
}

func TestClearsIdleSinceWhenWorkloadReturns(t *testing.T) {
	e := newEnv(t, managedNode("mini1", annotated(nodes.AnnotationIdleSince, stamp(t0.Add(-time.Hour)))), workload("app", "mini1"))
	e.reconcile()
	if got, ok := e.node().Annotations[nodes.AnnotationIdleSince]; ok {
		t.Fatalf("idle-since kept: %q", got)
	}
}

func TestKeepsWaitingBeforeIdleTimeout(t *testing.T) {
	e := newEnv(t, managedNode("mini1", annotated(nodes.AnnotationIdleSince, stamp(t0.Add(-4*time.Minute)))))
	res := e.reconcile()
	if res.RequeueAfter != 6*time.Minute {
		t.Fatalf("requeue = %v", res.RequeueAfter)
	}
	if _, ok := e.node().Annotations[nodes.AnnotationPowerState]; ok {
		t.Fatal("shut down early")
	}
}

func TestIdleTimeoutSetsPowerStateOff(t *testing.T) {
	e := newEnv(t, managedNode("mini1", annotated(nodes.AnnotationIdleSince, stamp(t0.Add(-10*time.Minute)))))
	e.reconcile()
	if got := e.node().Annotations[nodes.AnnotationPowerState]; got != nodes.PowerStateOff {
		t.Fatalf("power-state = %q", got)
	}
}

func TestPerNodeIdleTimeoutOverride(t *testing.T) {
	n := managedNode("mini1", annotated(nodes.AnnotationIdleSince, stamp(t0.Add(-time.Minute))))
	n.Labels[nodes.LabelIdleTimeout] = "30s"
	e := newEnv(t, n)
	e.reconcile()
	if got := e.node().Annotations[nodes.AnnotationPowerState]; got != nodes.PowerStateOff {
		t.Fatalf("power-state = %q", got)
	}
}

func TestCorruptIdleSinceRestartsTimer(t *testing.T) {
	e := newEnv(t, managedNode("mini1", annotated(nodes.AnnotationIdleSince, "yesterday")))
	e.reconcile()
	got := e.node().Annotations
	if got[nodes.AnnotationIdleSince] != stamp(t0) || got[nodes.AnnotationPowerState] != "" {
		t.Fatalf("annotations = %v", got)
	}
}

// --- use case 2: annotation-driven shutdown

func off() func(*corev1.Node) { return annotated(nodes.AnnotationPowerState, nodes.PowerStateOff) }

func TestPowerOffDrainsBeforeShutdown(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off()), workload("app", "mini1"), daemon("ds", "mini1"))
	res := e.reconcile()
	got := e.node()
	if !got.Spec.Unschedulable {
		t.Error("not cordoned")
	}
	if e.podExists("app") {
		t.Error("workload not evicted")
	}
	if !e.podExists("ds") {
		t.Error("daemonset pod evicted")
	}
	if res.RequeueAfter == 0 {
		t.Error("no requeue while draining")
	}
}

func TestPowerOffNotCalledWhilePodsRemain(t *testing.T) {
	p := workload("app", "mini1")
	now := metav1.NewTime(t0)
	p.DeletionTimestamp = &now
	p.Finalizers = []string{"test/hold"}
	e := newEnv(t, managedNode("mini1", off()), p)
	e.reconcile()
	if e.provider.powerOffs != 0 || e.node().Annotations[nodes.AnnotationShutdownRequested] != "" {
		t.Fatal("shutdown requested with a pod still terminating")
	}
}

func TestRequestsShutdownOnceDrained(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off()), daemon("ds", "mini1"))
	e.reconcile()
	if got := e.node().Annotations[nodes.AnnotationShutdownRequested]; got != stamp(t0) {
		t.Fatalf("shutdown-requested = %q", got)
	}
	if e.provider.powerOffs != 1 {
		t.Fatalf("powerOffs = %d", e.provider.powerOffs)
	}
	if e.node() == nil {
		t.Fatal("deleted before verifying the host is down")
	}
}

func TestDeletesNodeOnceConfirmedOffline(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off(), annotated(nodes.AnnotationShutdownRequested, stamp(t0))))
	e.provider.offline = true
	e.reconcile()
	if e.node() != nil {
		t.Fatal("node not deleted")
	}
}

func TestKeepsNodeWhileHostAnswers(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off(), annotated(nodes.AnnotationShutdownRequested, stamp(t0))))
	res := e.reconcile()
	if e.node() == nil {
		t.Fatal("deleted a live host")
	}
	if res.RequeueAfter == 0 {
		t.Fatal("no requeue")
	}
	if e.provider.powerOffs != 0 {
		t.Fatalf("re-requested too early: %d", e.provider.powerOffs)
	}
}

func TestProbeErrorNeverDeletes(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off(), annotated(nodes.AnnotationShutdownRequested, stamp(t0))))
	e.provider.offline = true
	e.provider.offlineErr = errProbe
	e.reconcile()
	if e.node() == nil {
		t.Fatal("deleted on probe error")
	}
}

func TestRetriesPowerOffAfterWindow(t *testing.T) {
	e := newEnv(t, managedNode("mini1", off(), annotated(nodes.AnnotationShutdownRequested, stamp(t0.Add(-2*time.Minute)))))
	e.reconcile()
	if e.provider.powerOffs != 1 {
		t.Fatalf("powerOffs = %d", e.provider.powerOffs)
	}
	if got := e.node().Annotations[nodes.AnnotationShutdownRequested]; got != stamp(t0) {
		t.Fatalf("shutdown-requested = %q", got)
	}
}

// --- use case 3: physical power button

func TestNotReadyWithinGraceWaits(t *testing.T) {
	e := newEnv(t, managedNode("mini1", notReadyFor(20*time.Second)))
	e.provider.offline = true
	res := e.reconcile()
	if e.node() == nil {
		t.Fatal("deleted inside grace period")
	}
	if res.RequeueAfter != 40*time.Second {
		t.Fatalf("requeue = %v", res.RequeueAfter)
	}
}

func TestNotReadyAndOfflineDeletesNode(t *testing.T) {
	e := newEnv(t, managedNode("mini1", notReadyFor(time.Minute)))
	e.provider.offline = true
	e.reconcile()
	if e.node() != nil {
		t.Fatal("node not deleted")
	}
}

func TestNotReadyButReachableKeepsNode(t *testing.T) {
	e := newEnv(t, managedNode("mini1", notReadyFor(time.Hour)))
	res := e.reconcile()
	if e.node() == nil || res.RequeueAfter == 0 {
		t.Fatalf("node=%v requeue=%v", e.node(), res.RequeueAfter)
	}
}

func TestNotReadyProbeErrorKeepsNode(t *testing.T) {
	e := newEnv(t, managedNode("mini1", notReadyFor(time.Hour)))
	e.provider.offline = true
	e.provider.offlineErr = errProbe
	e.reconcile()
	if e.node() == nil {
		t.Fatal("deleted on probe error")
	}
}

func TestNotReadyDoesNotStartIdleTimer(t *testing.T) {
	e := newEnv(t, managedNode("mini1", notReadyFor(10*time.Second)))
	e.reconcile()
	if _, ok := e.node().Annotations[nodes.AnnotationIdleSince]; ok {
		t.Fatal("idle timer started on a NotReady node")
	}
}

// A dead kubelet never finishes terminating pods, so waiting for the drain would hang forever.
func TestAnnotatedAndOfflineSkipsDrain(t *testing.T) {
	p := workload("app", "mini1")
	now := metav1.NewTime(t0)
	p.DeletionTimestamp = &now
	p.Finalizers = []string{"test/hold"}
	e := newEnv(t, managedNode("mini1", off(), notReadyFor(time.Second)), p)
	e.provider.offline = true
	e.reconcile()
	if e.node() != nil {
		t.Fatal("node not deleted")
	}
}
