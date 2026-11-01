package nodes

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
}

func TestManaged(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		want   bool
	}{
		"labelled":      {map[string]string{LabelManaged: "true"}, true},
		"no label":      {nil, false},
		"label false":   {map[string]string{LabelManaged: "false"}, false},
		"control plane": {map[string]string{LabelManaged: "true", LabelControlPlane: ""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Managed(node(tc.labels)); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestIdleTimeout(t *testing.T) {
	def := 10 * time.Minute
	if got, err := IdleTimeout(node(nil), def); got != def || err != nil {
		t.Errorf("default: %v %v", got, err)
	}
	if got, err := IdleTimeout(node(map[string]string{LabelIdleTimeout: "30m"}), def); got != 30*time.Minute || err != nil {
		t.Errorf("override: %v %v", got, err)
	}
	for _, bad := range []string{"soon", "-5m", "0s"} {
		if got, err := IdleTimeout(node(map[string]string{LabelIdleTimeout: bad}), def); got != def || err == nil {
			t.Errorf("%q: %v %v", bad, got, err)
		}
	}
}

func TestReady(t *testing.T) {
	at := metav1.NewTime(time.Unix(1000, 0))
	created := metav1.NewTime(time.Unix(500, 0))
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}

	if ok, since := Ready(n); ok || !since.Equal(created.Time) {
		t.Errorf("no condition: %v %v", ok, since)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, LastTransitionTime: at}}
	if ok, since := Ready(n); ok || !since.Equal(at.Time) {
		t.Errorf("unknown: %v %v", ok, since)
	}
	n.Status.Conditions[0].Status = corev1.ConditionTrue
	if ok, _ := Ready(n); !ok {
		t.Error("true: not ready")
	}
}

func TestBlocksIdle(t *testing.T) {
	yes := true
	pod := func(mut func(*corev1.Pod)) *corev1.Pod {
		p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if mut != nil {
			mut(p)
		}
		return p
	}
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want bool
	}{
		"running":   {pod(nil), true},
		"pending":   {pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), true},
		"succeeded": {pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), false},
		"failed":    {pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), false},
		"daemonset": {pod(func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Controller: &yes}}
		}), false},
		"static": {pod(func(p *corev1.Pod) {
			p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
		}), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BlocksIdle(tc.pod); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}
