// Package agent powers off the host it runs on when the controller asks for it.
package agent

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"gitlab.l3a.dev/infra/nekromancer/internal/nodes"
)

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

// Logind requests a graceful shutdown over the system bus. Going through logind
// honours delay inhibitors, which is what kubelet graceful shutdown relies on.
type Logind struct {
	Address string
}

func (l Logind) PowerOff(ctx context.Context) error {
	conn, err := dbus.Connect(l.Address, dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", l.Address, err)
	}
	defer conn.Close()
	obj := conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	return obj.CallWithContext(ctx, "org.freedesktop.login1.Manager.PowerOff", 0, false).Err
}
