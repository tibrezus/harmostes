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
//	--rbac-policy   path to JSON node-type RBAC policy file (optional)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/rbac"
	"github.com/tibrezus/harmostes/internal/ui"
)

var version = "dev"

func main() {
	var (
		addr       string
		namespace  string
		rbacPolicy string
	)
	flag.StringVar(&addr, "addr", envOr("HARMOSTES_UI_ADDR", ":8083"), "HTTP listen address")
	flag.StringVar(&namespace, "namespace", envOr("HARMOSTES_NAMESPACE", "harmostes"), "k8s namespace to query")
	flag.StringVar(&rbacPolicy, "rbac-policy", envOr("HARMOSTES_RBAC_POLICY_FILE", ""), "path to JSON node-type RBAC policy file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "harmostes-ui", "version", version)

	// Resolve RBAC policy: explicit file > default policy (deployment nodes
	// restricted to ops/admin). If the file is empty/unreadable, fall back to
	// the default.
	var nodePolicy rbac.NodePolicy
	if rbacPolicy != "" {
		nodePolicy = loadPolicyFile(logger, rbacPolicy)
	} else {
		nodePolicy = rbac.DefaultNodePolicy()
	}
	if len(nodePolicy) > 0 {
		logger.Info("RBAC policy loaded", "restricted_types", policyTypes(nodePolicy))
	} else {
		logger.Info("RBAC policy empty — all node types unrestricted")
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

	server, err := ui.New(k8sClient, namespace, logger, kubeClient, nodePolicy)
	if err != nil {
		logger.Error("create ui server", "err", err)
		os.Exit(1)
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

// loadPolicyFile reads a JSON file mapping node types to allowed groups.
// Format: {"vela-app": ["ops", "admin"], "agent": ["*"]}
func loadPolicyFile(logger *slog.Logger, path string) rbac.NodePolicy {
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Error("read RBAC policy file, using default", "path", path, "err", err)
		return rbac.DefaultNodePolicy()
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		logger.Error("parse RBAC policy file, using default", "path", path, "err", err)
		return rbac.DefaultNodePolicy()
	}
	return rbac.ParsePolicy(raw)
}

func policyTypes(p rbac.NodePolicy) []string {
	types := make([]string, 0, len(p))
	for t := range p {
		types = append(types, t)
	}
	return types
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
