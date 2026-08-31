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
	http.Redirect(w, r, "/runs", http.StatusSeeOther)
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

	// Group workflows by their template identity (spec.templateRef, resolved).
	// The WorkflowTemplate CRs are the single archetype registry — no more
	// plugin-name guessing; the description comes from the template itself.
	templates, err := s.listTemplates(r)
	if err != nil {
		s.logger.Error("list templates for grouping", "err", err)
		templates = nil // non-fatal — group with bare names
	}
	tmplMeta := map[string]v1alpha1.WorkflowTemplate{}
	for _, t := range templates {
		tmplMeta[t.Name] = t
	}

	type gateGroup struct {
		Gate        string // gate plugin name (resolved spec)
		Label       string // template name (grouping identity)
		Category    string
		CategoryUI  string // template kind marker
		Description string // template description
		Count       int
		Items       []v1alpha1.Workflow
	}
	groupMap := map[string]*gateGroup{}
	for i := range workflows {
		wf := &workflows[i]
		resolved := s.resolveWorkflow(r.Context(), wf)
		key := resolved.Spec.TemplateRef
		if key == "" {
			key = "custom" // graph-native or standalone specs
		}
		g, ok := groupMap[key]
		if !ok {
			label, desc := key, ""
			if t, found := tmplMeta[key]; found {
				desc = t.Spec.Description
			}
			g = &gateGroup{
				Gate:        resolved.Spec.Agent.Gate.Plugin.Name,
				Label:       label,
				Category:    "template",
				CategoryUI:  "template",
				Description: desc,
				Items:       []v1alpha1.Workflow{},
			}
			groupMap[key] = g
		}
		g.Items = append(g.Items, *wf)
		g.Count++
	}

	// Convert map to slice, sorted by category.
	groups := make([]gateGroup, 0, len(groupMap))
	for _, g := range groupMap {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Category < groups[j].Category
	})

	s.render(w, r, "pages/workflows.html", map[string]any{
		"Workflows":  workflows,
		"GateGroups": groups,
	})
}

// workflowNames lists the owner's workflow names, sorted — feeds the inline
// workflow selects on the consuming pages (timeline, sessions, metrics).
func (s *Server) workflowNames(r *http.Request, owner string) []string {
	workflows, err := s.listWorkflows(r, owner)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		names = append(names, wf.Name)
	}
	sort.Strings(names)
	return names
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

	// Thin templateRef instances render their merged shape (template defaults
	// + instance config) — pipeline, archetype, and graph all show the real
	// structure while the stored CR stays thin.
	resolved := s.resolveWorkflow(r.Context(), &wf)

	// Get run history (Jobs for this workflow, filtered by owner)
	jobs, err := s.listJobs(r, name, owner)
	if err != nil {
		s.logger.Error("list jobs", "workflow", name, "err", err)
		jobs = nil // non-fatal — show workflow without run history
	}

	s.render(w, r, "pages/detail.html", map[string]any{
		"Workflow": resolved,
		"Jobs":     jobs,
		"Pipeline": buildWorkflowPipelineView(&resolved),
	})
}

// renderError renders the error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, msg string) {
	s.render(w, r, "pages/error.html", map[string]any{
		"Error": msg,
	})
}
