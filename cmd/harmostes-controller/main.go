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
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/tibrezus/harmostes/internal/controller"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/webhook"
	"github.com/tibrezus/harmostes/version"
)

func main() {
	var (
		metricsAddr  string
		namespace    string
		pollInterval time.Duration
		otlpEndpoint string
		otlpInsecure bool
		webhookAddr  string
		triggerTopic string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics server bind address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "namespace the controller watches")
	flag.DurationVar(&pollInterval, "poll-interval", envDurationOr("HARMOSTES_POLL_INTERVAL", 5*time.Minute), "default run cadence for Workflows without a schedule")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", envOr("HARMOSTES_OTLP_ENDPOINT", ""), "OTLP collector endpoint stamped on trigger events")
	flag.BoolVar(&otlpInsecure, "otlp-insecure", false, "set OTEL_EXPORTER_OTLP_INSECURE on workers (plain gRPC for cluster-internal collectors)")
	flag.StringVar(&webhookAddr, "webhook-bind-address", envOr("HARMOSTES_WEBHOOK_ADDRESS", ":8082"), "webhook server bind address (for git push events)")
	flag.StringVar(&triggerTopic, "trigger-topic", envOr("HARMOSTES_TRIGGER_TOPIC", "harmostes-triggers"), "Dapr pub/sub topic for trigger events")
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
		Version:      version.Version,
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

	// Dapr client for pub/sub trigger publishing. The controller's daprd
	// sidecar (chart injects it) exposes the Dapr HTTP API on localhost:3500.
	var daprClient dapr.Client
	daprEndpoint := envOr("DAPR_HTTP_ENDPOINT", "http://localhost:3500")
	if daprEndpoint != "" {
		daprClient = dapr.Tracing(dapr.New(daprEndpoint))
		setupLogMsg("dapr client wired for trigger publishing at %s", daprEndpoint)
	}

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

	// Setup webhook server (if address is configured)
	if webhookAddr != "" {
		host, _, err := net.SplitHostPort(webhookAddr)
		if err != nil {
			setupLog("invalid webhook address", err)
			os.Exit(1)
		}

		// Create a direct (non-cached) client for webhook secret resolution.
		// The manager's cached client uses an informer that needs list+watch on
		// all secrets in the namespace — a broader permission than necessary.
		// A direct client needs only `get` on secrets and avoids informer sync
		// delays on the webhook hot path.
		webhookClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
		if err != nil {
			setupLog("failed to create webhook client", err)
			os.Exit(1)
		}

		// Create webhook mux
		webhookMux := http.NewServeMux()
		webhookHandler := webhook.NewHandler(webhookClient, namespace, ctrl.Log.WithName("webhook"))

		// Register routes: /webhook/{workflow-name}
		webhookMux.HandleFunc("/webhook/", func(w http.ResponseWriter, req *http.Request) {
			// Extract workflow name from path
			workflowName := strings.TrimPrefix(req.URL.Path, "/webhook/")
			if workflowName == "" {
				http.Error(w, "workflow name required", http.StatusBadRequest)
				return
			}
			webhookHandler.ServeHTTP(w, req, workflowName)
		})

		// Start webhook server in background
		go func() {
			handler := &corsHandler{handler: webhookMux}
			setupLogMsg("webhook server listening on %s (host=%s)", webhookAddr, host)
			if err := http.ListenAndServe(webhookAddr, handler); err != nil {
				setupLog("webhook server exited", err)
			}
		}()
	}
	if err != nil {
		setupLog("manager", err)
		os.Exit(1)
	}

	if err := (&controller.WorkflowReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		PollInterval: pollInterval,
		OTLPEndpoint: otlpEndpoint,
		OTLPInsecure: otlpInsecure,
		DaprClient:   daprClient,
		TriggerTopic: triggerTopic,
	}).SetupWithManager(mgr); err != nil {
		setupLog("controller setup", err)
		os.Exit(1)
	}

	setupLogMsg("starting harmostes controller (poll=%s webhook=%s)", pollInterval, webhookAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog("manager exited", err)
		os.Exit(1)
	}
}

// corsHandler wraps an http.Handler with CORS headers.
type corsHandler struct {
	handler http.Handler
}

func (c *corsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Hub-Signature-256, X-Gitlab-Token, X-Forgejo-Signature")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	c.handler.ServeHTTP(w, r)
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
