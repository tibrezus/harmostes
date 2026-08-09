package ui

import (
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// templateCardView is the display model for a template in the list page.
// It includes a compact pipeline graph so the template's structure is visible
// at a glance, not just as raw text.
type templateCardView struct {
	Name        string
	Description string
	Pipeline    PipelineView
	Tools       []string
	GateName    string
	Skill       string
	MaxFixes    int
}

// templateDetailView is the display model for the template detail page.
type templateDetailView struct {
	Name        string
	Description string
	Pipeline    PipelineView
	// Feature breakdown
	PreparePlugin string
	GateName      string
	DeployPlugin  string
	AgentModel    string
	AgentSkill    string
	AgentTools    []string
	AgentMaxFixes int
	AgentTimeout  int
	AgentScope    string
	// Usage
	WorkflowCount int
	Workflows     []string
}

// listTemplates returns all WorkflowTemplate CRs in the namespace.
func (s *Server) listTemplates(r *http.Request) ([]v1alpha1.WorkflowTemplate, error) {
	var list v1alpha1.WorkflowTemplateList
	if err := s.k8sClient.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list workflow templates: %w", err)
	}
	return list.Items, nil
}

// handleTemplateList renders all WorkflowTemplate CRs with their pipeline graphs.
func (s *Server) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	templates, err := s.listTemplates(r)
	if err != nil {
		s.renderError(w, r, "Failed to list templates: "+err.Error())
		return
	}

	// Count workflows per template (by gate plugin name, since templateRef
	// is optional — most workflows inherit structure by gate convention).
	wfCounts := map[string][]string{}
	if wfs, err := s.listAllWorkflows(r); err == nil {
		for _, wf := range wfs {
			gate := wf.Spec.Agent.Gate.Plugin.Name
			if gate == "" {
				gate = "noop"
			}
			wfCounts[gate] = append(wfCounts[gate], wf.Name)
		}
	}

	cards := make([]templateCardView, 0, len(templates))
	for _, t := range templates {
		gateName := t.Spec.Agent.Gate.Plugin.Name
		if gateName == "" {
			gateName = "noop"
		}
		cards = append(cards, templateCardView{
			Name:        t.Name,
			Description: t.Spec.Description,
			Pipeline:    buildTemplatePipelineView(&t),
			Tools:       t.Spec.Agent.Tools,
			GateName:    gateName,
			Skill:       t.Spec.Agent.Skill,
			MaxFixes:    t.Spec.Agent.MaxFixes,
		})
	}

	s.render(w, r, "pages/templates.html", map[string]any{
		"Templates": cards,
	})
}

// handleTemplateDetail renders a single WorkflowTemplate with its full pipeline
// graph, feature breakdown, and usage information.
func (s *Server) handleTemplateDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.renderError(w, r, "template name required")
		return
	}

	tmpl := &v1alpha1.WorkflowTemplate{}
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, tmpl); err != nil {
		s.renderError(w, r, "Failed to get template: "+err.Error())
		return
	}

	// Find workflows that use this template (by gate plugin name match).
	var workflows []string
	if wfs, err := s.listAllWorkflows(r); err == nil {
		gateName := tmpl.Spec.Agent.Gate.Plugin.Name
		for _, wf := range wfs {
			wfGate := wf.Spec.Agent.Gate.Plugin.Name
			if wfGate == "" {
				wfGate = "noop"
			}
			if wfGate == gateName || wf.Spec.TemplateRef == name {
				workflows = append(workflows, wf.Name)
			}
		}
	}

	data := templateDetailView{
		Name:          tmpl.Name,
		Description:   tmpl.Spec.Description,
		Pipeline:      buildTemplatePipelineView(tmpl),
		PreparePlugin: tmpl.Spec.Prepare.Plugin.Name,
		GateName:      tmpl.Spec.Agent.Gate.Plugin.Name,
		DeployPlugin:  tmpl.Spec.Deploy.Plugin.Name,
		AgentModel:    tmpl.Spec.Agent.Model,
		AgentSkill:    tmpl.Spec.Agent.Skill,
		AgentTools:    tmpl.Spec.Agent.Tools,
		AgentMaxFixes: tmpl.Spec.Agent.MaxFixes,
		AgentTimeout:  tmpl.Spec.Agent.Timeout,
		AgentScope:    tmpl.Spec.Agent.Scope,
		WorkflowCount: len(workflows),
		Workflows:     workflows,
	}
	s.render(w, r, "pages/template_detail.html", data)
}

// listAllWorkflows returns all workflows in the namespace (for template usage).
func (s *Server) listAllWorkflows(r *http.Request) ([]v1alpha1.Workflow, error) {
	var list v1alpha1.WorkflowList
	if err := s.k8sClient.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}
