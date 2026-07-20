package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Recording executor — mock for integration tests
// ---------------------------------------------------------------------------

// recordingExecutor records every execution and returns a configured result.
type recordingExecutor struct {
	mu       sync.Mutex
	visits   []string
	result   NodeResult
	execType string
}

func newRecording(typ string, result NodeResult) *recordingExecutor {
	return &recordingExecutor{execType: typ, result: result}
}

func (r *recordingExecutor) Execute(_ context.Context, node v1alpha1.NodeSpec, _ NodeEnv) (NodeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visits = append(r.visits, node.ID)
	return r.result, nil
}

func (r *recordingExecutor) Type() string        { return r.execType }
func (r *recordingExecutor) Deterministic() bool { return true }

func (r *recordingExecutor) visitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.visits)
}

func (r *recordingExecutor) visitList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.visits))
	copy(cp, r.visits)
	return cp
}

// registryWith builds a registry from a map of type→executor.
func registryWith(execs map[string]NodeExecutor) *Registry {
	r := NewRegistry()
	for _, e := range execs {
		r.Register(e)
	}
	return r
}

// ===========================================================================
// Linear graph: A → B → C
// ===========================================================================

func TestExecuteLinearGraph(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen, Outputs: NodeOutputs{"v": "a"}})
	execB := newRecording("typeB", NodeResult{Status: StatusGreen, Outputs: NodeOutputs{"v": "b"}})
	execC := newRecording("typeC", NodeResult{Status: StatusGreen, Outputs: NodeOutputs{"v": "c"}})

	// Override "plugin" type per executor is not possible (one type = one exec),
	// so use distinct types.
	registry := registryWith(map[string]NodeExecutor{
		"typeA": execA, "typeB": execB, "typeC": execC,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "typeA"},
			{ID: "B", Type: "typeB"},
			{ID: "C", Type: "typeC"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B"},
			{From: "B", To: "C"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, err := exec.Execute(context.Background(), graph, "test-pipeline")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if len(result.NodeResults) != 3 {
		t.Errorf("nodeResults = %d, want 3", len(result.NodeResults))
	}
	// Each node visited exactly once.
	for _, e := range []*recordingExecutor{execA, execB, execC} {
		if e.visitCount() != 1 {
			t.Errorf("%s visited %d times, want 1", e.Type(), e.visitCount())
		}
	}
}

// ===========================================================================
// Conditional edges: green → deploy, failed → alert
// ===========================================================================

func TestExecuteConditionalEdgeGreen(t *testing.T) {
	gateExec := newRecording("gate", NodeResult{Status: StatusGreen})
	deployExec := newRecording("deploy", NodeResult{Status: StatusGreen})
	alertExec := newRecording("alert", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"gate": gateExec, "deploy": deployExec, "alert": alertExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "gate", Type: "gate"},
			{ID: "deploy", Type: "deploy"},
			{ID: "alert", Type: "alert"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "gate", To: "deploy", When: "green"},
			{From: "gate", To: "alert", When: "failed"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, err := exec.Execute(context.Background(), graph, "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if deployExec.visitCount() != 1 {
		t.Errorf("deploy should be visited once, got %d", deployExec.visitCount())
	}
	if alertExec.visitCount() != 0 {
		t.Errorf("alert should NOT be visited, got %d", alertExec.visitCount())
	}
}

func TestExecuteConditionalEdgeFailed(t *testing.T) {
	gateExec := newRecording("gate", NodeResult{Status: StatusFailed, Feedback: "lint error"})
	deployExec := newRecording("deploy", NodeResult{Status: StatusGreen})
	alertExec := newRecording("alert", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"gate": gateExec, "deploy": deployExec, "alert": alertExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "gate", Type: "gate"},
			{ID: "deploy", Type: "deploy"},
			{ID: "alert", Type: "alert"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "gate", To: "deploy", When: "green"},
			{From: "gate", To: "alert", When: "failed"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, _ := exec.Execute(context.Background(), graph, "test")
	// Gate failed, but alert handles the failure → pipeline status depends on
	// whether alert succeeded. Alert is green, so overall pipeline is green
	// (the failure was handled).
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green (failure handled by alert)", result.Status)
	}
	if deployExec.visitCount() != 0 {
		t.Errorf("deploy should NOT be visited on gate failure, got %d", deployExec.visitCount())
	}
	if alertExec.visitCount() != 1 {
		t.Errorf("alert should be visited once, got %d", alertExec.visitCount())
	}
}

// ===========================================================================
// Branch edge conditions: changed/unchanged
// ===========================================================================

func TestExecuteBranchChanged(t *testing.T) {
	branchExec := newRecording("branch", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"changed": true},
	})
	workExec := newRecording("work", NodeResult{Status: StatusGreen})
	skipExec := newRecording("skip", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"branch": branchExec, "work": workExec, "skip": skipExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "check", Type: "branch"},
			{ID: "work", Type: "work"},
			{ID: "skip", Type: "skip"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "check", To: "work", When: "changed"},
			{From: "check", To: "skip", When: "unchanged"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, _ := exec.Execute(context.Background(), graph, "test")
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if workExec.visitCount() != 1 {
		t.Errorf("work should be visited when changed=true, got %d", workExec.visitCount())
	}
	if skipExec.visitCount() != 0 {
		t.Errorf("skip should NOT be visited when changed=true, got %d", skipExec.visitCount())
	}
}

func TestExecuteBranchUnchanged(t *testing.T) {
	branchExec := newRecording("branch", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"changed": false},
	})
	workExec := newRecording("work", NodeResult{Status: StatusGreen})
	skipExec := newRecording("skip", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"branch": branchExec, "work": workExec, "skip": skipExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "check", Type: "branch"},
			{ID: "work", Type: "work"},
			{ID: "skip", Type: "skip"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "check", To: "work", When: "changed"},
			{From: "check", To: "skip", When: "unchanged"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, _ := exec.Execute(context.Background(), graph, "test")
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if workExec.visitCount() != 0 {
		t.Errorf("work should NOT be visited when changed=false, got %d", workExec.visitCount())
	}
	if skipExec.visitCount() != 1 {
		t.Errorf("skip should be visited when changed=false, got %d", skipExec.visitCount())
	}
}

// ===========================================================================
// Loop-back with maxRetries (gate feedback pattern)
// ===========================================================================

func TestExecuteLoopBackMaxRetries(t *testing.T) {
	// Gate fails every time. Loop-back edge has maxRetries=2.
	// Expected: agent visited 3 times (initial + 2 retries), gate visited 3 times.
	// After maxRetries exceeded, pipeline fails.
	agentExec := newRecording("agent", NodeResult{Status: StatusGreen})
	gateExec := newRecording("gate", NodeResult{Status: StatusFailed, Feedback: "lint error"})
	deployExec := newRecording("deploy", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"agent": agentExec, "gate": gateExec, "deploy": deployExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			{ID: "gate", Type: "gate"},
			{ID: "deploy", Type: "deploy"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "agent", To: "gate"},
			{From: "gate", To: "deploy", When: "green"},
			{From: "gate", To: "agent", When: "failed", MaxRetries: 2},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, _ := exec.Execute(context.Background(), graph, "test")
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (maxRetries exceeded)", result.Status)
	}
	// Agent: initial visit + 2 loop-back visits = 3
	if agentExec.visitCount() != 3 {
		t.Errorf("agent visited %d times, want 3 (1 + 2 retries)", agentExec.visitCount())
	}
	// Gate: visited after each agent execution = 3
	if gateExec.visitCount() != 3 {
		t.Errorf("gate visited %d times, want 3", gateExec.visitCount())
	}
	// Deploy: never reached (gate never passed)
	if deployExec.visitCount() != 0 {
		t.Errorf("deploy visited %d times, want 0", deployExec.visitCount())
	}
}

func TestExecuteLoopBackSuccess(t *testing.T) {
	// Gate fails twice then passes. maxRetries=3.
	// Expected: agent visited 3 times, gate visited 3 times, deploy visited once.
	agentExec := &flippingExecutor{
		execType: "agent",
		results: []NodeResult{
			{Status: StatusGreen},
			{Status: StatusGreen},
			{Status: StatusGreen},
		},
	}
	gateExec := &flippingExecutor{
		execType: "gate",
		results: []NodeResult{
			{Status: StatusFailed, Feedback: "fail 1"},
			{Status: StatusFailed, Feedback: "fail 2"},
			{Status: StatusGreen}, // 3rd pass
		},
	}
	deployExec := newRecording("deploy", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"agent": agentExec, "gate": gateExec, "deploy": deployExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			{ID: "gate", Type: "gate"},
			{ID: "deploy", Type: "deploy"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "agent", To: "gate"},
			{From: "gate", To: "deploy", When: "green"},
			{From: "gate", To: "agent", When: "failed", MaxRetries: 3},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, err := exec.Execute(context.Background(), graph, "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if deployExec.visitCount() != 1 {
		t.Errorf("deploy visited %d times, want 1", deployExec.visitCount())
	}
}

// flippingExecutor returns different results on successive calls.
type flippingExecutor struct {
	mu       sync.Mutex
	execType string
	results  []NodeResult
	calls    int
}

func (f *flippingExecutor) Execute(_ context.Context, node v1alpha1.NodeSpec, _ NodeEnv) (NodeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.calls
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	f.calls++
	return f.results[idx], nil
}

func (f *flippingExecutor) Type() string        { return f.execType }
func (f *flippingExecutor) Deterministic() bool { return true }

// ===========================================================================
// Template edge condition
// ===========================================================================

func TestExecuteTemplateEdgeCondition(t *testing.T) {
	checkExec := newRecording("check", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"score": "85"},
	})
	highExec := newRecording("high", NodeResult{Status: StatusGreen})
	lowExec := newRecording("low", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"check": checkExec, "high": highExec, "low": lowExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "check", Type: "check"},
			{ID: "high", Type: "high"},
			{ID: "low", Type: "low"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "check", To: "high", When: `{{ gt (index (index .Nodes "check") "score") "80" }}`},
			{From: "check", To: "low", When: `{{ not (gt (index (index .Nodes "check") "score") "80") }}`},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, _ := exec.Execute(context.Background(), graph, "test")
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if highExec.visitCount() != 1 {
		t.Errorf("high should be visited (score=85 > 80), got %d", highExec.visitCount())
	}
	if lowExec.visitCount() != 0 {
		t.Errorf("low should NOT be visited, got %d", lowExec.visitCount())
	}
}

// ===========================================================================
// No entry nodes (pure cycle)
// ===========================================================================

func TestExecuteNoEntryNodes(t *testing.T) {
	exec := newRecording("plugin", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"plugin": exec})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "plugin"},
			{ID: "B", Type: "plugin"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B"},
			{From: "B", To: "A"},
		},
	}

	ge := NewGraphExecutor(registry, nil)
	_, err := ge.Execute(context.Background(), graph, "test")
	if err == nil {
		t.Fatal("expected error for cyclic graph with no entry")
	}
}

// ===========================================================================
// Node failure with no handler → pipeline fails
// ===========================================================================

func TestExecuteNodeFailureNoHandler(t *testing.T) {
	failExec := newRecording("plugin", NodeResult{Status: StatusFailed, Feedback: "crashed"})
	registry := registryWith(map[string]NodeExecutor{"plugin": failExec})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "plugin"},
			{ID: "B", Type: "plugin"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B", When: "green"}, // A failed → B not reached
		},
	}

	ge := NewGraphExecutor(registry, nil)
	result, _ := ge.Execute(context.Background(), graph, "test")
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if failExec.visitCount() != 1 {
		t.Errorf("A visited %d times, want 1", failExec.visitCount())
	}
}

// ===========================================================================
// Checkpointing + lifecycle events (with fake Dapr client)
// ===========================================================================

func TestExecuteCheckpointAndEvents(t *testing.T) {
	client := newFakeDaprClient()
	exec := newRecording("plugin", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"artifact": "result"},
	})
	registry := registryWith(map[string]NodeExecutor{"plugin": exec})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "plugin"},
			{ID: "B", Type: "plugin"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B"},
		},
	}

	ge := NewGraphExecutor(registry, client)
	result, err := ge.Execute(context.Background(), graph, "test-pipeline")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}

	// Verify checkpoints: pipeline/test-pipeline/nodes/A and .../B
	keyA := "pipeline/test-pipeline/nodes/A"
	keyB := "pipeline/test-pipeline/nodes/B"
	if _, ok := client.state[DefaultStateStore+"/"+keyA]; !ok {
		t.Errorf("checkpoint for A not found")
	}
	if _, ok := client.state[DefaultStateStore+"/"+keyB]; !ok {
		t.Errorf("checkpoint for B not found")
	}

	// Verify lifecycle events: pipeline.started, node.started x2, node.completed x2, pipeline.completed
	events := make([]string, 0, len(client.published))
	for _, msg := range client.published {
		var payload map[string]any
		if json.Unmarshal([]byte(msg.Payload), &payload) == nil {
			if evt, ok := payload["event"].(string); ok {
				events = append(events, evt)
			}
		}
	}
	hasEvent := func(name string) bool {
		for _, e := range events {
			if e == name {
				return true
			}
		}
		return false
	}
	expected := []string{"pipeline.started", "node.started", "node.completed", "pipeline.completed"}
	for _, e := range expected {
		if !hasEvent(e) {
			t.Errorf("lifecycle event %q not published. Events: %v", e, events)
		}
	}
}

// ===========================================================================
// Unknown node type
// ===========================================================================

func TestExecuteUnknownNodeType(t *testing.T) {
	registry := NewRegistry() // empty

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "nonexistent"},
		},
	}

	ge := NewGraphExecutor(registry, nil)
	result, _ := ge.Execute(context.Background(), graph, "test")
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
}

// ===========================================================================
// Input resolution: downstream node receives upstream outputs
// ===========================================================================

func TestExecuteInputResolution(t *testing.T) {
	// A produces output "sha" = "abc123". B checks that it received A's output.
	producerExec := newRecording("producer", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"sha": "abc123"},
	})
	consumerExec := &inputCheckingExecutor{
		execType: "consumer",
		wantInputs: map[string]NodeOutputs{
			"A": {"sha": "abc123"},
		},
	}
	registry := registryWith(map[string]NodeExecutor{
		"producer": producerExec, "consumer": consumerExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "producer"},
			{ID: "B", Type: "consumer"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B"},
		},
	}

	ge := NewGraphExecutor(registry, nil)
	result, err := ge.Execute(context.Background(), graph, "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if !consumerExec.gotExpectedInputs {
		t.Errorf("consumer did not receive expected inputs")
	}
}

type inputCheckingExecutor struct {
	execType          string
	wantInputs        map[string]NodeOutputs
	gotExpectedInputs bool
}

func (i *inputCheckingExecutor) Execute(_ context.Context, _ v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	i.gotExpectedInputs = true
	for nodeID, wantOutputs := range i.wantInputs {
		gotOutputs, ok := env.Inputs[nodeID]
		if !ok {
			i.gotExpectedInputs = false
			return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("missing input from node %s", nodeID)}, nil
		}
		for k, wantV := range wantOutputs {
			if gotOutputs[k] != wantV {
				i.gotExpectedInputs = false
				return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("input %s.%s = %v, want %v", nodeID, k, gotOutputs[k], wantV)}, nil
			}
		}
	}
	return NodeResult{Status: StatusGreen}, nil
}

func (i *inputCheckingExecutor) Type() string        { return i.execType }
func (i *inputCheckingExecutor) Deterministic() bool { return true }

// ===========================================================================
// ExecuteGraph convenience function
// ===========================================================================

func TestExecuteGraphConvenience(t *testing.T) {
	deps := Dependencies{
		DaprClient: newFakeDaprClient(),
	}
	registry := NewDefaultRegistry(deps)
	exec := NewGraphExecutor(registry, deps.DaprClient)

	// Use a branch node to test the default registry integration.
	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{
				ID:     "check",
				Type:   "branch",
				Config: json.RawMessage(`{"condition":"true"}`),
			},
		},
	}

	result, err := exec.Execute(context.Background(), graph, "conv-test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
}

// ===========================================================================
// CompileWorkflow
// ===========================================================================

func TestCompileWorkflowWithAgent(t *testing.T) {
	enabled := true
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "rig-emit"}},
			Agent: v1alpha1.AgentSpec{
				Enabled: &enabled,
				Model:   "zai/glm-5.2",
				Skill:   "llm-wiki",
				TaskTemplate: v1alpha1.TaskTemplate{
					Name: "wiki-update",
				},
				Gate:     v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
				MaxFixes: 3,
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "git-push"}},
		},
	}

	graph := CompileWorkflow(wf)

	if len(graph.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(graph.Nodes))
	}
	if graph.Nodes[0].ID != "prepare" || graph.Nodes[0].Type != "plugin" {
		t.Errorf("node[0] = %+v, want prepare/plugin", graph.Nodes[0])
	}
	if graph.Nodes[1].ID != "agent" || graph.Nodes[1].Type != "agent" {
		t.Errorf("node[1] = %+v, want agent/agent", graph.Nodes[1])
	}
	if graph.Nodes[2].ID != "deploy" || graph.Nodes[2].Type != "plugin" {
		t.Errorf("node[2] = %+v, want deploy/plugin", graph.Nodes[2])
	}
	if len(graph.Edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(graph.Edges))
	}
	if graph.Edges[0].From != "prepare" || graph.Edges[0].To != "agent" {
		t.Errorf("edge[0] = %+v, want prepare→agent", graph.Edges[0])
	}
	if graph.Edges[1].From != "agent" || graph.Edges[1].To != "deploy" {
		t.Errorf("edge[1] = %+v, want agent→deploy", graph.Edges[1])
	}

	// Verify agent config has inline gate.
	var agentCfg AgentNodeConfig
	if err := json.Unmarshal(graph.Nodes[1].Config, &agentCfg); err != nil {
		t.Fatalf("unmarshal agent config: %v", err)
	}
	if agentCfg.Model != "zai/glm-5.2" {
		t.Errorf("agent model = %q, want zai/glm-5.2", agentCfg.Model)
	}
	if agentCfg.Gate == nil {
		t.Error("agent should have inline gate")
	}
}

func TestCompileWorkflowAgentDisabled(t *testing.T) {
	disabled := false
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "rig-emit"}},
			Agent: v1alpha1.AgentSpec{
				Enabled: &disabled,
				Model:   "zai/glm-5.2",
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "git-push"}},
		},
	}

	graph := CompileWorkflow(wf)

	if len(graph.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (prepare + deploy, no agent)", len(graph.Nodes))
	}
	if len(graph.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (prepare→deploy)", len(graph.Edges))
	}
	if graph.Edges[0].From != "prepare" || graph.Edges[0].To != "deploy" {
		t.Errorf("edge[0] = %+v, want prepare→deploy", graph.Edges[0])
	}
}

func TestCompileWorkflowMaxFixesDefault(t *testing.T) {
	wf := &v1alpha1.Workflow{
		Spec: v1alpha1.WorkflowSpec{
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "rig-emit"}},
			Agent: v1alpha1.AgentSpec{
				Model: "zai/glm-5.2",
				Gate:  v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
				// MaxFixes = 0 → should default to 3
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "git-push"}},
		},
	}

	graph := CompileWorkflow(wf)

	var agentCfg AgentNodeConfig
	json.Unmarshal(graph.Nodes[1].Config, &agentCfg)
	if agentCfg.MaxFixes != 3 {
		t.Errorf("maxFixes = %d, want 3 (default)", agentCfg.MaxFixes)
	}
}

// ===========================================================================
// getBoolOutput helper
// ===========================================================================

func TestGetBoolOutput(t *testing.T) {
	tests := []struct {
		name    string
		outputs NodeOutputs
		key     string
		want    bool
	}{
		{"bool_true", NodeOutputs{"changed": true}, "changed", true},
		{"bool_false", NodeOutputs{"changed": false}, "changed", false},
		{"string_true", NodeOutputs{"changed": "true"}, "changed", true},
		{"string_TRUE", NodeOutputs{"changed": "TRUE"}, "changed", true},
		{"string_false", NodeOutputs{"changed": "false"}, "changed", false},
		{"missing", NodeOutputs{}, "changed", false},
		{"wrong_type", NodeOutputs{"changed": 42}, "changed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getBoolOutput(tt.outputs, tt.key); got != tt.want {
				t.Errorf("getBoolOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ===========================================================================
// Integration: full gate feedback loop (end-to-end with mock nodes)
// ===========================================================================

func TestIntegrationGateFeedbackLoop(t *testing.T) {
	// Full pattern: prepare → agent → gate → [green] → deploy
	//                                → [failed] → agent (maxRetries: 2)
	// Gate fails twice, then passes.
	prepareExec := newRecording("prepare", NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"artifact": "rig.json", "changed": "true"},
	})
	agentExec := &flippingExecutor{
		execType: "agent",
		results: []NodeResult{
			{Status: StatusGreen, Outputs: NodeOutputs{"commit": "sha1"}},
			{Status: StatusGreen, Outputs: NodeOutputs{"commit": "sha2"}},
			{Status: StatusGreen, Outputs: NodeOutputs{"commit": "sha3"}},
		},
	}
	gateExec := &flippingExecutor{
		execType: "gate",
		results: []NodeResult{
			{Status: StatusFailed, Feedback: "lint: missing blank line"},
			{Status: StatusFailed, Feedback: "lint: trailing space"},
			{Status: StatusGreen}, // 3rd attempt passes
		},
	}
	deployExec := newRecording("deploy", NodeResult{Status: StatusGreen})

	registry := registryWith(map[string]NodeExecutor{
		"prepare": prepareExec, "agent": agentExec, "gate": gateExec, "deploy": deployExec,
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "prepare", Type: "prepare"},
			{ID: "agent", Type: "agent"},
			{ID: "gate", Type: "gate"},
			{ID: "deploy", Type: "deploy"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "prepare", To: "agent"},
			{From: "agent", To: "gate"},
			{From: "gate", To: "deploy", When: "green"},
			{From: "gate", To: "agent", When: "failed", MaxRetries: 2},
		},
	}

	client := newFakeDaprClient()
	ge := NewGraphExecutor(registry, client, WithLogger(func(format string, args ...any) {
		t.Logf(format, args...)
	}))
	result, err := ge.Execute(context.Background(), graph, "integration-test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}

	// Verify execution sequence: prepare(1) → agent(1) → gate(1,fail) → agent(2) → gate(2,fail) → agent(3) → gate(3,green) → deploy(1)
	if prepareExec.visitCount() != 1 {
		t.Errorf("prepare visited %d, want 1", prepareExec.visitCount())
	}
	if deployExec.visitCount() != 1 {
		t.Errorf("deploy visited %d, want 1", deployExec.visitCount())
	}

	// Verify checkpoints exist for all nodes.
	for _, nodeID := range []string{"prepare", "agent", "gate", "deploy"} {
		key := fmt.Sprintf("pipeline/integration-test/nodes/%s", nodeID)
		if _, ok := client.state[DefaultStateStore+"/"+key]; !ok {
			t.Errorf("checkpoint for %s not found", nodeID)
		}
	}
}
