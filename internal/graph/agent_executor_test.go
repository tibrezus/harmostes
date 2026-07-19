package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
)

func TestAgentExecutorGreenNoGate(t *testing.T) {
	runner := &fakeAgentRunner{result: agent.Result{Green: true, Attempts: 1}}
	exec := NewAgentExecutor(runner, nil, nil)

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
	exec := NewAgentExecutor(runner, nil, nil)

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
}

func TestAgentExecutorRunnerError(t *testing.T) {
	runner := &fakeAgentRunner{err: errors.New("pi session failed to start")}
	exec := NewAgentExecutor(runner, nil, nil)

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
	exec := NewAgentExecutor(runner, tasks, nil)

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
	exec := NewAgentExecutor(runner, tasks, nil)

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
	exec := NewAgentExecutor(runner, nil, nil) // no resolver

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
	exec := NewAgentExecutor(&fakeAgentRunner{}, nil, nil)

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
	exec := NewAgentExecutor(runner, nil, nil)

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
