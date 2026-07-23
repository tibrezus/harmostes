package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// TestHandleWorkflowGraphAPI verifies that the /api/workflows/{name}/graph
// endpoint compiles a Workflow CR's declarative spec (prepare → agent → deploy)
// into a GraphSpec with the correct nodes and edges.
func TestHandleWorkflowGraphAPI(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-wiki",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{Kind: "git", Repo: "test-wiki", Branch: "main"},
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "rig-emit"},
			},
			Agent: v1alpha1.AgentSpec{
				Model:    "litellm/zai/glm-5.2",
				Skill:    "/skills/wiki/SKILL.md",
				MaxFixes: 3,
				Gate:     v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "git-push"},
			},
		},
	}

	s := workflowTestServer(wf)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/test-wiki/graph", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	req.SetPathValue("name", "test-wiki")

	rr := httptest.NewRecorder()
	s.handleWorkflowGraphAPI(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp workflowGraphResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Workflow != "test-wiki" {
		t.Errorf("workflow = %q, want %q", resp.Workflow, "test-wiki")
	}

	// Should have 3 nodes: prepare, agent, deploy
	nodeTypes := map[string]string{}
	for _, n := range resp.Graph.Nodes {
		nodeTypes[n.ID] = n.Type
	}
	if len(resp.Graph.Nodes) != 3 {
		t.Errorf("expected 3 nodes, got %d: %+v", len(resp.Graph.Nodes), nodeTypes)
	}
	if nodeTypes["prepare"] != "plugin" {
		t.Errorf("prepare node type = %q, want %q", nodeTypes["prepare"], "plugin")
	}
	if nodeTypes["agent"] != "agent" {
		t.Errorf("agent node type = %q, want %q", nodeTypes["agent"], "agent")
	}
	if nodeTypes["deploy"] != "plugin" {
		t.Errorf("deploy node type = %q, want %q", nodeTypes["deploy"], "plugin")
	}

	// Should have 2 edges: prepare→agent, agent→deploy
	if len(resp.Graph.Edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(resp.Graph.Edges))
	}
}

// TestHandleWorkflowGraphAPI_OwnershipCheck verifies that a user cannot
// access another user's workflow graph.
func TestHandleWorkflowGraphAPI_OwnershipCheck(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "secret-wiki",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{Kind: "git"},
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "rig-emit"},
			},
			Agent: v1alpha1.AgentSpec{
				Enabled: &[]bool{false}[0],
				Model:   "test",
				Skill:   "test",
				Gate:    v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "noop"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "git-push"},
			},
		},
	}

	s := workflowTestServer(wf)

	// Bob tries to access Alice's workflow → 404
	req := httptest.NewRequest(http.MethodGet, "/api/workflows/secret-wiki/graph", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "bob"}))
	req.SetPathValue("name", "secret-wiki")

	rr := httptest.NewRecorder()
	s.handleWorkflowGraphAPI(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for other user's workflow, got %d", rr.Code)
	}
}

// TestHandleWorkflowGraphAPI_AgentDisabled verifies that when the agent is
// disabled, the compiled graph has prepare → deploy (2 nodes, 1 edge).
func TestHandleWorkflowGraphAPI_AgentDisabled(t *testing.T) {
	agentDisabled := false
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "deterministic-only",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{Kind: "git"},
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "rig-emit"},
			},
			Agent: v1alpha1.AgentSpec{
				Enabled: &agentDisabled,
				Model:   "test",
				Skill:   "test",
				Gate:    v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "noop"}},
			},
			Deploy: v1alpha1.DeploySpec{
				Plugin: v1alpha1.PluginRef{Name: "git-push"},
			},
		},
	}

	s := workflowTestServer(wf)

	req := httptest.NewRequest(http.MethodGet, "/api/workflows/deterministic-only/graph", nil)
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	req.SetPathValue("name", "deterministic-only")

	rr := httptest.NewRecorder()
	s.handleWorkflowGraphAPI(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp workflowGraphResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Agent disabled → 2 nodes (prepare, deploy), 1 edge (prepare→deploy)
	if len(resp.Graph.Nodes) != 2 {
		t.Errorf("expected 2 nodes (agent disabled), got %d", len(resp.Graph.Nodes))
	}
	if len(resp.Graph.Edges) != 1 {
		t.Errorf("expected 1 edge (prepare→deploy), got %d", len(resp.Graph.Edges))
	}
}
