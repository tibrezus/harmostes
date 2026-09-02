package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestPluginExecutorGreen(t *testing.T) {
	// Create a script that exits 0 and prints JSON result
	dir := t.TempDir()
	script := filepath.Join(dir, "success.sh")
	scriptBody := "#!/bin/sh\necho '{\"status\":\"ok\",\"artifact\":\"out.txt\",\"changed\":true}'\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
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

func TestPluginExecutorChangedFalseReturnsSkipped(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "noop.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"changed":false,"event":{"rig_hash":"abc123"}}'
`), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "prepare",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "prepare"}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusSkipped {
		t.Errorf("status = %q, want skipped", result.Status)
	}
	// rig_hash should be exposed in outputs even on skip
	if result.Outputs["rig_hash"] != "abc123" {
		t.Errorf("rig_hash = %v, want abc123", result.Outputs["rig_hash"])
	}
}

func TestPluginExecutorExposesEventFields(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "emit.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo '{"changed":true,"artifact":"rig.json","event":{"rig_hash":"sha256:xyz","components":42}}'
`), 0755); err != nil {
		t.Fatal(err)
	}

	resolver := &fakeResolver{command: "/bin/sh", args: []string{script}}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "prepare",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "rig-emit"}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["rig_hash"] != "sha256:xyz" {
		t.Errorf("rig_hash = %v, want sha256:xyz", result.Outputs["rig_hash"])
	}
	if components, ok := result.Outputs["components"].(float64); !ok || components != 42 {
		t.Errorf("components = %v, want 42", result.Outputs["components"])
	}
}

// fakeTL captures emissions; fakeResolver serves a script whose output
// carries a credential URL — proving redaction happens at the seam.
type fakeTL struct{ items []captureTLItem }

func (f *fakeTL) Emit(_ context.Context, kind, node string, payload any) error {
	m, _ := payload.(map[string]any)
	f.items = append(f.items, captureTLItem{Kind: kind, Node: node, Payload: m})
	return nil
}

type scriptResolver struct{ command string }

func (r scriptResolver) Resolve(_ context.Context, _ v1alpha1.PluginRef, _ string) (string, []string, error) {
	return r.command, nil, nil
}

func TestPluginTailEmissionRedacts(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/p.sh"
	scriptBody := "#!/bin/sh\necho line1\necho cloning https://user:secret@host/x.git\necho last\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	tl := &fakeTL{}
	e := NewPluginExecutor(scriptResolver{script})
	e.tl = tl
	res, err := e.Execute(t.Context(), v1alpha1.NodeSpec{ID: "n", Type: "plugin", Config: []byte(`{"name":"p"}`)}, NodeEnv{Workdir: dir})
	if err != nil || res.Status != StatusGreen {
		t.Fatalf("execute: %v %s", err, res.Status)
	}
	var tail []string
	for _, it := range tl.items {
		if it.Kind == "plugin.tail" {
			tail, _ = it.Payload["tail"].([]string)
		}
	}
	if len(tail) == 0 {
		t.Fatal("no plugin.tail emitted")
	}
	for _, l := range tail {
		if strings.Contains(fmt.Sprint(l), "secret") {
			t.Fatalf("credential leaked into tail: %v", l)
		}
	}
}

// An exec that never started (missing script, permission denied) produces no
// output at all — the run error must become the feedback, or the failure is
// invisible: "prepare failed: " with nothing on the wall (#311's live shape).
func TestPluginExecutorExecFailureSurfacesError(t *testing.T) {
	// The resolver returns a path that does not exist — the live #311 shape.
	resolver := &fakeResolver{command: "/plugins/fork-maintenance-plugins/fork-sync.sh"}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "prepare",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "fork-sync"}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if strings.TrimSpace(result.Feedback) == "" {
		t.Fatal("exec failure with no plugin output must carry the run error as feedback")
	}
	if !strings.Contains(result.Feedback, "fork-sync.sh") {
		t.Errorf("feedback should name the failing executable, got: %s", result.Feedback)
	}
	if len(result.Feedback) > 512 {
		t.Errorf("feedback must be truncated, got %d bytes", len(result.Feedback))
	}
}
