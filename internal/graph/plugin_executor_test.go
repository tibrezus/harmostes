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

func TestPluginExecutorGreen(t *testing.T) {
	// Create a script that exits 0 and prints JSON result
	dir := t.TempDir()
	script := filepath.Join(dir, "success.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"artifact":"out.txt","changed":true}'
`), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "checkout",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "checkout"}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["artifact"] != "out.txt" {
		t.Errorf("artifact = %v, want out.txt", result.Outputs["artifact"])
	}
	if changed, ok := result.Outputs["changed"].(bool); !ok || !changed {
		t.Errorf("changed = %v, want true", result.Outputs["changed"])
	}
}

func TestPluginExecutorFailed(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo "error: something went wrong"
exit 1
`), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "deploy",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "deploy"}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	// Non-zero exit is a node failure, NOT a system error
	if err != nil {
		t.Fatalf("Execute should not return error for non-zero exit: %v", err)
	}
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if result.Feedback == "" {
		t.Error("feedback should contain stderr/stdout")
	}
}

func TestPluginExecutorResolveError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("plugin not found")}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "missing",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "missing"}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestPluginExecutorBadConfig(t *testing.T) {
	exec := NewPluginExecutor(&fakeResolver{})

	node := v1alpha1.NodeSpec{
		ID:     "bad",
		Type:   "plugin",
		Config: json.RawMessage(`{invalid json`),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected config parse error")
	}
}
