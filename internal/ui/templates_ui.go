package ui

import (
	"fmt"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// templateListData is the template data for the template list page.
type templateListData struct {
	Templates []templateSummary
}

type templateSummary struct {
	Name        string
	Description string
	Prepare     string
	Gate        string
	Deploy      string
}

// listTemplates returns all WorkflowTemplate CRs in the namespace.
func (s *Server) listTemplates(r *http.Request) ([]v1alpha1.WorkflowTemplate, error) {
	var list v1alpha1.WorkflowTemplateList
	if err := s.k8sClient.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("list workflow templates: %w", err)
	}
	return list.Items, nil
}

// handleTemplateList renders all WorkflowTemplate CRs discovered from the cluster.
func (s *Server) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	templates, err := s.listTemplates(r)
	if err != nil {
		s.renderError(w, r, "Failed to list templates: "+err.Error())
		return
	}

	summaries := make([]templateSummary, 0, len(templates))
	for _, t := range templates {
		summaries = append(summaries, templateSummary{
			Name:        t.Name,
			Description: t.Spec.Description,
			Prepare:     t.Spec.Prepare.Plugin.Name,
			Gate:        t.Spec.Agent.Gate.Plugin.Name,
			Deploy:      t.Spec.Deploy.Plugin.Name,
		})
	}

	s.render(w, r, "pages/templates.html", map[string]any{
		"Templates": summaries,
	})
}

// templateDetailData is the template data for the template detail page.
type templateDetailData struct {
	Name        string
	Description string
	Prepare     string
	Gate        string
	Deploy      string
	Skill       string
	Model       string
	MaxFixes    int
}

// handleTemplateDetail renders a single WorkflowTemplate with its auto-generated graph.
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

	maxFixes := tmpl.Spec.Agent.MaxFixes

	data := templateDetailData{
		Name:        tmpl.Name,
		Description: tmpl.Spec.Description,
		Prepare:     tmpl.Spec.Prepare.Plugin.Name,
		Gate:        tmpl.Spec.Agent.Gate.Plugin.Name,
		Deploy:      tmpl.Spec.Deploy.Plugin.Name,
		Skill:       tmpl.Spec.Agent.Skill,
		Model:       tmpl.Spec.Agent.Model,
		MaxFixes:    maxFixes,
	}
	s.render(w, r, "pages/template_detail.html", data)
}
