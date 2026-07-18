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

// OwnerLabel is the label harmostes-ui uses to isolate workflows per user.
// The value is the Authentik username extracted from forward-auth headers.
const OwnerLabel = "harmostes.dev/owner"

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

	// Pages — all wrapped in auth middleware
	pages := http.NewServeMux()
	pages.HandleFunc("GET /", s.handleIndex)
	pages.HandleFunc("GET /workflows", s.handleWorkflowList)
	pages.HandleFunc("GET /workflows/{name}", s.handleWorkflowDetail)

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
		opts = append(opts, client.InNamespace(s.namespace), client.MatchingLabels{OwnerLabel: owner})
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
		opts = append(opts, client.MatchingLabels{OwnerLabel: owner})
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
	case "pages/error.html":
		return "Error"
	default:
		return page
	}
}
