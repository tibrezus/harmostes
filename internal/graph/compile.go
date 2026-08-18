package graph

import (
	"encoding/json"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// CompileWorkflow translates a Workflow CR spec into an equivalent pipeline
// graph. This enables backward compatibility: existing Workflow CRs (the
// fixed-shape prepare→agent→deploy pipeline) can run through the graph executor
// without migration.
//
// The compiled graph preserves the agent's inline gate: the agent node carries
// its gate configuration, and the AgentExecutor runs the feedback loop
// internally (matching the current worker behavior). In a pure graph-native
// Pipeline CR, the gate would be a separate node with a loop-back edge.
func CompileWorkflow(wf *v1alpha1.Workflow) v1alpha1.GraphSpec {
	prepareCfg, _ := json.Marshal(PluginNodeConfig{
		Name:      wf.Spec.Prepare.Plugin.Name,
		Args:      wf.Spec.Prepare.Plugin.Args,
		ConfigMap: wf.Spec.Prepare.Plugin.ConfigMap,
		Config:    wf.Spec.Prepare.Config,
	})
	deployCfg, _ := json.Marshal(PluginNodeConfig{
		Name:      wf.Spec.Deploy.Plugin.Name,
		Args:      wf.Spec.Deploy.Plugin.Args,
		ConfigMap: wf.Spec.Deploy.Plugin.ConfigMap,
	})

	nodes := []v1alpha1.NodeSpec{
		{
			ID:     "prepare",
			Type:   "plugin",
			Config: prepareCfg,
		},
	}

	agentEnabled := wf.Spec.Agent.Enabled == nil || *wf.Spec.Agent.Enabled
	if agentEnabled {
		maxFixes := wf.Spec.Agent.MaxFixes
		if maxFixes == 0 {
			maxFixes = 3
		}
		agentCfg, _ := json.Marshal(AgentNodeConfig{
			Model:    wf.Spec.Agent.Model,
			Skill:    wf.Spec.Agent.Skill,
			Tools:    wf.Spec.Agent.Tools,
			Task:     wf.Spec.Agent.TaskTemplate.Name,
			MaxFixes: maxFixes,
			Gate: &GateNodeConfig{
				Plugin: PluginNodeConfig{
					Name:      wf.Spec.Agent.Gate.Plugin.Name,
					ConfigMap: wf.Spec.Agent.Gate.Plugin.ConfigMap,
					Args:      wf.Spec.Agent.Gate.Plugin.Args,
				},
			},
			Scope: wikiLintScope(wf.Name),
		})
		nodes = append(nodes, v1alpha1.NodeSpec{
			ID:     "agent",
			Type:   "agent",
			Config: agentCfg,
		})
	}

	nodes = append(nodes, v1alpha1.NodeSpec{
		ID:     "deploy",
		Type:   "plugin",
		Config: deployCfg,
	})

	edges := []v1alpha1.EdgeSpec{
		{From: "prepare", To: "agent"},
	}
	if !agentEnabled {
		edges = []v1alpha1.EdgeSpec{
			{From: "prepare", To: "deploy"},
		}
	} else {
		edges = append(edges, v1alpha1.EdgeSpec{From: "agent", To: "deploy"})
	}

	return v1alpha1.GraphSpec{
		Nodes: nodes,
		Edges: edges,
	}
}

// wikiLintScope returns the task scope clause for wiki-lint workflows, or
// empty for other gate types. The scope confines the agent to one project
// under raw/arch/ so a multi-project namespace doesn't have the agent touch
// every project. This matches the declarative pipeline's scope injection.
func wikiLintScope(workflowName string) string {
	return "SCOPE: this Workflow owns exactly ONE project: " + workflowName +
		". Work ONLY on raw/arch/" + workflowName +
		"/, its model.c4, and wiki/entities/" + workflowName +
		".md (plus index.md/log.md). Do NOT read or modify any other project" +
		" under raw/arch/ \u2014 those are owned by other Workflows."
}

// CompileTemplate compiles a WorkflowTemplate's spec into a pipeline graph,
// identical in structure to CompileWorkflow but operating on WorkflowTemplateSpec.
// This lets the UI render any template as a visual pipeline graph without
// duplicating the compilation logic.
func CompileTemplate(tmpl *v1alpha1.WorkflowTemplate) v1alpha1.GraphSpec {
	prepareCfg, _ := json.Marshal(PluginNodeConfig{
		Name: tmpl.Spec.Prepare.Plugin.Name,
	})
	deployCfg, _ := json.Marshal(PluginNodeConfig{
		Name: tmpl.Spec.Deploy.Plugin.Name,
	})

	nodes := []v1alpha1.NodeSpec{
		{
			ID:     "prepare",
			Type:   "plugin",
			Config: prepareCfg,
		},
	}

	agentEnabled := tmpl.Spec.Agent.Enabled == nil || *tmpl.Spec.Agent.Enabled
	if agentEnabled {
		maxFixes := tmpl.Spec.Agent.MaxFixes
		if maxFixes == 0 {
			maxFixes = 3
		}
		agentCfg, _ := json.Marshal(AgentNodeConfig{
			Model:    tmpl.Spec.Agent.Model,
			Skill:    tmpl.Spec.Agent.Skill,
			Tools:    tmpl.Spec.Agent.Tools,
			Task:     tmpl.Spec.Agent.TaskTemplate.Name,
			MaxFixes: maxFixes,
			Gate: &GateNodeConfig{
				Plugin: PluginNodeConfig{
					Name: tmpl.Spec.Agent.Gate.Plugin.Name,
				},
			},
		})
		nodes = append(nodes, v1alpha1.NodeSpec{
			ID:     "agent",
			Type:   "agent",
			Config: agentCfg,
		})
	}

	nodes = append(nodes, v1alpha1.NodeSpec{
		ID:     "deploy",
		Type:   "plugin",
		Config: deployCfg,
	})

	edges := []v1alpha1.EdgeSpec{
		{From: "prepare", To: "agent"},
	}
	if !agentEnabled {
		edges = []v1alpha1.EdgeSpec{
			{From: "prepare", To: "deploy"},
		}
	} else {
		edges = append(edges, v1alpha1.EdgeSpec{From: "agent", To: "deploy"})
	}

	return v1alpha1.GraphSpec{
		Nodes: nodes,
		Edges: edges,
	}
}
