// Package ui implements the harmostes-ui HTTP server: a self-service web
// interface for managing Workflow CRs. It reads user identity from Authentik
// forward-auth headers and proxies to the Kubernetes API, filtering all queries
// by the harmostes.dev/owner label so each user sees only their own workflows.
package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

//go:embed templates/*.html templates/pages/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the harmostes-ui HTTP server.
type Server struct {
	k8sClient client.Client
	namespace string
	logger    *slog.Logger
	templates *template.Template
}

// New creates a Server with parsed templates and the given k8s client.
func New(k8sClient client.Client, namespace string, logger *slog.Logger) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		k8sClient: k8sClient,
		namespace: namespace,
		logger:    logger,
		templates: tmpl,
	}, nil
}

// Routes returns the HTTP handler with all routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Static assets (CSS, fonts, JS)
	staticSub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))

	// Health check (no auth — kubelet probes don't send forward-auth headers)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Pages — all wrapped in auth middleware
	pages := http.NewServeMux()
	pages.HandleFunc("GET /", s.handleIndex)
	pages.HandleFunc("GET /workflows", s.handleWorkflowList)
	pages.HandleFunc("GET /workflows/new", s.handleWorkflowNew)
	pages.HandleFunc("GET /workflows/{name}", s.handleWorkflowDetail)
	pages.HandleFunc("POST /workflows", s.handleWorkflowCreate)
	pages.HandleFunc("POST /workflows/{name}/delete", s.handleWorkflowDelete)
	pages.HandleFunc("POST /workflows/{name}/trigger", s.handleWorkflowTrigger)
	pages.HandleFunc("POST /workflows/{name}/toggle", s.handleWorkflowToggle)

	// Token management (Phase C)
	pages.HandleFunc("GET /tokens", s.handleTokenList)
	pages.HandleFunc("POST /tokens", s.handleTokenCreate)
	pages.HandleFunc("POST /tokens/{name}/delete", s.handleTokenDelete)

	mux.Handle("/", s.authMiddleware(pages))

	return mux
}

func parseTemplates() (*template.Template, error) {
	tmpl := template.New("").Funcs(template.FuncMap{
		"statusClass": statusClass,
		"statusText":  statusText,
		"default": func(def any, val any) any { /* note: arg order is def, val */
			if val == nil || val == "" {
				return def
			}
			return val
		},
		"truncate": func(s string, n int) string {
			if len(s) > n {
				return s[:n] + "…"
			}
			return s
		},
	})
	for _, pattern := range []string{"templates/*.html", "templates/pages/*.html"} {
		matches, err := fs.Glob(templateFS, pattern)
		if err != nil {
			return nil, fmt.Errorf("glob %s: %w", pattern, err)
		}
		for _, m := range matches {
			data, err := templateFS.ReadFile(m)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", m, err)
			}
			name := strings.TrimPrefix(m, "templates/")
			if _, err := tmpl.New(name).Parse(string(data)); err != nil {
				return nil, fmt.Errorf("parse %s: %w", m, err)
			}
		}
	}
	return tmpl, nil
}

// render executes a named template into the layout via two-pass rendering:
// pass 1 renders the page template into an HTML string; pass 2 renders the
// layout with the page output as .Content (template.HTML, not escaped).
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	user := identityFromContext(r.Context())

	pageTmpl := s.templates.Lookup(page)
	if pageTmpl == nil {
		s.logger.Error("template not found", "page", page)
		http.Error(w, "template not found: "+page, http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		s.logger.Error("render page template", "page", page, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	layout := s.templates.Lookup("layout.html")
	if err := layout.Execute(w, map[string]any{
		"Page":    pageTitle(page),
		"PageKey": pageKey(page),
		"Content": template.HTML(buf.String()),
		"User":    user,
	}); err != nil {
		s.logger.Error("render layout", "page", page, "err", err)
	}
}

// listWorkflows returns all Workflow CRs owned by the given user.
func (s *Server) listWorkflows(r *http.Request, owner string) ([]v1alpha1.Workflow, error) {
	var list v1alpha1.WorkflowList
	opts := []client.ListOption{}
	if owner != "" {
		opts = append(opts, client.InNamespace(s.namespace), client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	} else {
		opts = append(opts, client.InNamespace(s.namespace))
	}
	if err := s.k8sClient.List(r.Context(), &list, opts...); err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	return list.Items, nil
}

// listJobs returns worker Jobs for a workflow, filtered by owner.
func (s *Server) listJobs(r *http.Request, workflow, owner string) ([]batchv1.Job, error) {
	var jobList batchv1.JobList
	opts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{"harmostes.dev/workflow": workflow},
	}
	if owner != "" {
		// Jobs inherit the owner label from the controller.
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(r.Context(), &jobList, opts...); err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return jobList.Items, nil
}

// statusClass maps a gate status to a CSS class for badges/dots.
func statusClass(status string) string {
	switch status {
	case "green":
		return "green"
	case "failed":
		return "red"
	default:
		return "muted"
	}
}

// statusText maps a gate status to human-readable text.
func statusText(status string) string {
	switch status {
	case "green":
		return "Green"
	case "failed":
		return "Failed"
	case "skipped":
		return "Skipped"
	default:
		return "Unknown"
	}
}

// pageTitle maps a template path to a human-readable page title.
func pageTitle(page string) string {
	switch page {
	case "pages/workflows.html":
		return "Workflows"
	case "pages/detail.html":
		return "Workflow Detail"
	case "pages/tokens.html":
		return "Tokens"
	case "pages/workflow_new.html":
		return "New Workflow"
	case "pages/error.html":
		return "Error"
	default:
		return page
	}
}

// pageKey returns a lowercase key for nav active-state matching in the layout.
func pageKey(page string) string {
	switch {
	case strings.HasPrefix(page, "pages/workflows"):
		return "workflows"
	case strings.HasPrefix(page, "pages/tokens"):
		return "tokens"
	case strings.HasPrefix(page, "pages/workflow_new"):
		return "workflows" // active state stays on Workflows nav
	default:
		return ""
	}
}

// SanitizeLabels strips any client-supplied harmostes.dev/owner label from a
// label map and stamps the authenticated user's username instead. This is the
// anti-spoofing boundary: the owner label is ALWAYS set by the server from the
// verified Authentik identity, never trusted from client input (a malicious
// user could otherwise set owner=alice to create a workflow under another
// user's namespace).
//
// It also strips harmostes.dev/workflow (a controller-managed label) to
// prevent accidental or malicious override of the workflow linkage.
func SanitizeLabels(labels map[string]string, owner string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	delete(labels, v1alpha1.OwnerLabel)
	delete(labels, v1alpha1.WorkflowLabel)
	if owner != "" {
		labels[v1alpha1.OwnerLabel] = owner
	}
	return labels
}

// StampOwnerLabel sets the owner label on a Workflow CR's ObjectMeta, stripping
// any pre-existing value first. Used by the Workflow CRUD handlers (Phase D)
// to ensure every UI-created CR carries the authenticated user's identity.
func StampOwnerLabel(obj *v1alpha1.Workflow, owner string) {
	if obj.Labels == nil {
		obj.Labels = map[string]string{}
	}
	delete(obj.Labels, v1alpha1.OwnerLabel)
	delete(obj.Labels, v1alpha1.WorkflowLabel)
	obj.Labels[v1alpha1.OwnerLabel] = owner
}
