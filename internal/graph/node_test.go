package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
)

// --- Registry tests ---

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	exec := NewBranchExecutor()
	r.Register(exec)

	got, err := r.Get("branch")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type() != "branch" {
		t.Errorf("type = %q, want branch", got.Type())
	}
}

func TestRegistryGetUnknownType(t *testing.T) {
	r := NewRegistry()
	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for unregistered type")
	}
}

func TestRegistryTypes(t *testing.T) {
	r := NewRegistry()
	r.Register(NewBranchExecutor())
	r.Register(NewPluginExecutor(nil))

	types := r.Types()
	if len(types) != 2 {
		t.Fatalf("types = %v, want 2", types)
	}
	// Types() returns sorted
	if types[0] != "branch" || types[1] != "plugin" {
		t.Errorf("types = %v, want [branch plugin]", types)
	}
}

func TestRegistryHas(t *testing.T) {
	r := NewRegistry()
	r.Register(NewBranchExecutor())

	if !r.Has("branch") {
		t.Error("Has(branch) = false, want true")
	}
	if r.Has("plugin") {
		t.Error("Has(plugin) = true, want false")
	}
}

func TestRegistryReplaceOverwrites(t *testing.T) {
	r := NewRegistry()
	first := &fakeExecutor{typ: "test"}
	second := &fakeExecutor{typ: "test"}
	r.Register(first)
	r.Register(second)

	got, _ := r.Get("test")
	if got != second {
		t.Error("Register did not overwrite existing executor")
	}
}

// --- NewDefaultRegistry test ---

func TestNewDefaultRegistry(t *testing.T) {
	deps := Dependencies{
		PluginResolver: &fakeResolver{},
		AgentRunner:    &fakeAgentRunner{},
		TaskResolver:   &fakeTaskResolver{},
	}
	r := NewDefaultRegistry(deps)

	expected := []string{"agent", "branch", "gate", "plugin"}
	got := r.Types()
	if len(got) != len(expected) {
		t.Fatalf("types = %v, want %v", got, expected)
	}
	for i, typ := range expected {
		if got[i] != typ {
			t.Errorf("types[%d] = %q, want %q", i, got[i], typ)
		}
		if !r.Has(typ) {
			t.Errorf("Has(%q) = false", typ)
		}
	}
}

// --- Deterministic flag tests ---

func TestDeterministicFlags(t *testing.T) {
	tests := []struct {
		exec        NodeExecutor
		want        bool
		description string
	}{
		{NewPluginExecutor(&fakeResolver{}), true, "plugin"},
		{NewGateExecutor(&fakeResolver{}), true, "gate"},
		{NewBranchExecutor(), true, "branch"},
		{NewAgentExecutor(&fakeAgentRunner{}, nil, nil), false, "agent"},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			if got := tt.exec.Deterministic(); got != tt.want {
				t.Errorf("%s.Deterministic() = %v, want %v", tt.description, got, tt.want)
			}
		})
	}
}

// --- Type() method tests ---

func TestTypeMethods(t *testing.T) {
	tests := []struct {
		exec NodeExecutor
		want string
	}{
		{NewPluginExecutor(&fakeResolver{}), "plugin"},
		{NewGateExecutor(&fakeResolver{}), "gate"},
		{NewBranchExecutor(), "branch"},
		{NewAgentExecutor(&fakeAgentRunner{}, nil, nil), "agent"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.exec.Type(); got != tt.want {
				t.Errorf("Type() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Helpers ---

type fakeExecutor struct {
	typ string
}

func (f *fakeExecutor) Execute(_ context.Context, _ v1alpha1.NodeSpec, _ NodeEnv) (NodeResult, error) {
	return NodeResult{Status: StatusGreen}, nil
}
func (f *fakeExecutor) Type() string        { return f.typ }
func (f *fakeExecutor) Deterministic() bool { return true }

type fakeResolver struct {
	command string
	args    []string
	err     error
}

func (f *fakeResolver) Resolve(_ context.Context, _ v1alpha1.PluginRef, _ string) (string, []string, error) {
	if f.err != nil {
		return "", nil, f.err
	}
	cmd := f.command
	if cmd == "" {
		cmd = "/bin/true"
	}
	return cmd, f.args, nil
}

type fakeAgentRunner struct {
	result agent.Result
	err    error
}

func (f *fakeAgentRunner) Run(_ context.Context, _ string, _ agent.Gate, _ int, _ agent.Logger) (agent.Result, error) {
	return f.result, f.err
}

type fakeTaskResolver struct {
	text string
	err  error
}

func (f *fakeTaskResolver) Get(_ context.Context, _ string) (string, error) {
	return f.text, f.err
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNodeResultDefaults(t *testing.T) {
	r := NodeResult{}
	if r.Status != "" {
		t.Errorf("default status = %q, want empty", r.Status)
	}
	if r.Outputs != nil {
		t.Error("default outputs should be nil")
	}
}

func TestEnvToPluginEnv(t *testing.T) {
	env := NodeEnv{
		Workflow:     "test-wf",
		Namespace:    "test-ns",
		Workdir:      "/work",
		Source:       "abc123",
		SourceURL:    "https://example.com/repo",
		SourceBranch: "main",
		State:        "test-wf",
	}

	pe := envToPluginEnv(env, "plugin", `{"name":"test"}`)
	if pe.Workflow != "test-wf" {
		t.Errorf("workflow = %q", pe.Workflow)
	}
	if pe.Phase != "plugin" {
		t.Errorf("phase = %q, want plugin", pe.Phase)
	}
	if pe.Spec != `{"name":"test"}` {
		t.Errorf("spec = %q", pe.Spec)
	}
}

// Verify that errors propagate correctly through the registry
func TestRegistryErrorIncludesRegisteredTypes(t *testing.T) {
	r := NewRegistry()
	r.Register(NewBranchExecutor())

	_, err := r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	// Error message should list registered types for debugging
	if !contains(err.Error(), "branch") {
		t.Errorf("error should mention registered types, got: %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure fakeAgentRunner satisfies AgentRunner
var _ AgentRunner = (*fakeAgentRunner)(nil)

// Ensure fakeTaskResolver satisfies TaskResolver
var _ TaskResolver = (*fakeTaskResolver)(nil)

// Ensure fakeResolver satisfies worker.PluginResolver
var _ interface {
	Resolve(context.Context, v1alpha1.PluginRef, string) (string, []string, error)
} = (*fakeResolver)(nil)

// Ensure errors can be returned
var _ = errors.New
