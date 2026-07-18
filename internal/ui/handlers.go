package ui

import (
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// handleIndex redirects to the workflow list.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Only match exact "/" — Go 1.22 mux matches subtree for "/".
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/workflows", http.StatusSeeOther)
}

// handleWorkflowList renders all workflows owned by the current user.
func (s *Server) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflows, err := s.listWorkflows(r, owner)
	if err != nil {
		s.logger.Error("list workflows", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load workflows: "+err.Error())
		return
	}
	s.render(w, r, "pages/workflows.html", map[string]any{
		"Workflows": workflows,
	})
}

// handleWorkflowDetail renders a single workflow with its run history.
func (s *Server) handleWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	// Get the workflow (filtered by owner)
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &wf); err != nil {
		s.logger.Error("get workflow", "name", name, "err", err)
		s.renderError(w, r, "Workflow not found: "+name)
		return
	}

	// Enforce owner isolation: if the workflow has an owner label and it
	// doesn't match the current user, treat as not found.
	if wf.Labels[OwnerLabel] != "" && wf.Labels[OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	// Get run history (Jobs for this workflow, filtered by owner)
	jobs, err := s.listJobs(r, name, owner)
	if err != nil {
		s.logger.Error("list jobs", "workflow", name, "err", err)
		jobs = nil // non-fatal — show workflow without run history
	}

	s.render(w, r, "pages/detail.html", map[string]any{
		"Workflow": wf,
		"Jobs":     jobs,
	})
}

// renderError renders the error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, msg string) {
	s.render(w, r, "pages/error.html", map[string]any{
		"Error": msg,
	})
}
