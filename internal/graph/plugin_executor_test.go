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
	"time"

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
	// The path lives in a git submodule: without it vendored the resolver's
	// fallback finds an unrelated host script and the fixture is meaningless.
	if _, err := os.Stat("/plugins/fork-maintenance-plugins/fork-sync.sh"); err != nil {
		t.Skip("plugin scripts not vendored (.agents submodule missing)")
	}
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

// TestPluginTailCarriesArtifact pins the #685 r10 recovery surface: the
// deploy verdict's truncation marker promises 'full text in the run
// record' — without the artifact field on the persisted plugin.tail
// event, a green deploy retained the artifact nowhere (logs die with the
// pod; the onDeployed pubsub event has no persisting subscriber).
func TestPluginTailCarriesArtifact(t *testing.T) {
	dir := t.TempDir()
	script := dir + "/p.sh"
	art := `{"artifact":"pr-github.com-o/r-99","status":"ok","event":{"decision":"REQUEST_CHANGES","inline_threads":{"carried":[{"path":"x.md","body":"FIX: the tail the marker promises must be recoverable"}]}}}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho working\necho '"+art+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tl := &fakeTL{}
	e := NewPluginExecutor(scriptResolver{script})
	e.tl = tl
	res, err := e.Execute(t.Context(), v1alpha1.NodeSpec{ID: "deploy", Type: "plugin", Config: []byte(`{"name":"post-review"}`)}, NodeEnv{Workdir: dir})
	if err != nil || res.Status != StatusGreen {
		t.Fatalf("execute: %v %s", err, res.Status)
	}
	var got string
	for _, it := range tl.items {
		if it.Kind == "plugin.tail" {
			got, _ = it.Payload["artifact"].(string)
		}
	}
	if got == "" {
		t.Fatal("plugin.tail carries no artifact — the truncation marker's 'full text in the run record' points at nothing")
	}
	if !strings.Contains(got, "FIX: the tail the marker promises must be recoverable") {
		t.Fatalf("the artifact field must carry the FULL structured output (carried bodies included), got %s", got)
	}
}

// TestPluginNodeTimeoutAttribution (#695 r2 4301176018): a REAL plugin node
// killed by its G8 bound must come back with the timeout attribution in the
// feedback — PluginExecutor returns StatusFailed+nil-error for a killed run,
// so an error-keyed check never fires and the operator sees the output tail
// ("starting"), indistinguishable from a script that died on its own.
func TestPluginNodeTimeoutAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	resolver := &fakeResolver{command: "/bin/sh", args: []string{"-c", "echo starting; sleep 30"}}
	registry := NewRegistry()
	registry.Register(NewPluginExecutor(resolver))
	exec := NewGraphExecutor(registry, nil)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "prepare", Type: "plugin", Timeout: "1s", Config: mustJSON(t, PluginNodeConfig{Name: "workspace"})},
		},
	}
	start := time.Now()
	runResult, err := exec.Execute(context.Background(), graph, "test-prepare-bound")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("node returned after %s — the bound did not bound", elapsed)
	}
	result := runResult.NodeResults["prepare"]
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Feedback, "timed out after 1s") {
		t.Fatalf("feedback %q lacks the timeout attribution — a circuit-breaker trip reads like a script bug (the #695 r2 4301176018 probe)", result.Feedback)
	}
}

// TestPluginConfigTimeoutBoundsGraphNativeNode (#695 r2 4301215249): a
// graph-native plugin node with ONLY a config timeout (no NodeSpec.Timeout)
// must be bounded by it.
func TestPluginConfigTimeoutBoundsGraphNativeNode(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	resolver := &fakeResolver{command: "/bin/sh", args: []string{"-c", "echo starting; sleep 30"}}
	exec := NewPluginExecutor(resolver)

	node := v1alpha1.NodeSpec{
		ID:     "p",
		Type:   "plugin",
		Config: mustJSON(t, PluginNodeConfig{Name: "p", Timeout: "1s"}),
	}
	start := time.Now()
	result, _ := exec.Execute(context.Background(), node, NodeEnv{})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("config.timeout not enforced — still running after %s", elapsed)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}
