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

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

var t0 = time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)

type fakeProvider struct {
	offline             bool
	offlineErr, onErr   error
	powerOffs, powerOns int
}

func (f *fakeProvider) PowerOn(context.Context) error  { f.powerOns++; return f.onErr }
func (f *fakeProvider) PowerOff(context.Context) error { f.powerOffs++; return nil }
func (f *fakeProvider) Offline(context.Context) (bool, error) {
	return f.offline, f.offlineErr
}

var errProbe = errors.New("probe failed")

func managedNode(name string, mut ...func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{nodes.LabelManaged: "true"},
			CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour)),
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(t0.Add(-time.Hour)),
		}}},
	}
	for _, m := range mut {
		m(n)
	}
	return n
}

func annotated(k, v string) func(*corev1.Node) {
	return func(n *corev1.Node) {
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[k] = v
	}
}

func notReadyFor(d time.Duration) func(*corev1.Node) {
	return func(n *corev1.Node) {
		n.Status.Conditions[0].Status = corev1.ConditionUnknown
		n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(t0.Add(-d))
	}
}

func workload(name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func daemon(name, nodeName string) *corev1.Pod {
	p := workload(name, nodeName)
	yes := true
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "ds", UID: "1", Controller: &yes}}
	return p
}

type env struct {
	t        *testing.T
	client   client.Client
	clock    *clocktesting.FakeClock
	provider *fakeProvider
	rec      *NodeReconciler
	events   *events.FakeRecorder
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	c := fake.NewClientBuilder().WithObjects(objs...).
		WithStatusSubresource(&corev1.Node{}, &corev1.Pod{}).
		WithIndex(&corev1.Pod{}, PodNodeNameIndex, podNodeName).
		Build()
	clk := clocktesting.NewFakeClock(t0)
	p := &fakeProvider{}
	rec := events.NewFakeRecorder(50)
	return &env{t: t, client: c, clock: clk, provider: p, events: rec, rec: &NodeReconciler{
		Client: c, Recorder: rec, Clock: clk,
		Providers:          map[string]power.Provider{"mini1": p},
		DefaultIdleTimeout: 10 * time.Minute,
		PowerOffRetry:      2 * time.Minute,
	}}
}

func (e *env) reconcile() ctrl.Result {
	e.t.Helper()
	res, err := e.rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mini1"}})
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

// node returns nil once the Node is gone.
func (e *env) node() *corev1.Node {
	e.t.Helper()
	var n corev1.Node
	err := e.client.Get(context.Background(), types.NamespacedName{Name: "mini1"}, &n)
	if client.IgnoreNotFound(err) != nil {
		e.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return &n
}

func (e *env) podExists(name string) bool {
	e.t.Helper()
	var p corev1.Pod
	err := e.client.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &p)
	if client.IgnoreNotFound(err) != nil {
		e.t.Fatal(err)
	}
	return err == nil
}
