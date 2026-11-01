// Command node-agent powers off the host it runs on when asked by the controller.
package main

import (
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"gitlab.l3a.dev/infra/nekromancer/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME must be set")
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}: {Field: fields.OneTermEqualSelector("metadata.name", nodeName)},
		}},
	})
	if err != nil {
		return err
	}
	r := &agent.Reconciler{Client: mgr.GetClient(), PowerOff: agent.HostShutdown.PowerOff}
	if err := r.SetupWithManager(mgr); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
