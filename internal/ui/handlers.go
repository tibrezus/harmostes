package ui

import (
	"net/http"
	"sort"

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

// handleWorkflowList renders all workflows owned by the current user,
// grouped by gate archetype.
func (s *Server) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflows, err := s.listWorkflows(r, owner)
	if err != nil {
		s.logger.Error("list workflows", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load workflows: "+err.Error())
		return
	}

	// Group workflows by gate (derived from spec.agent.gate.plugin.name).
	type gateGroup struct {
		Gate     string // gate plugin name
		Label    string // category label with icon
		Category string
		Count    int
		Items    []v1alpha1.Workflow
	}
	groups := []gateGroup{}
	groupMap := map[string]*gateGroup{}
	for i := range workflows {
		wf := &workflows[i]
		gateName := workflowGate(wf.Spec.Agent.Gate.Plugin.Name)
		g, ok := groupMap[gateName]
		if !ok {
			cat := "other"
			label := gateName
			if arch := gateByName(gateName); arch != nil {
				cat = arch.Category
				label = arch.Label
			}
			g = &gateGroup{
				Gate:     gateName,
				Label:    gateCategoryLabel(cat) + " — " + label,
				Category: cat,
			}
			groupMap[gateName] = g
			groups = append(groups, gateGroup{}) // placeholder; updated below
		}
		g.Items = append(g.Items, *wf)
		g.Count++
	}

	// Copy back from map (map pointers were updated in-place).
	for i := range groups {
		g := groupMap[groups[i].Gate]
		if g != nil {
			groups[i] = *g
		}
	}

	// Sort groups by category for stable display.
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Category < groups[j].Category
	})

	s.render(w, r, "pages/workflows.html", map[string]any{
		"Workflows":  workflows,
		"GateGroups": groups,
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

	// Enforce owner isolation: a workflow without an owner label is
	// "unmanaged" (GitOps-created system workflow) and is NOT surfaced in
	// the self-service UI. A workflow with a non-matching owner label is
	// treated as not found.
	if wf.Labels[v1alpha1.OwnerLabel] != owner {
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
