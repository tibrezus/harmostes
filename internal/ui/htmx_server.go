// Package ui provides the harmostes-ui HTTP server for the redesigned UI.
// This server uses Go + HTMX + Templ + Dapr, replacing the React SPA.
package ui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/ui/templ/pages"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HTMXServer is the redesigned harmostes-ui HTTP server using HTMX + Templ.
// This is the Phase 6 implementation that renders workflow lists using templ.
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

// handleIndex redirects to the workflow list.
func (s *HTMXServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Only match exact "/" — Go 1.22 mux matches subtree for "/".
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/workflows", http.StatusSeeOther)
}

// handleWorkflows renders all workflows owned by the current user,
// grouped by gate archetype using templ components.
func (s *HTMXServer) handleWorkflows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := identityFromContext(ctx).Username
	if owner == "" {
		s.logger.Warn("no identity in context", "path", r.URL.Path)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Fetch workflows from Kubernetes
	workflows, err := s.k8sClient.ListWorkflows(ctx, owner)
	if err != nil {
		s.logger.Error("list workflows", "owner", owner, "err", err)
		http.Error(w, "Failed to load workflows", http.StatusInternalServerError)
		return
	}

	// Group workflows by gate
	gateGroups := s.groupWorkflowsByGate(workflows)

	// Render the page using templ
	err = pages.WorkflowsPage(owner, gateGroups).Render(ctx, w)
	if err != nil {
		s.logger.Error("render workflows page", "owner", owner, "err", err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
	}
}

// gateGroupData holds data for templ rendering
type gateGroupData struct {
	gate      string
	label     string
	category  string
	workflows []workflowCardData
}

// groupWorkflowsByGate groups workflows by their gate archetype.
func (s *HTMXServer) groupWorkflowsByGate(workflows []v1alpha1.Workflow) []pages.GateGroup {
	groupMap := map[string]*gateGroupData{}

	for _, wf := range workflows {
		gateName := workflowGate(wf.Spec.Agent.Gate.Plugin.Name)

		// Get gate metadata
		var label, category string
		if arch := gateByName(gateName); arch != nil {
			label = arch.Label
			category = arch.Category
		} else {
			label = gateName
			category = "other"
		}

		// Create group if it doesn't exist
		if _, ok := groupMap[gateName]; !ok {
			groupMap[gateName] = &gateGroupData{
				gate:      gateName,
				label:     gateCategoryLabel(category) + " — " + label,
				category:  category,
				workflows: []workflowCardData{},
			}
		}

		// Add workflow to group
		card := workflowCardData{
			Name:      wf.Name,
			Gate:      gateName,
			Disabled:  wf.Spec.Disabled,
			LastRunAt: formatLastRun(wf.Status.LastRunAt),
		}
		groupMap[gateName].workflows = append(groupMap[gateName].workflows, card)
	}

	// Convert to sorted slice
	groups := make([]pages.GateGroup, 0, len(groupMap))
	for _, g := range groupMap {
		// Convert workflow card data to pages format
		wfs := make([]pages.WorkflowCardData, len(g.workflows))
		for i, w := range g.workflows {
			wfs[i] = pages.WorkflowCardData{
				Name:      w.Name,
				Gate:      w.Gate,
				Disabled:  w.Disabled,
				LastRunAt: w.LastRunAt,
			}
		}

		gg := pages.GateGroup{
			Gate:      g.gate,
			Label:     g.label,
			Count:     len(g.workflows),
			Workflows: wfs,
		}
		groups = append(groups, gg)
	}

	// Sort by gate name
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Gate < groups[j].Gate
	})

	return groups
}

// handleWorkflowDetail renders a single workflow with its run history.
func (s *HTMXServer) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	owner := identityFromContext(r.Context()).Username

	w.Write([]byte(fmt.Sprintf("Workflow detail: %s (owner: %s) - Phase 6", name, owner)))
}

// workflowCardData holds the data needed for a workflow card component.
type workflowCardData struct {
	Name      string
	Gate      string
	Disabled  bool
	LastRunAt string
}

// formatLastRun formats a metav1.Time as a relative string (e.g., "2h ago").
func formatLastRun(t metav1.Time) string {
	if t.IsZero() {
		return "Never"
	}

	age := time.Since(t.Time)
	switch {
	case age < time.Minute:
		return "Just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	case age < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
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
