package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
)

// --- Fake Dapr client ---

type fakeDaprClient struct {
	state     map[string]string // store/key → value
	published []publishedMsg
	getErr    error
	saveErr   error
	pubErr    error
}

type publishedMsg struct {
	Pubsub  string
	Topic   string
	Payload string
}

func newFakeDaprClient() *fakeDaprClient {
	return &fakeDaprClient{state: make(map[string]string)}
}

func (f *fakeDaprClient) stateKey(store, key string) string { return store + "/" + key }

func (f *fakeDaprClient) GetState(_ context.Context, store, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.state[f.stateKey(store, key)], nil
}

func (f *fakeDaprClient) SaveState(_ context.Context, store, key, value string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.state[f.stateKey(store, key)] = value
	return nil
}

func (f *fakeDaprClient) DeleteState(_ context.Context, store, key string) error {
	delete(f.state, f.stateKey(store, key))
	return nil
}

func (f *fakeDaprClient) Publish(_ context.Context, pubsub, topic, payload string) error {
	if f.pubErr != nil {
		return f.pubErr
	}
	f.published = append(f.published, publishedMsg{pubsub, topic, payload})
	return nil
}

// Compile-time check.
var _ dapr.Client = (*fakeDaprClient)(nil)

// =========================
// StateGetExecutor tests
// =========================

func TestStateGetExecutorSuccess(t *testing.T) {
	client := newFakeDaprClient()
	client.state["harmostes-state/rig-hash"] = "abc123"

	exec := NewStateGetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "get-hash",
		Type: "dapr-state-get",
		Config: mustJSON(t, DaprStateGetConfig{
			Store: "harmostes-state",
			Key:   "rig-hash",
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["value"] != "abc123" {
		t.Errorf("value = %v, want abc123", result.Outputs["value"])
	}
}

func TestStateGetExecutorMissingKey(t *testing.T) {
	client := newFakeDaprClient()

	exec := NewStateGetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "get-missing",
		Type: "dapr-state-get",
		Config: mustJSON(t, DaprStateGetConfig{
			Store: "harmostes-state",
			Key:   "nonexistent",
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("missing key should not be an error: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["value"] != "" {
		t.Errorf("value = %v, want empty string for missing key", result.Outputs["value"])
	}
}

func TestStateGetExecutorTemplateKey(t *testing.T) {
	client := newFakeDaprClient()
	client.state["harmostes-state/my-wf/rig-hash"] = "sha256"

	exec := NewStateGetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "get-hash",
		Type: "dapr-state-get",
		Config: mustJSON(t, DaprStateGetConfig{
			Store: "harmostes-state",
			Key:   "{{ index (index .Nodes \"prepare\") \"workflow\" }}/rig-hash",
		}),
	}

	env := NodeEnv{
		Inputs: map[string]NodeOutputs{
			"prepare": {"workflow": "my-wf"},
		},
	}

	// Wait — template renders to "my-wf/rig-hash", so the key lookup should be
	// "my-wf/rig-hash". But we stored "rig-hash/my-wf". Let me fix the template.
	// Actually, the template renders: {{ ... }} → "my-wf", then "/rig-hash" → "my-wf/rig-hash"
	result, _ := exec.Execute(context.Background(), node, env)
	if result.Outputs["value"] != "sha256" {
		t.Errorf("value = %v, want sha256 (template key should resolve)", result.Outputs["value"])
	}
}

func TestStateGetExecutorClientError(t *testing.T) {
	client := &fakeDaprClient{getErr: errors.New("connection refused")}

	exec := NewStateGetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "get-fail",
		Type: "dapr-state-get",
		Config: mustJSON(t, DaprStateGetConfig{
			Store: "harmostes-state",
			Key:   "any",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected client error")
	}
}

func TestStateGetExecutorNilClient(t *testing.T) {
	exec := NewStateGetExecutor(nil)

	node := v1alpha1.NodeSpec{
		ID:   "get-nodapr",
		Type: "dapr-state-get",
		Config: mustJSON(t, DaprStateGetConfig{
			Store: "harmostes-state",
			Key:   "any",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected error for nil Dapr client")
	}
}

func TestStateGetExecutorBadConfig(t *testing.T) {
	exec := NewStateGetExecutor(newFakeDaprClient())

	node := v1alpha1.NodeSpec{
		ID:     "bad",
		Type:   "dapr-state-get",
		Config: json.RawMessage(`{invalid`),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected config parse error")
	}
}

// =========================
// StateSetExecutor tests
// =========================

func TestStateSetExecutorSuccess(t *testing.T) {
	client := newFakeDaprClient()

	exec := NewStateSetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "set-hash",
		Type: "dapr-state-set",
		Config: mustJSON(t, DaprStateSetConfig{
			Store: "harmostes-state",
			Key:   "rig-hash",
			Value: "def456",
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if client.state["harmostes-state/rig-hash"] != "def456" {
		t.Errorf("state not written, got %q", client.state["harmostes-state/rig-hash"])
	}
}

func TestStateSetExecutorTemplate(t *testing.T) {
	client := newFakeDaprClient()

	exec := NewStateSetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "checkpoint",
		Type: "dapr-state-set",
		Config: mustJSON(t, DaprStateSetConfig{
			Store: "harmostes-state",
			Key:   "last-sha",
			Value: "{{ index (index .Nodes \"agent\") \"commitSha\" }}",
		}),
	}

	env := NodeEnv{
		Inputs: map[string]NodeOutputs{
			"agent": {"commitSha": "abc789"},
		},
	}

	result, err := exec.Execute(context.Background(), node, env)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if client.state["harmostes-state/last-sha"] != "abc789" {
		t.Errorf("value = %q, want abc789 (template should resolve)", client.state["harmostes-state/last-sha"])
	}
}

func TestStateSetExecutorClientError(t *testing.T) {
	client := &fakeDaprClient{saveErr: errors.New("state store unavailable")}

	exec := NewStateSetExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "set-fail",
		Type: "dapr-state-set",
		Config: mustJSON(t, DaprStateSetConfig{
			Store: "harmostes-state",
			Key:   "any",
			Value: "val",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected client error")
	}
}

func TestStateSetExecutorNilClient(t *testing.T) {
	exec := NewStateSetExecutor(nil)

	node := v1alpha1.NodeSpec{
		ID:   "set-nodapr",
		Type: "dapr-state-set",
		Config: mustJSON(t, DaprStateSetConfig{
			Store: "harmostes-state",
			Key:   "any",
			Value: "val",
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected error for nil Dapr client")
	}
}

// =========================
// PublishExecutor tests
// =========================

func TestPublishExecutorSuccess(t *testing.T) {
	client := newFakeDaprClient()

	exec := NewPublishExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "publish-event",
		Type: "dapr-publish",
		Config: mustJSON(t, DaprPublishConfig{
			Pubsub:  "harmostes-pubsub",
			Topic:   "pipeline.started",
			Payload: `{"workflow":"test"}`,
		}),
	}

	result, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if len(client.published) != 1 {
		t.Fatalf("published = %d msgs, want 1", len(client.published))
	}
	msg := client.published[0]
	if msg.Pubsub != "harmostes-pubsub" {
		t.Errorf("pubsub = %q, want harmostes-pubsub", msg.Pubsub)
	}
	if msg.Topic != "pipeline.started" {
		t.Errorf("topic = %q, want pipeline.started", msg.Topic)
	}
	if msg.Payload != `{"workflow":"test"}` {
		t.Errorf("payload = %q", msg.Payload)
	}
}

func TestPublishExecutorTemplatePayload(t *testing.T) {
	client := newFakeDaprClient()

	exec := NewPublishExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "publish-event",
		Type: "dapr-publish",
		Config: mustJSON(t, DaprPublishConfig{
			Pubsub:  "harmostes-pubsub",
			Topic:   "node.completed",
			Payload: `{"sha":"{{ index (index .Nodes "agent") "commitSha" }}"}`,
		}),
	}

	env := NodeEnv{
		Inputs: map[string]NodeOutputs{
			"agent": {"commitSha": "abc123"},
		},
	}

	_, err := exec.Execute(context.Background(), node, env)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(client.published) != 1 {
		t.Fatalf("published = %d, want 1", len(client.published))
	}
	expected := `{"sha":"abc123"}`
	if client.published[0].Payload != expected {
		t.Errorf("payload = %q, want %q", client.published[0].Payload, expected)
	}
}

func TestPublishExecutorClientError(t *testing.T) {
	client := &fakeDaprClient{pubErr: errors.New("topic not found")}

	exec := NewPublishExecutor(client)

	node := v1alpha1.NodeSpec{
		ID:   "publish-fail",
		Type: "dapr-publish",
		Config: mustJSON(t, DaprPublishConfig{
			Pubsub:  "harmostes-pubsub",
			Topic:   "any",
			Payload: `{}`,
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected client error")
	}
}

func TestPublishExecutorNilClient(t *testing.T) {
	exec := NewPublishExecutor(nil)

	node := v1alpha1.NodeSpec{
		ID:   "publish-nodapr",
		Type: "dapr-publish",
		Config: mustJSON(t, DaprPublishConfig{
			Pubsub:  "harmostes-pubsub",
			Topic:   "any",
			Payload: `{}`,
		}),
	}

	_, err := exec.Execute(context.Background(), node, NodeEnv{})
	if err == nil {
		t.Fatal("expected error for nil Dapr client")
	}
}

// =========================
// Registry integration
// =========================

func TestNewDefaultRegistryIncludesDaprTypes(t *testing.T) {
	deps := Dependencies{
		PluginResolver: &fakeResolver{},
		AgentRunner:    &fakeAgentRunner{},
		TaskResolver:   &fakeTaskResolver{},
		DaprClient:     newFakeDaprClient(),
	}
	r := NewDefaultRegistry(deps)

	expected := []string{
		"agent", "branch", "dapr-publish", "dapr-state-get", "dapr-state-set",
		"flux-reconcile", "gate", "plugin", "vela-app",
	}
	got := r.Types()
	if len(got) != len(expected) {
		t.Fatalf("types = %v, want %v", got, expected)
	}
	for _, typ := range expected {
		if !r.Has(typ) {
			t.Errorf("Has(%q) = false", typ)
		}
	}
}

func TestDaprExecutorsDeterministic(t *testing.T) {
	client := newFakeDaprClient()
	tests := []struct {
		exec NodeExecutor
		want bool
	}{
		{NewStateGetExecutor(client), true},
		{NewStateSetExecutor(client), true},
		{NewPublishExecutor(client), true},
	}
	for _, tt := range tests {
		if got := tt.exec.Deterministic(); got != tt.want {
			t.Errorf("%s.Deterministic() = %v, want %v", tt.exec.Type(), got, tt.want)
		}
	}
}

// =========================
// resolveTemplate tests
// =========================

func TestResolveTemplateNoDelimiters(t *testing.T) {
	got := resolveTemplate("plain-string", nil)
	if got != "plain-string" {
		t.Errorf("resolveTemplate = %q, want plain-string", got)
	}
}

func TestResolveTemplateWithInputs(t *testing.T) {
	inputs := map[string]NodeOutputs{
		"prepare": {"workflow": "test-wf"},
	}
	got := resolveTemplate("{{ index (index .Nodes \"prepare\") \"workflow\" }}", inputs)
	if got != "test-wf" {
		t.Errorf("resolveTemplate = %q, want test-wf", got)
	}
}

func TestResolveTemplateParseError(t *testing.T) {
	// Malformed template → returns original string
	got := resolveTemplate("{{{invalid", nil)
	if got != "{{{invalid" {
		t.Errorf("resolveTemplate = %q, want original", got)
	}
}
