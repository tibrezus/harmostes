// Command harmostes-ui is the self-service web interface for harmostes.
// It serves a multi-tenant dashboard (HTMX + Go templates, RezusCloud design
// system) where each user manages their own Workflow CRs and git tokens.
//
// Authentication is via Authentik forward-auth: the proxy provider injects
// identity headers (X-Authentik-Username, X-Authentik-Email, X-Authentik-Groups)
// on every authenticated request. The UI extracts the username and filters all
// k8s queries by the harmostes.dev/owner label.
//
// For local development without Authentik, set HARMOSTES_DEV_USER to bypass
// identity extraction.
//
// Flags:
//
//	--addr          HTTP listen address (default :8083)
//	--namespace     k8s namespace to query (default from HARMOSTES_NAMESPACE env)
//	--kubeconfig    path to kubeconfig (default: in-cluster config)
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/timeline"
	"github.com/tibrezus/harmostes/internal/ui"
	"github.com/tibrezus/harmostes/version"
)

func main() {
	var (
		addr            string
		namespace       string
		platformsConfig string
	)
	flag.StringVar(&addr, "addr", envOr("HARMOSTES_UI_ADDR", ":8083"), "HTTP listen address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "k8s namespace to query")
	flag.StringVar(&platformsConfig, "platforms-config", envOr("HARMOSTES_PLATFORMS_CONFIG_FILE", ""), "path to JSON platform display config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "harmostes-ui", "version", version.Version)

	// Load platform display configs (plug-and-play: any platform string is
	// accepted for tokens; this only enriches display metadata for known ones).
	platformConfigs := ui.LoadPlatformConfigs(platformsConfig)
	logger.Info("platform configs loaded", "count", len(platformConfigs))

	// k8s client — same scheme as controller/worker (v1alpha1 + core + batch).
	// Use a direct (non-cached) client: the UI is read-heavy but low-traffic.
	// A direct client avoids informer cache sync issues (the same lesson as
	// the webhook secret resolution fix — see PR #50).
	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		logger.Error("create k8s client", "err", err)
		os.Exit(1)
	}

	// kubernetes clientset for pod log streaming (Phase E: run detail).
	kubeClient, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
	if err != nil {
		logger.Error("create kubernetes clientset", "err", err)
		os.Exit(1)
	}

	server, err := ui.New(k8sClient, namespace, logger, kubeClient, platformConfigs)
	if err != nil {
		logger.Error("create ui server", "err", err)
		os.Exit(1)
	}

	// Wire the Dapr client for reading session transcripts from the worker's
	// state store. resolveDaprEndpoint prefers the explicit DAPR_HTTP_ENDPOINT
	// and falls back to the injector's DAPR_HTTP_PORT (127.0.0.1 — Go resolves
	// localhost to ::1 while the sidecar binds v4 only).
	store := os.Getenv("HARMOSTES_STATE_STORE")
	if store == "" {
		store = "statestore"
	}
	daprEndpoint := resolveDaprEndpoint()
	server.SetDaprClient(ui.NewDaprClient(dapr.New(daprEndpoint)))
	server.SetTimelineReader(timeline.NewReader(dapr.New(daprEndpoint), store))
	logger.Info("Dapr client wired for session viewer + timeline", "endpoint", daprEndpoint)

	// Wire the SigNoz client for querying agent metrics (token usage, durations,
	// cost). Optional — the metrics view shows "not configured" when nil.
	if signozURL := os.Getenv("SIGNOZ_URL"); signozURL != "" {
		signozKey := os.Getenv("SIGNOZ_API_KEY")
		server.SetSignozClient(ui.NewSignozClient(signozURL, signozKey))
		logger.Info("SigNoz client wired for metrics view", "url", signozURL)
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.Routes(),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		httpServer.Shutdown(context.Background())
	}()

	logger.Info("starting harmostes-ui", "addr", addr, "namespace", namespace)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveDaprEndpoint returns the Dapr sidecar HTTP endpoint: explicit
// DAPR_HTTP_ENDPOINT wins; else the injector-provided DAPR_HTTP_PORT on
// loopback; else the conventional default.
func resolveDaprEndpoint() string {
	if e := os.Getenv("DAPR_HTTP_ENDPOINT"); e != "" {
		return e
	}
	if p := os.Getenv("DAPR_HTTP_PORT"); p != "" {
		return "http://127.0.0.1:" + p
	}
	return "http://127.0.0.1:3500"
}
