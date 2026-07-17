// Command harmostes-controller is the monitor controller: an always-on
// controller-runtime manager that watches Workflow CRs and schedules worker Jobs
// (prepare→agent→deploy) for due runs. It owns scheduling + observedGeneration;
// the worker owns the run outcome.
//
// Flags / env configure the worker image to spawn, the poll interval, and the
// in-cluster identity (service account + image pull secret).
package main

import (
	"context"
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
	"github.com/tibrezus/harmostes/internal/observability"
)

// version is the controller image version (set via -ldflags at build; "dev" locally).
var version = "dev"

func main() {
	var (
		metricsAddr         string
		namespace           string
		workerImage         string
		workerImagePullSecs string
		serviceAccount      string
		pollInterval        time.Duration
		daprEnabled         bool
		daprdImage          string
		otlpEndpoint        string
		otlpInsecure        bool
		skillsRepo          string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics server bind address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "namespace the controller watches + creates worker Jobs in")
	flag.StringVar(&workerImage, "worker-image", envOr("HARMOSTES_WORKER_IMAGE", "ghcr.io/tibrezus/harmostes-worker:dev"), "worker image the controller spawns")
	flag.StringVar(&workerImagePullSecs, "worker-image-pull-secret", envOr("HARMOSTES_WORKER_IMAGE_PULL_SECRET", ""), "image pull secret for the worker")
	flag.StringVar(&serviceAccount, "service-account", "harmostes-controller", "service account for worker Jobs")
	flag.DurationVar(&pollInterval, "poll-interval", envDurationOr("HARMOSTES_POLL_INTERVAL", 5*time.Minute), "default run cadence for Workflows without a schedule")
	flag.BoolVar(&daprEnabled, "dapr-enabled", false, "inject the Dapr sidecar into worker Jobs (requires the namespace/SA trusted by the Dapr sentry)")
	flag.StringVar(&daprdImage, "daprd-image", envOr("HARMOSTES_DAPRD_IMAGE", ""), "forked daprd sidecar image for worker Jobs (empty = stock daprd, no OTLP push)")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", envOr("HARMOSTES_OTLP_ENDPOINT", ""), "OTLP collector endpoint stamped on worker Jobs as OTEL_EXPORTER_OTLP_ENDPOINT (enables worker pipeline spans; empty = disabled)")
	flag.StringVar(&skillsRepo, "skills-repo", envOr("HARMOSTES_SKILLS_REPO", "https://github.com/tibrezus/agents.git"), "git URL cloned into /skills by the worker init container")
	flag.BoolVar(&otlpInsecure, "otlp-insecure", false, "set OTEL_EXPORTER_OTLP_INSECURE on worker sidecars (plain gRPC for cluster-internal collectors)")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Observability: OTLP tracer/meter providers (disabled when
	// OTEL_EXPORTER_OTLP_ENDPOINT is unset). Flushed on graceful shutdown (the
	// controller is long-running; boot-error exits skip it, which is fine — no
	// telemetry is emitted before the manager starts).
	obsShutdown, obsErr := observability.Init(context.Background(), observability.Config{
		Component:    "harmostes-controller",
		Version:      version,
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Insecure:     os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
		PodName:      os.Getenv("POD_NAME"),
		PodNamespace: namespace,
	})
	if obsErr != nil {
		setupLog("observability init (telemetry disabled)", obsErr)
	}
	defer func() {
		if obsShutdown != nil {
			_ = observability.ShutdownWithTimeout(context.Background(), obsShutdown, observability.ShutdownTimeout)
		}
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
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
		DaprdImage:          daprdImage,
		OTLPEndpoint:        otlpEndpoint,
		OTLPInsecure:        otlpInsecure,
		SkillsRepo:          skillsRepo,
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
