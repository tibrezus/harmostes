package graph

import (
	"bytes"
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

// Regression: declarative Workflows keep their prepare plugin config in the
// compiled graph — without it, pr-fetch saw an empty repo list and skipped
// every run (the pr-review workflow never fired on labeled PRs).
func TestCompileWorkflowCarriesPrepareConfig(t *testing.T) {
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "pr-fetch", ConfigMap: "harmostes-pr-review"},
				Config: json.RawMessage(`{"label":"needs-review","repos":["tibrezus/harmostes"]}`),
			},
			Agent: v1alpha1.AgentSpec{
				Model:        "m",
				TaskTemplate: v1alpha1.TaskTemplate{Name: "t"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "g"}},
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
		},
	}

	gs := CompileWorkflow(wf)

	var prepare PluginNodeConfig
	for _, n := range gs.Nodes {
		if n.ID == "prepare" {
			if err := json.Unmarshal(n.Config, &prepare); err != nil {
				t.Fatalf("unmarshal prepare config: %v", err)
			}
		}
	}
	if prepare.Name != "pr-fetch" {
		t.Fatalf("prepare plugin = %q, want pr-fetch", prepare.Name)
	}
	if string(prepare.Config) == "" || !bytes.Contains(prepare.Config, []byte("needs-review")) {
		t.Fatalf("prepare plugin config lost in compilation: %q", string(prepare.Config))
	}
	// And the plugin-visible SPEC (node.Config) must surface it at top level.
	for _, n := range gs.Nodes {
		if n.ID == "prepare" && !bytes.Contains(n.Config, []byte(`"config":`)) {
			t.Fatalf("node.Config lacks config key: %s", string(n.Config))
		}
	}
}

func TestCompileWorkflowTaskRefCarriesResolution(t *testing.T) {
	enabled := true
	wf := &v1alpha1.Workflow{Spec: v1alpha1.WorkflowSpec{
		Agent: v1alpha1.AgentSpec{
			Enabled:      &enabled,
			Model:        "m",
			TaskTemplate: v1alpha1.TaskTemplate{Name: "pr-review", ConfigMap: "harmostes-tasks", Key: "pr-review.txt"},
		},
	}}
	g := CompileWorkflow(wf)
	var agent v1alpha1.NodeSpec
	for _, n := range g.Nodes {
		if n.Type == "agent" {
			agent = n
		}
	}
	var cfg AgentNodeConfig
	if err := json.Unmarshal(agent.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Task != "harmostes-tasks/pr-review.txt" {
		t.Fatalf("task ref must carry configmap/key so the resolver can fetch the real text, got %q", cfg.Task)
	}
}

// Compiled nodes carry real component names (the map/canvas fidelity fix):
// plugin names and agent · model, not anonymous prepare/agent/deploy.
func TestCompileLabelsRealComponents(t *testing.T) {
	enabled := true
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "workspace"}},
			Agent: v1alpha1.AgentSpec{
				Enabled: &enabled,
				Model:   "litellm/zai/glm-5.3",
				Gate:    v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
		},
	}
	g := CompileWorkflow(wf)
	labels := map[string]string{}
	for _, n := range g.Nodes {
		labels[n.ID] = n.Label
	}
	if labels["prepare"] != "workspace" {
		t.Errorf("prepare label = %q, want workspace", labels["prepare"])
	}
	if labels["agent"] != "agent · litellm/zai/glm-5.3" {
		t.Errorf("agent label = %q", labels["agent"])
	}
	if labels["deploy"] != "post-review" {
		t.Errorf("deploy label = %q, want post-review", labels["deploy"])
	}
}
