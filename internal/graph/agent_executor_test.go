package graph

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
)

func TestAgentExecutorGreenNoGate(t *testing.T) {
	runner := &fakeAgentRunner{result: agent.Result{Green: true, Attempts: 1}}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Model: "zai/glm-5.2",
			Skill: "llm-wiki",
			Task:  "Update the wiki page for X",
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
}

func TestAgentExecutorFailed(t *testing.T) {
	runner := &fakeAgentRunner{result: agent.Result{Green: false, Attempts: 3}}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Model:    "zai/glm-5.2",
			Skill:    "llm-wiki",
			Task:     "Update the wiki page for X",
			MaxFixes: 3,
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if attempts, ok := result.Outputs["attempts"].(int); !ok || attempts != 3 {
		t.Errorf("attempts = %v, want 3", result.Outputs["attempts"])
	}
	if model, ok := result.Outputs["model"].(string); !ok || model != "zai/glm-5.2" {
		t.Errorf("model = %v, want zai/glm-5.2", result.Outputs["model"])
	}
	turns, ok := result.Outputs["turns"].(int)
	if !ok {
		t.Errorf("turns = %v, want int", result.Outputs["turns"])
	} else if turns < 0 {
		t.Errorf("turns = %d, want non-negative", turns)
	}
}

func TestAgentExecutorRunnerError(t *testing.T) {
	runner := &fakeAgentRunner{err: errors.New("pi session failed to start")}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Task: "do something",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected runner error")
	}
}

func TestAgentExecutorTaskRef(t *testing.T) {
	runner := &fakeAgentRunner{result: agent.Result{Green: true, Attempts: 1}}
	tasks := &fakeTaskResolver{text: "resolved task content"}
	exec := NewAgentExecutor(runner, tasks, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Task: "tasks/wiki-update",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestAgentExecutorTaskRefError(t *testing.T) {
	runner := &fakeAgentRunner{}
	tasks := &fakeTaskResolver{err: errors.New("configmap not found")}
	exec := NewAgentExecutor(runner, tasks, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Task: "tasks/missing",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected task resolve error")
	}
}

func TestAgentExecutorInlineGateNoResolver(t *testing.T) {
	runner := &fakeAgentRunner{}
	exec := NewAgentExecutor(runner, nil, nil, nil, "") // no resolver

	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Task: "do something",
			Gate: &GateNodeConfig{
				Plugin: PluginNodeConfig{Name: "lint"},
			},
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected error: inline gate without resolver")
	}
}

func TestAgentExecutorBadConfig(t *testing.T) {
	exec := NewAgentExecutor(&fakeAgentRunner{}, nil, nil, nil, "")

	node := v1alpha1.NodeSpec{
		ID:     "bad",
		Type:   "agent",
		Config: json.RawMessage(`{invalid`),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected config parse error")
	}
}

func TestAgentExecutorMaxFixesDefault(t *testing.T) {
	runner := &fakeAgentRunner{result: agent.Result{Green: true, Attempts: 1}}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")

	// No maxFixes in config → should default to 1
	node := v1alpha1.NodeSpec{
		ID:   "writer",
		Type: "agent",
		Config: mustJSON(t, AgentNodeConfig{
			Task: "do something",
			// MaxFixes intentionally zero
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestLooksLikeRef(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"tasks/wiki-update", true},
		{"configmap:my-task", true},
		{"path/to/something", true},
		{"inline task text", false},
		{"", false},
		{"short", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := looksLikeRef(tt.input); got != tt.want {
				t.Errorf("looksLikeRef(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// capturingAgentRunner records the task text it was given.
type capturingAgentRunner struct{ task string }

func (f *capturingAgentRunner) Run(_ context.Context, task string, _ agent.Gate, _ int, _ agent.Logger, _ ...agent.TaskOption) (agent.Result, error) {
	f.task = task
	return agent.Result{Green: true}, nil
}

// TestAgentExecutorResumeNoteAppended (ADR-0010): a resumed lineage's run
// carries the delta note — do-not-redo + the new head.
func TestAgentExecutorResumeNoteAppended(t *testing.T) {
	t.Setenv("HARMOSTES_SESSION_RESUME", "1")
	t.Setenv("HARMOSTES_TRIGGER_SHA", "deadbeef123")
	runner := &capturingAgentRunner{}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")
	node := v1alpha1.NodeSpec{ID: "a", Type: "agent", Config: mustJSON(t, AgentNodeConfig{Task: "review it"})}
	if _, err := exec.Execute(context.Background(), node, NodeEnv{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(runner.task, "RESUMED session") || !strings.Contains(runner.task, "deadbeef123") {
		t.Fatalf("delta note missing: %q", runner.task)
	}
}

func TestAgentExecutorFreshRunHasNoResumeNote(t *testing.T) {
	t.Setenv("HARMOSTES_SESSION_RESUME", "")
	runner := &capturingAgentRunner{}
	exec := NewAgentExecutor(runner, nil, nil, nil, "")
	node := v1alpha1.NodeSpec{ID: "a", Type: "agent", Config: mustJSON(t, AgentNodeConfig{Task: "review it"})}
	if _, err := exec.Execute(context.Background(), node, NodeEnv{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(runner.task, "RESUMED session") {
		t.Fatalf("fresh run must not carry the resume note: %q", runner.task)
	}
}
