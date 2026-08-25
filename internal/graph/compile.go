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

	prepareLabel := wf.Spec.Prepare.Plugin.Name
	if prepareLabel == "" {
		prepareLabel = "prepare"
	}
	nodes := []v1alpha1.NodeSpec{
		{
			ID:     "prepare",
			Type:   "plugin",
			Label:  prepareLabel, // real component name for the map/canvas
			Config: prepareCfg,
		},
	}

	agentEnabled := wf.Spec.Agent.EnabledOrDefault()
	if agentEnabled {
		maxFixes := wf.Spec.Agent.MaxFixes
		if maxFixes == 0 {
			maxFixes = 3
		}
		agentCfg, _ := json.Marshal(AgentNodeConfig{
			Model:    wf.Spec.Agent.Model,
			Skill:    wf.Spec.Agent.Skill,
			Tools:    wf.Spec.Agent.Tools,
			Task:     taskRef(wf.Spec.Agent.TaskTemplate),
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
		agentLabel := "agent"
		if wf.Spec.Agent.Model != "" {
			agentLabel = "agent · " + wf.Spec.Agent.Model
		}
		nodes = append(nodes, v1alpha1.NodeSpec{
			ID:     "agent",
			Type:   "agent",
			Label:  agentLabel,
			Config: agentCfg,
		})
	}

	deployLabel := wf.Spec.Deploy.Plugin.Name
	if deployLabel == "" {
		deployLabel = "deploy"
	}
	nodes = append(nodes, v1alpha1.NodeSpec{
		ID:     "deploy",
		Type:   "plugin",
		Label:  deployLabel,
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

// taskRef renders a TaskTemplate as the agent node's task string. When the
// template carries full resolution info (configMap + key) the ref is
// "configmap/key" — the graph TaskResolver parses it back into a lookup, so
// the 6 KB task text actually reaches the agent (a bare name like "pr-review"
// fails the looksLikeRef heuristic and would be used as literal inline text).
// Without resolution info the bare name is kept (legacy behavior).
func taskRef(tt v1alpha1.TaskTemplate) string {
	if tt.ConfigMap != "" && tt.Key != "" {
		return tt.ConfigMap + "/" + tt.Key
	}
	return tt.Name
}

// CompileTemplate compiles a WorkflowTemplate's spec into a pipeline graph
// by delegating to CompileWorkflow with the template's spec as the workflow
// spec. One compiler, one defaults policy — template and workflow specs are
// the same shape, so they must never diverge in how they compile.
func CompileTemplate(tmpl *v1alpha1.WorkflowTemplate) v1alpha1.GraphSpec {
	wf := v1alpha1.Workflow{Spec: v1alpha1.WorkflowSpec{
		Prepare: tmpl.Spec.Prepare,
		Agent:   tmpl.Spec.Agent,
		Deploy:  tmpl.Spec.Deploy,
	}}
	return CompileWorkflow(&wf)
}
