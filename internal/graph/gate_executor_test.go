package graph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestGateExecutorGreen(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pass.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'all good'\n"), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewGateExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:   "lint",
		Type: "gate",
		Config: mustJSON(t, GateNodeConfig{
			Plugin: PluginNodeConfig{Name: "lint"},
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if green, ok := result.Outputs["green"].(bool); !ok || !green {
		t.Errorf("outputs.green = %v, want true", result.Outputs["green"])
	}
}

func TestGateExecutorFailed(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'lint errors found'\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewGateExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:   "lint",
		Type: "gate",
		Config: mustJSON(t, GateNodeConfig{
			Plugin: PluginNodeConfig{Name: "lint"},
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("Execute should not return error for gate failure: %v", err)
	}
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if result.Feedback == "" {
		t.Error("feedback should contain gate stderr")
	}
}

func TestGateExecutorResolveError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("gate plugin not found")}
	exec := NewGateExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:   "missing",
		Type: "gate",
		Config: mustJSON(t, GateNodeConfig{
			Plugin: PluginNodeConfig{Name: "missing"},
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestGateExecutorBadConfig(t *testing.T) {
	exec := NewGateExecutor(&fakeResolver{})

	node := v1alpha1.NodeSpec{
		ID:     "bad",
		Type:   "gate",
		Config: json.RawMessage(`{invalid`),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected config parse error")
	}
}

func TestGateNodeConfigAsAgentGate(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	gateCfg := GateNodeConfig{Plugin: PluginNodeConfig{Name: "lint"}}

	gate, err := gateCfg.AsAgentGate(context.Background(), resolver, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("AsAgentGate: %v", err)
	}

	green, _, err := gate.Run(context.Background())
	if err != nil {
		t.Fatalf("gate.Run: %v", err)
	}
	if !green {
		t.Error("green = false, want true")
	}
}
