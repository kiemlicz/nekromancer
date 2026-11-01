package agent

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
)

func node(labels, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mini1", Labels: labels, Annotations: annotations}}
}

var (
	managed   = map[string]string{nodes.LabelManaged: "true"}
	requested = map[string]string{nodes.AnnotationPowerState: nodes.PowerStateOff, nodes.AnnotationShutdownRequested: "t1"}
)

func run(t *testing.T, n *corev1.Node, powerOff func(context.Context) error) (*Reconciler, error) {
	t.Helper()
	r := &Reconciler{Client: fake.NewClientBuilder().WithObjects(n).Build(), PowerOff: powerOff}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mini1"}})
	return r, err
}

func TestPowersOffWhenRequested(t *testing.T) {
	calls := 0
	if _, err := run(t, node(managed, requested), func(context.Context) error { calls++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestDoesNotPowerOff(t *testing.T) {
	controlPlane := map[string]string{nodes.LabelManaged: "true", nodes.LabelControlPlane: ""}
	for name, n := range map[string]*corev1.Node{
		"no annotations":      node(managed, nil),
		"drain not finished":  node(managed, map[string]string{nodes.AnnotationPowerState: nodes.PowerStateOff}),
		"power-state cleared": node(managed, map[string]string{nodes.AnnotationShutdownRequested: "t1"}),
		"unmanaged":           node(nil, requested),
		"control plane":       node(controlPlane, requested),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := run(t, n, func(context.Context) error { t.Error("powered off"); return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSameRequestActsOnce(t *testing.T) {
	calls := 0
	r, err := run(t, node(managed, requested), func(context.Context) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mini1"}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestFailureIsRetried(t *testing.T) {
	calls := 0
	r, err := run(t, node(managed, requested), func(context.Context) error { calls++; return errors.New("shutdown failed") })
	if err == nil {
		t.Fatal("want error")
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "mini1"}})
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}
