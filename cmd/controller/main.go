// Command controller reaps idle managed nodes and wakes them for pending pods.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"gitlab.l3a.dev/infra/nekromancer/internal/config"
	"gitlab.l3a.dev/infra/nekromancer/internal/controller"
	"gitlab.l3a.dev/infra/nekromancer/internal/power"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/nekromancer/config.yaml", "path to the inventory")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "metrics endpoint")
	probeAddr := flag.String("health-probe-bind-address", ":8081", "health probe endpoint")
	leaderElect := flag.Bool("leader-elect", true, "enable leader election")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	providers, err := power.Build(cfg, power.Credentials{
		MikrotikUsername: os.Getenv("MIKROTIK_USERNAME"),
		MikrotikPassword: os.Getenv("MIKROTIK_PASSWORD"),
		PiKVMUsername:    os.Getenv("PIKVM_USERNAME"),
		PiKVMPassword:    os.Getenv("PIKVM_PASSWORD"),
	})
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "nekromancer.kiemlicz.github.io",
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
	})
	if err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()
	recorder := mgr.GetEventRecorder("nekromancer")
	if err := (&controller.NodeReconciler{
		Client: mgr.GetClient(), Recorder: recorder, Clock: clock.RealClock{},
		Providers:          providers,
		DefaultIdleTimeout: cfg.DefaultIdleTimeout.Duration,
		PowerOffRetry:      cfg.PowerOffRetry.Duration,
	}).SetupWithManager(ctx, mgr); err != nil {
		return err
	}
	if err := (&controller.WakeReconciler{
		Client: mgr.GetClient(), Recorder: recorder, Clock: clock.RealClock{},
		Cooldown: cfg.WakeCooldown.Duration, Hosts: cfg.Hosts, Providers: providers,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
