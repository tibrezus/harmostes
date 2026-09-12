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
	"github.com/tibrezus/harmostes/internal/timeline"
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
		chartDir        string
	)
	flag.StringVar(&addr, "addr", envOr("HARMOSTES_UI_ADDR", ":8083"), "HTTP listen address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "k8s namespace to query")
	flag.StringVar(&platformsConfig, "platforms-config", envOr("HARMOSTES_PLATFORMS_CONFIG_FILE", ""), "path to JSON platform display config file")
	flag.BoolVar(&fixtureMode, "fixture", false, "serve the deterministic in-memory fixture world instead of a cluster")
	flag.StringVar(&chartDir, "chart", "chart", "chart directory the fixture world loads its CRDs and pr-review template from (fixture mode only)")
	flag.Parse()

	// Loopback default for the fixture server (#436): the fixture world is
	// write-capable and unauthenticated by construction (DevIdentity), so
	// binding it to all interfaces by accident is the one wrong default.
	// An explicit -addr always wins; the e2e harness passes its own.
	addr = fixtureListenAddr(fixtureMode, addr)

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
		fixtureServer, err := fixture.NewWorld(namespace, logger, chartDir)
		if err != nil {
			logger.Error("seed fixture world", "err", err)
			os.Exit(1)
		}
		serve(logger, addr, fixtureServer.Routes(), namespace, true)
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
	// Authentik groups whose members see across all owner labels (see
	// Server.SetAdminGroups — owner labels have churned before; without the
	// bypass one mismatch bricks every page for the operator).
	// Template MR bridge (#420, ADR-0012 §5): where this environment's
	// template values live. Unset = the propose surface is absent, never an
	// error. The token rides separately (sourceTokenEnv) from an
	// ExternalSecret — server-side only, never rendered.
	ts, err := ui.ParseTemplateSource(os.Getenv("HARMOSTES_TEMPLATE_SOURCE"))
	if err != nil {
		logger.Error("template source", "err", err)
		os.Exit(1)
	}
	server.SetTemplateSource(ts)
	server.SetSourceToken(os.Getenv(ui.SourceTokenEnv))

	if groups := ui.ParseAdminGroups(os.Getenv("HARMOSTES_UI_ADMIN_GROUPS")); len(groups) > 0 {
		server.SetAdminGroups(groups)
		logger.Info("admin groups configured", "groups", groups)
	}

	// Dev-identity writes (X-Harmostes-Dev-User): OFF unless explicitly
	// enabled. Production chart values never set this — the invariant lives
	// here, not in an assumption about network reachability (PR #427 review).
	if envOr("HARMOSTES_UI_DEV_WRITE", "") == "true" {
		server.SetDevWriteEnabled(true)
		logger.Warn("dev-identity writes ENABLED — never set in production")
	}

	// Wire the Dapr client for reading session transcripts from the worker's
	// state store. resolveDaprEndpoint prefers the explicit DAPR_HTTP_ENDPOINT
	// and falls back to the injector's DAPR_HTTP_PORT (127.0.0.1 — Go resolves
	// localhost to ::1 while the sidecar binds v4 only).
	daprEndpoint := resolveDaprEndpoint()
	daprCli := dapr.New(daprEndpoint)
	server.SetDaprClient(ui.NewDaprClient(daprCli))
	// Event Timeline (ADR-0012 §4): the UI's first reader of the worker's
	// timeline store. Same store the transcript reader uses.
	server.SetTimelineReader(timeline.NewReader(daprCli, ui.TimelineStateStore()))
	logger.Info("Dapr client wired for transcripts, usage + timeline", "endpoint", daprEndpoint)

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
		_ = httpServer.Shutdown(context.Background())
	}()

	logger.Info("starting harmostes-ui", "addr", addr, "namespace", namespace, "fixture", fixtureMode)
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

// fixtureListenAddr narrows the fixture default to loopback (#436): the
// fixture world is write-capable and unauthenticated by construction, so
// `:8083` on all interfaces is the wrong default for it. Only the DEFAULT
// moves — an explicit -addr / HARMOSTES_UI_ADDR is respected verbatim, and
// non-fixture servers keep the wide default (production sits behind the
// outpost and needs the Service's cluster IP to reach it).
func fixtureListenAddr(fixtureMode bool, addr string) string {
	if fixtureMode && addr == ":8083" {
		return "127.0.0.1:8083"
	}
	return addr
}
