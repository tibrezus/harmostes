package graph

import (
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestCompileWorkflowProducesScope(t *testing.T) {
	enabled := true
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "rig-emit"},
			},
			Agent: v1alpha1.AgentSpec{
				Enabled:      &enabled,
				Model:        "test/model",
				Skill:        "/skills/wiki/SKILL.md",
				TaskTemplate: v1alpha1.TaskTemplate{Name: "arch-sync"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "git-push"},
			},
		},
	}

	gs := CompileWorkflow(wf)

	if len(gs.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (prepare, agent, deploy)", len(gs.Nodes))
	}
	if len(gs.Edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(gs.Edges))
	}

	// The agent node should carry a Scope field confining it to the project.
	var agentCfg AgentNodeConfig
	for _, n := range gs.Nodes {
		if n.Type == "agent" {
			if err := json.Unmarshal(n.Config, &agentCfg); err != nil {
				t.Fatalf("unmarshal agent config: %v", err)
			}
		}
	}
	if agentCfg.Scope == "" {
		t.Error("agent config Scope is empty, want a project-scoping clause")
	}
	if !strings.Contains(agentCfg.Scope, wf.Name) {
		t.Errorf("Scope does not contain workflow name %q: %q", wf.Name, agentCfg.Scope)
	}
}

func TestCompileWorkflowAgentDisabledSkipsAgent(t *testing.T) {
	disabled := false
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "fork-sync"},
			},
			Agent: v1alpha1.AgentSpec{
				Enabled:      &disabled,
				Model:        "none",
				TaskTemplate: v1alpha1.TaskTemplate{Name: "none"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "noop"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "noop"},
			},
		},
	}

	gs := CompileWorkflow(wf)

	if len(gs.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (prepare, deploy — no agent)", len(gs.Nodes))
	}
	// Edge should be prepare → deploy (no agent node)
	if len(gs.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(gs.Edges))
	}
	if gs.Edges[0].From != "prepare" || gs.Edges[0].To != "deploy" {
		t.Errorf("edge = %s→%s, want prepare→deploy", gs.Edges[0].From, gs.Edges[0].To)
	}
}

func TestCompileWorkflowCarriesConfigMapAndArgs(t *testing.T) {
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{
					Name:      "fork-sync",
					ConfigMap: "fork-maintenance-plugins",
					Args:      []string{"forgejo"},
				},
			},
			Agent: v1alpha1.AgentSpec{
				Model:        "none",
				TaskTemplate: v1alpha1.TaskTemplate{Name: "none"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "noop"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "noop"},
			},
		},
	}

	gs := CompileWorkflow(wf)

	var prepCfg PluginNodeConfig
	for _, n := range gs.Nodes {
		if n.ID == "prepare" {
			if err := json.Unmarshal(n.Config, &prepCfg); err != nil {
				t.Fatalf("unmarshal prepare config: %v", err)
			}
		}
	}
	if prepCfg.ConfigMap != "fork-maintenance-plugins" {
		t.Errorf("ConfigMap = %q, want fork-maintenance-plugins", prepCfg.ConfigMap)
	}
	if len(prepCfg.Args) != 1 || prepCfg.Args[0] != "forgejo" {
		t.Errorf("Args = %v, want [forgejo]", prepCfg.Args)
	}
}
