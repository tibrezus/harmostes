// Command harmostes-ui is the self-service web interface for harmostes.
// It serves a multi-tenant dashboard (HTMX + Go templates, RezusCloud design
// system) where each user manages their own Workflow CRs and git tokens.
//
// Authentication is via Authentik forward-auth: the proxy provider injects
// identity headers (X-Authentik-Username, X-Authentik-Email, X-Authentik-Groups)
// on every authenticated request. The UI extracts the username and filters all
// k8s queries by the harmostes.dev/owner label.
//
// For local development without Authentik, run with -fixture (identity is
// injected as the fixture dev user) or send the X-Harmostes-Dev-User header.
//
// Flags:
//
//	--addr          HTTP listen address (default :8083)
//	--namespace     k8s namespace to query (default from HARMOSTES_NAMESPACE env)
//	--kubeconfig    path to kubeconfig (default: in-cluster config)
//	--fixture       serve a deterministic synthetic world (in-memory, no
//	                cluster) for local development and the E2E target;
//	                requests are identity-injected as the fixture dev user
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
	"github.com/tibrezus/harmostes/internal/ui"
	"github.com/tibrezus/harmostes/internal/ui/fixture"
	"github.com/tibrezus/harmostes/version"
)

func main() {
	var (
		addr            string
		namespace       string
		platformsConfig string
		fixtureMode     bool
	)
	flag.StringVar(&addr, "addr", envOr("HARMOSTES_UI_ADDR", ":8083"), "HTTP listen address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "k8s namespace to query")
	flag.StringVar(&platformsConfig, "platforms-config", envOr("HARMOSTES_PLATFORMS_CONFIG_FILE", ""), "path to JSON platform display config file")
	flag.BoolVar(&fixtureMode, "fixture", false, "serve the deterministic in-memory fixture world instead of a cluster")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "harmostes-ui", "version", version.Version)

	// Load platform display configs (plug-and-play: any platform string is
	// accepted for tokens; this only enriches display metadata for known ones).
	platformConfigs := ui.LoadPlatformConfigs(platformsConfig)
	logger.Info("platform configs loaded", "count", len(platformConfigs))

	// Fixture mode: in-memory synthetic world through the same construction
	// path production uses — the -fixture contract is that page behavior is
	// identical, only the data source differs.
	if fixtureMode {
		fixtureServer, err := fixture.NewServer(namespace, logger)
		if err != nil {
			logger.Error("seed fixture world", "err", err)
			os.Exit(1)
		}
		serve(logger, addr, withDevIdentity(fixtureServer.Routes(), fixture.DevUser), namespace, true)
		return
	}

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
	daprEndpoint := resolveDaprEndpoint()
	server.SetDaprClient(ui.NewDaprClient(dapr.New(daprEndpoint)))
	logger.Info("Dapr client wired for transcripts + usage", "endpoint", daprEndpoint)

	serve(logger, addr, server.Routes(), namespace, false)
}

// serve runs the HTTP server until the process is signalled to stop.
func serve(logger *slog.Logger, addr string, handler http.Handler, namespace string, fixtureMode bool) {
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		httpServer.Shutdown(context.Background())
	}()

	logger.Info("starting harmostes-ui", "addr", addr, "namespace", namespace, "fixture", fixtureMode)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server", "err", err)
		os.Exit(1)
	}
}

// withDevIdentity injects the development identity so `-fixture` works with
// zero external setup (no Authentik, no headers): every request without
// explicit identity headers is served as the fixture dev user.
func withDevIdentity(next http.Handler, devUser string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Authentik-Username") == "" && r.Header.Get("X-Harmostes-Dev-User") == "" {
			r.Header.Set("X-Harmostes-Dev-User", devUser)
		}
		next.ServeHTTP(w, r)
	})
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
