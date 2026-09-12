package v1alpha1

import "encoding/json"

// ApplyTemplateDefaults overlays a WorkflowTemplate's defaults onto a Workflow
// that references it via spec.templateRef. Every field the Workflow leaves
// unset is inherited from the template; fields the Workflow sets win — a
// Workflow is a thin instantiation of a reusable pipeline shape.
//
// After the overlay, spec.config (the instance-level scope: repos, label,
// wiki, …) is applied per key on top of prepare.config — instance-set keys
// win, template keys survive — so a template may ship a default scope and an
// instance overrides only the keys it declares.
//
// This runs in the worker right after it fetches the Workflow CR, so every
// execution path (schedule, webhook, manual) sees the merged spec.
func ApplyTemplateDefaults(wf *Workflow, tmpl *WorkflowTemplate) {
	if wf == nil || tmpl == nil {
		return
	}
	t := tmpl.Spec
	s := &wf.Spec

	// Prepare: plugin resolution (name/configMap) and defaults.
	if s.Prepare.Plugin.Name == "" {
		s.Prepare.Plugin = t.Prepare.Plugin
	}
	if s.Prepare.Detect == "" {
		s.Prepare.Detect = t.Prepare.Detect
	}
	if s.Prepare.Output == "" {
		s.Prepare.Output = t.Prepare.Output
	}
	if s.Prepare.Config == nil {
		s.Prepare.Config = t.Prepare.Config
	}

	// Agent: model, skill, tools, task, gate, tuning — field-wise.
	a, ta := &s.Agent, &t.Agent
	if a.Enabled == nil {
		a.Enabled = ta.Enabled
	}
	if a.Model == "" {
		a.Model = ta.Model
	}
	if a.Skill == "" {
		a.Skill = ta.Skill
	}
	if len(a.Tools) == 0 {
		a.Tools = ta.Tools
	}
	if a.TaskTemplate == (TaskTemplate{}) {
		a.TaskTemplate = ta.TaskTemplate
	}
	if a.Gate.Plugin.Name == "" {
		a.Gate = ta.Gate
	}
	if a.MaxFixes == 0 {
		a.MaxFixes = ta.MaxFixes
	}
	if a.Timeout == 0 {
		a.Timeout = ta.Timeout
	}
	if a.Scope == "" {
		a.Scope = ta.Scope
	}

	// Deploy: plugin resolution.
	if s.Deploy.Plugin.Name == "" {
		s.Deploy.Plugin = t.Deploy.Plugin
	}

	// Review-Ready Gate (ADR-0006): the template declares the gate; the
	// instance may override it.
	if s.ReviewReady == nil {
		s.ReviewReady = t.ReviewReady
	}

	// Cache: whole-struct inherit, same semantics as ReviewReady — the
	// flags (git/go/npm) are one coherent mount config, not per-instance
	// knobs an instance would partially override.
	if s.Cache == nil {
		s.Cache = t.Cache
	}

	// Instance scope wins PER KEY: spec.config overlays prepare.config —
	// instance-set fields win, template fields survive. (The wholesale
	// replace shipped earlier contradicted the overlay contract: any
	// instance that set ANY key — even `{}` from a scope-less form —
	// silently discarded the template's prepare.config; PR #427 review P2.)
	if len(s.Config) > 0 {
		var inst map[string]any
		if err := json.Unmarshal(s.Config, &inst); err != nil || inst == nil {
			// Non-object instance config is a deliberate opaque payload —
			// wholesale replace is the only sane reading.
			s.Prepare.Config = s.Config
		} else {
			base := map[string]any{}
			_ = json.Unmarshal(s.Prepare.Config, &base) // non-object base → empty underlay
			for k, v := range inst {
				base[k] = v
			}
			if merged, err := json.Marshal(base); err == nil {
				s.Prepare.Config = merged
			}
		}
	}
}

// TemplateRevision is one entry of the TemplateRevisionsAnnotation history
// (ascending; the CR's live spec is the head — readers derive it as
// Rev = len(entries)+1, never stored redundantly). Description is free text
// for humans; the recorder leaves it empty.
type TemplateRevision struct {
	Rev         int                  `json:"rev"`
	Description string               `json:"description,omitempty"`
	Spec        WorkflowTemplateSpec `json:"spec"`
}
