// Package agent powers off the host it runs on when the controller asks for it.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
)

const commandTimeout = 30 * time.Second

// Reconciler watches only its own Node, so the agent needs no network listener.
type Reconciler struct {
	client.Client
	PowerOff func(context.Context) error

	// handled is the last acted on request. A controller retry rewrites the
	// annotation with a new timestamp, which fires again.
	handled string
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	request, requested := node.Annotations[nodes.AnnotationShutdownRequested]
	if !requested || !nodes.Managed(&node) || !nodes.PowerOffRequested(&node) || request == r.handled {
		return ctrl.Result{}, nil
	}
	log.FromContext(ctx).Info("powering off host", "request", request)
	if err := r.PowerOff(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("powering off: %w", err)
	}
	r.handled = request
	return ctrl.Result{}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("agent").For(&corev1.Node{}).Complete(r)
}

// HostShutdown runs the host's own shutdown command from inside its namespaces,
// so kubelet graceful shutdown and everything else on the host see a regular shutdown.
var HostShutdown = Command{Argv: []string{"/nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "shutdown", "-h", "now"}}

// Command powers off by running a program.
type Command struct {
	Argv []string
}

func (c Command) PowerOff(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", c.Argv, err, bytes.TrimSpace(out))
	}
	return nil
}
