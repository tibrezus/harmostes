// Package ui provides the harmostes-ui HTTP server for the redesigned UI.
// This server uses Go + HTMX + Templ + Dapr, replacing the React SPA.
package ui

import (
	"context"
	"log/slog"
	"net/http"
)

// HTMXServer is the redesigned harmostes-ui HTTP server using HTMX + Templ.
// This is the Phase 5 skeleton that will be extended in subsequent phases.
type HTMXServer struct {
	k8sClient  K8sClient
	daprClient DaprClient
	logger     *slog.Logger
}

// NewHTMXServer creates a new HTMX server with the given clients.
func NewHTMXServer(k8sClient K8sClient, daprClient DaprClient, logger *slog.Logger) *HTMXServer {
	return &HTMXServer{
		k8sClient:  k8sClient,
		daprClient: daprClient,
		logger:     logger,
	}
}

// Routes returns the HTTP handler with all routes registered.
func (s *HTMXServer) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health check (no auth — kubelet probes)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Authenticated routes (wrapped in auth middleware)
	mux.Handle("GET /", withAuth(s.handleIndex, s.logger))
	mux.Handle("GET /workflows", withAuth(s.handleWorkflows, s.logger))
	mux.Handle("GET /workflows/{name}", withAuth(s.handleWorkflowDetail, s.logger))

	return mux
}

// handleHealthz is an unauthenticated health check endpoint.
func (s *HTMXServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleIndex renders the home page.
func (s *HTMXServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Home page - Phase 5"))
}

// handleWorkflows renders the workflow list page.
func (s *HTMXServer) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("Workflow list - Phase 5"))
}

// handleWorkflowDetail renders a workflow detail page.
func (s *HTMXServer) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w.Write([]byte("Workflow detail: " + name + " - Phase 5"))
}

// withAuth wraps a handler with Authentik forward-auth middleware.
// It extracts identity from Authentik headers and adds it to the request context.
func withAuth(next http.HandlerFunc, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := extractIdentity(r)

		if identity == nil {
			logger.Warn("unauthenticated request", "path", r.URL.Path)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		logger.Debug("authenticated request", "user", identity.Username, "path", r.URL.Path)

		// Add identity to context for downstream handlers
		ctx := context.WithValue(r.Context(), identityKey, identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}