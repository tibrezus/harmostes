package ui

import (
	"fmt"
	"net/http"
	"strconv"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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
	// Workflow Code island (ADR-0012 §2, #416): the template document as
	// YAML — the same artifact a future edit produces and reviews.
	YAML      string
	ModelPath string
	// Topology (ADR-0012 §3, #417): the compiled graph as the layered SVG
	// projection — the same geometry the run graph paints. Its NodeLinks
	// (when non-nil) make each node navigate to its inspector panel (#419).
	Topology      topologyView
	RevisionCount int // history entries incl. the live head; >1 links the revisions view

	// Version switcher + inspector (#419, ADR-0012 §7): the selected
	// revision drives EVERY projection on the page — pipeline, topology,
	// document, inspector values — consistently.
	Rev         int         // selected revision number (0 = head)
	RevOptions  []revOption // switcher entries; rendered when >1
	InspectNode string      // which node's panel is shown: template|prepare|agent|deploy
	Inspect     []inspectFieldView
	Historical  bool // viewing a non-head revision (inspector disabled)
}

// revOption is one entry of the version switcher.
type revOption struct {
	Rev         int // 0 = head
	Label       string
	Description string
	Selected    bool
}

// inspectFieldView is an editable field rendered with its current value.
type inspectFieldView struct {
	Path     string
	Label    string
	Kind     string // string|bool|int — the input type
	Value    string // rendered value ("" = unset)
	Disabled bool   // historical revisions render read-only
}

// templateDocument is the canonical YAML projection of a WorkflowTemplate:
// identity + spec, no status, no server bookkeeping (managedFields,
// creationTimestamp). This is the document form — what an edit writes and
// what a review diffs; live runtime state belongs to the attempt views.
type templateDocument struct {
	APIVersion string                        `json:"apiVersion"`
	Kind       string                        `json:"kind"`
	Metadata   templateDocumentMeta          `json:"metadata"`
	Spec       v1alpha1.WorkflowTemplateSpec `json:"spec"`
}

type templateDocumentMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
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

	// Version switcher (#419, Conductor pattern): ?rev=N renders every
	// projection — pipeline, topology, document, inspector — from THAT
	// revision's spec, consistently. The head is the last revision; history
	// is annotation-carried until #420. An explicit but unknown revision is
	// a 404 (a mistyped URL must not silently show the wrong version).
	revs := templateRevisions(tmpl)
	selRev := 0
	if rv := r.URL.Query().Get("rev"); rv != "" {
		n, err := strconv.Atoi(rv)
		if err != nil || n < 1 || n > len(revs) {
			s.renderErrorStatus(w, r, http.StatusNotFound, "Unknown revision: "+rv)
			return
		}
		selRev = n
	}
	spec := tmpl.Spec
	if selRev > 0 {
		spec = revs[selRev-1].Spec
	}
	historical := selRev > 0 && selRev != len(revs)

	// Inspector node: ?node=<template|prepare|agent|deploy> — the topology
	// nodes link here. Unknown nodes are 404s for the same reason.
	inspectNode := r.URL.Query().Get("node")
	if inspectNode == "" {
		inspectNode = "agent" // the meaty panel is the default
	}
	if inspectNode != "template" && inspectNode != "prepare" && inspectNode != "agent" && inspectNode != "deploy" {
		s.renderErrorStatus(w, r, http.StatusNotFound, "Unknown inspector node: "+inspectNode)
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

	topo := buildTopology(graphForTemplate(spec), s.nodeTypePalette(r.Context()))
	topo.NodeLinks = nodeLinks(selRev) // the frag reads links off the view itself
	data := templateDetailView{
		Name:          tmpl.Name,
		Description:   spec.Description,
		Pipeline:      buildPipelineView(graphForTemplate(spec)),
		PreparePlugin: spec.Prepare.Plugin.Name,
		GateName:      spec.Agent.Gate.Plugin.Name,
		DeployPlugin:  spec.Deploy.Plugin.Name,
		AgentModel:    spec.Agent.Model,
		AgentSkill:    spec.Agent.Skill,
		AgentTools:    spec.Agent.Tools,
		AgentMaxFixes: spec.Agent.MaxFixes,
		AgentTimeout:  spec.Agent.Timeout,
		AgentScope:    spec.Agent.Scope,
		WorkflowCount: len(workflows),
		Workflows:     workflows,
		YAML:          templateYAMLOf(tmpl, spec),
		ModelPath:     tmpl.Name + ".yaml",
		Topology:      topo,
		RevisionCount: len(revs),
		Rev:           selRev,
		RevOptions:    revOptions(revs, selRev),
		InspectNode:   inspectNode,
		Inspect:       inspectFieldsFor(spec, inspectNode, historical),
		Historical:    historical,
	}
	s.render(w, r, "pages/template_detail.html", data)
}

// nodeLinks maps each inspector node to its URL on this page (same revision
// context). The topology fragment turns these into per-node links — only
// the template's authoring view navigates.
func nodeLinks(selRev int) map[string]string {
	links := map[string]string{}
	for _, n := range []string{"template", "prepare", "agent", "deploy"} {
		href := "?node=" + n
		if selRev > 0 {
			href += "&rev=" + strconv.Itoa(selRev)
		}
		links[n] = href
	}
	return links
}

// revOptions builds the version-switcher entries: one per revision, head
// last (the live CR spec), the selected one marked. Nil when there is no
// history — a lone "head" entry is noise.
func revOptions(revs []templateRevision, selRev int) []revOption {
	if len(revs) <= 1 {
		return nil
	}
	opts := make([]revOption, 0, len(revs)+1)
	for _, r := range revs {
		isHead := r.Rev == revs[len(revs)-1].Rev
		opt := revOption{
			Rev:         r.Rev,
			Label:       "head",
			Description: r.Description,
			Selected:    r.Rev == selRev || (selRev == 0 && isHead),
		}
		if !isHead {
			opt.Label = fmt.Sprintf("r%d", r.Rev)
		}
		opts = append(opts, opt)
	}
	return opts
}

// inspectFieldsFor renders the inspector panel for one node: the whitelisted
// fields with their values from the selected spec. Historical revisions
// render the panel disabled — the values are what that revision declared.
func inspectFieldsFor(spec v1alpha1.WorkflowTemplateSpec, node string, historical bool) []inspectFieldView {
	out := []inspectFieldView{}
	for _, f := range inspectFields {
		if f.Node != node {
			continue
		}
		v := inspectFieldView{Path: f.Path, Label: f.Label, Kind: f.Kind, Disabled: historical}
		switch f.Path {
		case "description":
			v.Value = spec.Description
		case "prepare.plugin.name":
			v.Value = spec.Prepare.Plugin.Name
		case "agent.enabled":
			// EFFECTIVE value: nil reads as enabled (the compile default).
			v.Value = strconv.FormatBool(spec.Agent.EnabledOrDefault())
		case "agent.model":
			v.Value = spec.Agent.Model
		case "agent.skill":
			v.Value = spec.Agent.Skill
		case "agent.gate.plugin.name":
			v.Value = spec.Agent.Gate.Plugin.Name
		case "agent.maxFixes":
			if spec.Agent.MaxFixes > 0 {
				v.Value = strconv.Itoa(spec.Agent.MaxFixes)
			}
		case "deploy.plugin.name":
			v.Value = spec.Deploy.Plugin.Name
		}
		out = append(out, v)
	}
	return out
}

// templateYAML renders the WorkflowTemplate as its canonical document YAML
// (spec + identity only — see templateDocument). Marshal failure is a
// programming error (structs with json tags); panicking would take the page
// down, so fall back to an explicitly-marked placeholder instead.
func templateYAML(tmpl *v1alpha1.WorkflowTemplate) string {
	return templateYAMLOf(tmpl, tmpl.Spec)
}

// templateYAMLOf is templateYAML for an explicit spec — the version
// switcher (#419) renders historical revision specs through the same
// document shape. Labels stay the live CR's (identity is not revisioned).
func templateYAMLOf(tmpl *v1alpha1.WorkflowTemplate, spec v1alpha1.WorkflowTemplateSpec) string {
	doc := templateDocument{
		APIVersion: v1alpha1.SchemeGroupVersion.Identifier(),
		Kind:       "WorkflowTemplate",
		Metadata: templateDocumentMeta{
			Name:      tmpl.Name,
			Namespace: tmpl.Namespace,
			Labels:    tmpl.Labels,
		},
		Spec: spec,
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "# template serialization failed: " + err.Error()
	}
	return string(b)
}

// listAllWorkflows returns all workflows in the namespace (for template usage).
func (s *Server) listAllWorkflows(r *http.Request) ([]v1alpha1.Workflow, error) {
	var list v1alpha1.WorkflowList
	if err := s.k8sClient.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}
