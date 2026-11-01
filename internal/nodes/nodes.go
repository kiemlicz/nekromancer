// Package nodes defines the Node contract shared by the controller and node-agent.
package nodes

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	domain = "nekromancer.kiemlicz.github.io/"

	LabelManaged      = domain + "managed"
	LabelIdleTimeout  = domain + "idle-timeout"
	LabelControlPlane = "node-role.kubernetes.io/control-plane"

	AnnotationPowerState = domain + "power-state"
	AnnotationIdleSince  = domain + "idle-since"
	// AnnotationShutdownRequested is the signal to the node-agent, written once the drain is finished.
	AnnotationShutdownRequested = domain + "shutdown-requested"

	PowerStateOff = "off"
)

// Managed is the single gate for acting on a Node. Control plane always loses, even if mislabeled.
func Managed(n *corev1.Node) bool {
	if _, cp := n.Labels[LabelControlPlane]; cp {
		return false
	}
	return n.Labels[LabelManaged] == "true"
}

func PowerOffRequested(n *corev1.Node) bool {
	return n.Annotations[AnnotationPowerState] == PowerStateOff
}

// IdleTimeout returns the per-node override. An invalid one yields def and an error.
func IdleTimeout(n *corev1.Node, def time.Duration) (time.Duration, error) {
	raw, ok := n.Labels[LabelIdleTimeout]
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("invalid %s %q: %w", LabelIdleTimeout, raw, err)
	}
	if d <= 0 {
		return def, fmt.Errorf("invalid %s %q: must be positive", LabelIdleTimeout, raw)
	}
	return d, nil
}

// Ready reports the Ready condition and when it last changed.
func Ready(n *corev1.Node) (bool, time.Time) {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue, c.LastTransitionTime.Time
		}
	}
	return false, n.CreationTimestamp.Time
}

func IsDaemonSetPod(p *corev1.Pod) bool {
	owner := metav1.GetControllerOf(p)
	return owner != nil && owner.Kind == "DaemonSet"
}

// BlocksIdle reports whether a pod counts as workload: it keeps the node awake and must be drained.
// Static pods are treated like DaemonSets, neither can be evicted.
func BlocksIdle(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, static := p.Annotations[corev1.MirrorPodAnnotationKey]; static {
		return false
	}
	return !IsDaemonSetPod(p)
}
