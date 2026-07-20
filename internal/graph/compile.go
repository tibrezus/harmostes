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
		Name: wf.Spec.Prepare.Plugin.Name,
	})
	deployCfg, _ := json.Marshal(PluginNodeConfig{
		Name: wf.Spec.Deploy.Plugin.Name,
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
					Name: wf.Spec.Agent.Gate.Plugin.Name,
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
