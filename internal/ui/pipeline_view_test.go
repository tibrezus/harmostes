package ui

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestBuildTemplatePipelineView_StandardGate(t *testing.T) {
	tmpl := &v1alpha1.WorkflowTemplate{}
	tmpl.Name = "wiki-lint"
	tmpl.Spec.Prepare.Plugin.Name = "rig-emit"
	tmpl.Spec.Agent.Gate.Plugin.Name = "wiki-lint"
	tmpl.Spec.Agent.Skill = "/skills/wiki/SKILL.md"
	tmpl.Spec.Agent.Tools = []string{"read", "bash"}
	tmpl.Spec.Agent.MaxFixes = 3
	tmpl.Spec.Deploy.Plugin.Name = "git-push"

	pv := buildTemplatePipelineView(tmpl)

	if len(pv.Nodes) != 3 {
		t.Fatalf("expected 3 nodes (prepare, agent, deploy), got %d", len(pv.Nodes))
	}
	if pv.Nodes[0].Label != "PREPARE" {
		t.Errorf("node 0 label = %q, want PREPARE", pv.Nodes[0].Label)
	}
	if pv.Nodes[0].Sublabel != "rig-emit" {
		t.Errorf("node 0 sublabel = %q, want rig-emit", pv.Nodes[0].Sublabel)
	}
	if pv.Nodes[1].Label != "AGENT" {
		t.Errorf("node 1 label = %q, want AGENT", pv.Nodes[1].Label)
	}
	if pv.Nodes[1].Sublabel != "" {
		// Model is empty in this template
	}
	if pv.Nodes[2].Label != "DEPLOY" {
		t.Errorf("node 2 label = %q, want DEPLOY", pv.Nodes[2].Label)
	}
	if !pv.Linear {
		t.Error("expected linear graph")
	}
}

func TestBuildTemplatePipelineView_AgentDisabled(t *testing.T) {
	tmpl := &v1alpha1.WorkflowTemplate{}
	tmpl.Name = "deterministic"
	agentDisabled := false
	tmpl.Spec.Agent.Enabled = &agentDisabled
	tmpl.Spec.Agent.Gate.Plugin.Name = "noop"
	tmpl.Spec.Prepare.Plugin.Name = "rig-emit"
	tmpl.Spec.Deploy.Plugin.Name = "git-push"

	pv := buildTemplatePipelineView(tmpl)

	// With agent explicitly disabled, should be prepare → deploy (2 nodes).
	if len(pv.Nodes) != 2 {
		t.Fatalf("expected 2 nodes (prepare, deploy), got %d", len(pv.Nodes))
	}
	if pv.Nodes[0].Label != "PREPARE" {
		t.Errorf("node 0 = %q, want PREPARE", pv.Nodes[0].Label)
	}
	if pv.Nodes[1].Label != "DEPLOY" {
		t.Errorf("node 1 = %q, want DEPLOY", pv.Nodes[1].Label)
	}
}

func TestBuildPipelineView_GraphNative(t *testing.T) {
	// A graph-native spec with custom nodes (5 nodes, branching).
	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "fetch", Type: "plugin", Config: mustJSON(`{"name":"pr-fetch"}`)},
			{ID: "lint", Type: "plugin", Config: mustJSON(`{"name":"wiki-lint"}`)},
			{ID: "agent", Type: "agent", Config: mustJSON(`{"model":"litellm/zai/glm-5.2","skill":"/skills/pr-review/SKILL.md","maxFixes":1}`)},
			{ID: "deploy", Type: "plugin", Config: mustJSON(`{"name":"post-review"}`)},
			{ID: "notify", Type: "plugin", Config: mustJSON(`{"name":"slack-notify"}`)},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "fetch", To: "lint"},
			{From: "lint", To: "agent"},
			{From: "agent", To: "deploy"},
			{From: "agent", To: "notify"},
		},
	}

	pv := buildPipelineView(gs)

	if len(pv.Nodes) != 5 {
		t.Fatalf("expected 5 nodes, got %d", len(pv.Nodes))
	}
	// Topological order: fetch must come before lint, lint before agent,
	// agent before deploy and notify.
	ids := make([]string, len(pv.Nodes))
	for i, n := range pv.Nodes {
		ids[i] = n.ID
	}
	if ids[0] != "fetch" {
		t.Errorf("expected fetch first, got %s", ids[0])
	}
	if ids[len(ids)-1] != "deploy" && ids[len(ids)-1] != "notify" {
		t.Errorf("expected deploy or notify last, got %s", ids[len(ids)-1])
	}

	// Not linear (agent branches to deploy + notify).
	if pv.Linear {
		t.Error("expected non-linear graph (has branch)")
	}
}

func TestBuildPipelineView_AgentNodeMetadata(t *testing.T) {
	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent", Config: mustJSON(`{"model":"litellm/zai/glm-5.2","skill":"/skills/wiki/SKILL.md","tools":["read","bash"],"maxFixes":3}`)},
		},
	}

	pv := buildPipelineView(gs)
	if len(pv.Nodes) != 1 {
		t.Fatal("expected 1 node")
	}

	node := pv.Nodes[0]
	if node.Sublabel != "litellm/zai/glm-5.2" {
		t.Errorf("sublabel = %q", node.Sublabel)
	}

	// Check metadata keys
	metaKeys := make(map[string]bool)
	for _, m := range node.Meta {
		metaKeys[m.Key] = true
	}
	if !metaKeys["skill"] {
		t.Error("missing skill metadata")
	}
	if !metaKeys["tools"] {
		t.Error("missing tools metadata")
	}
	if !metaKeys["maxFixes"] {
		t.Error("missing maxFixes metadata")
	}
}

func TestTopoSort_LinearChain(t *testing.T) {
	nodes := []v1alpha1.NodeSpec{
		{ID: "c", Type: "plugin"},
		{ID: "a", Type: "plugin"},
		{ID: "b", Type: "plugin"},
	}
	edges := []v1alpha1.EdgeSpec{
		{From: "a", To: "b"},
		{From: "b", To: "c"},
	}

	sorted := topoSort(nodes, edges)
	if len(sorted) != 3 {
		t.Fatal("expected 3 nodes")
	}
	if sorted[0].ID != "a" || sorted[1].ID != "b" || sorted[2].ID != "c" {
		t.Errorf("order = %s,%s,%s; want a,b,c", sorted[0].ID, sorted[1].ID, sorted[2].ID)
	}
}

func TestIsLinear(t *testing.T) {
	tests := []struct {
		name  string
		edges []v1alpha1.EdgeSpec
		want  bool
	}{
		{"empty", nil, true},
		{"linear", []v1alpha1.EdgeSpec{{From: "a", To: "b"}, {From: "b", To: "c"}}, true},
		{"branch", []v1alpha1.EdgeSpec{{From: "a", To: "b"}, {From: "a", To: "c"}}, false},
		{"merge", []v1alpha1.EdgeSpec{{From: "a", To: "c"}, {From: "b", To: "c"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLinear(tt.edges); got != tt.want {
				t.Errorf("isLinear() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToUpper(t *testing.T) {
	tests := []struct{ in, want string }{
		{"prepare", "PREPARE"},
		{"wiki-lint", "WIKI-LINT"},
		{"", ""},
		{"ABC", "ABC"},
	}
	for _, tt := range tests {
		if got := toUpper(tt.in); got != tt.want {
			t.Errorf("toUpper(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// mustJSON panics on invalid JSON — for test fixtures only.
func mustJSON(s string) json.RawMessage {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic(err)
	}
	return json.RawMessage(s)
}
