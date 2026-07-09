// Command harmostes-controller is the monitor controller: an always-on
// controller-runtime manager that watches Workflow CRs and schedules worker Jobs
// (prepare→agent→deploy) for due runs. It owns scheduling + observedGeneration;
// the worker owns the run outcome.
//
// Flags / env configure the worker image to spawn, the poll interval, and the
// in-cluster identity (service account + image pull secret).
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tibrezus/harmostes/internal/controller"
	"github.com/tibrezus/harmostes/internal/k8s"
)

func main() {
	var (
		metricsAddr          string
		namespace            string
		workerImage          string
		workerImagePullSecs  string
		serviceAccount       string
		pollInterval         time.Duration
		daprEnabled          bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics server bind address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "namespace the controller watches + creates worker Jobs in")
	flag.StringVar(&workerImage, "worker-image", envOr("HARMOSTES_WORKER_IMAGE", "ghcr.io/tibrezus/harmostes-worker:dev"), "worker image the controller spawns")
	flag.StringVar(&workerImagePullSecs, "worker-image-pull-secret", envOr("HARMOSTES_WORKER_IMAGE_PULL_SECRET", ""), "image pull secret for the worker")
	flag.StringVar(&serviceAccount, "service-account", "harmostes-controller", "service account for worker Jobs")
	flag.DurationVar(&pollInterval, "poll-interval", envDurationOr("HARMOSTES_POLL_INTERVAL", 5*time.Minute), "default run cadence for Workflows without a schedule")
	flag.BoolVar(&daprEnabled, "dapr-enabled", false, "inject the Dapr sidecar into worker Jobs (requires the namespace/SA trusted by the Dapr sentry)")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Cache:                 cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Metrics:               metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: ":8081",
	})
	if err != nil {
		setupLog("manager", err)
		os.Exit(1)
	}

	if err := (&controller.WorkflowReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		WorkerImage:         workerImage,
		WorkerImagePullSecs: workerImagePullSecs,
		ServiceAccountName:  serviceAccount,
		PollInterval:        pollInterval,
		JobNamespace:        namespace,
		DaprEnabled:         daprEnabled,
	}).SetupWithManager(mgr); err != nil {
		setupLog("controller setup", err)
		os.Exit(1)
	}

	setupLogMsg("starting harmostes monitor controller (worker-image=%s poll=%s)", workerImage, pollInterval)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog("manager exited", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDurationOr is reserved for env-driven config; the flag default suffices today.
func envDurationOr(_ string, def time.Duration) time.Duration { return def }

func setupLog(msg string, err error) {
	ctrl.Log.WithName("setup").Error(err, msg)
}

func setupLogMsg(format string, args ...any) {
	ctrl.Log.WithName("setup").Info(fmt.Sprintf(format, args...))
}
