package v1alpha1

// ApplyTemplateDefaults overlays a WorkflowTemplate's defaults onto a Workflow
// that references it via spec.templateRef. Every field the Workflow leaves
// unset is inherited from the template; fields the Workflow sets win — a
// Workflow is a thin instantiation of a reusable pipeline shape.
//
// After the overlay, spec.config (the instance-level scope: repos, label,
// wiki, …) is applied on top of prepare.config, so a template may ship a
// default scope and an instance may override it wholesale.
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

	// Instance scope wins: spec.config overrides whatever prepare.config
	// holds after the template overlay.
	if len(s.Config) > 0 {
		s.Prepare.Config = s.Config
	}
}
