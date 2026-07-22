package graph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// slowExecutor blocks for the given duration before returning green.
type slowExecutor struct {
	delay time.Duration
}

func (e *slowExecutor) Execute(ctx context.Context, _ v1alpha1.NodeSpec, _ NodeEnv) (NodeResult, error) {
	select {
	case <-time.After(e.delay):
		return NodeResult{Status: StatusGreen}, nil
	case <-ctx.Done():
		// Return empty feedback — the graph executor will detect the timeout
		// and set "timed out after {duration}".
		return NodeResult{Status: StatusFailed}, ctx.Err()
	}
}
func (e *slowExecutor) Type() string        { return "slow" }
func (e *slowExecutor) Deterministic() bool { return true }

// TestPerNodeTimeout verifies that a node with a Timeout field is killed
// when the deadline fires, and marked failed with "timed out after {duration}".
func TestPerNodeTimeout(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&slowExecutor{delay: 5 * time.Second})

	exec := NewGraphExecutor(registry, nil)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "slow-node", Type: "slow", Timeout: "100ms"},
		},
	}

	result, err := exec.Execute(context.Background(), graph, "test-timeout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected pipeline failed, got %s", result.Status)
	}
	nodeResult := result.NodeResults["slow-node"]
	if nodeResult.Status != StatusFailed {
		t.Fatalf("expected node failed, got %s", nodeResult.Status)
	}
	if !strings.Contains(nodeResult.Feedback, "timed out") {
		t.Fatalf("expected timeout feedback, got %q", nodeResult.Feedback)
	}
}

// TestPerNodeTimeoutNotTriggered verifies that a node with a generous timeout
// completes normally.
func TestPerNodeTimeoutNotTriggered(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&slowExecutor{delay: 10 * time.Millisecond})

	exec := NewGraphExecutor(registry, nil)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "fast-node", Type: "slow", Timeout: "5s"},
		},
	}

	result, err := exec.Execute(context.Background(), graph, "test-no-timeout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusGreen {
		t.Fatalf("expected pipeline green, got %s", result.Status)
	}
}

// TestPerNodeInvalidTimeout verifies that an invalid timeout string is ignored
// (the node runs without a deadline).
func TestPerNodeInvalidTimeout(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&slowExecutor{delay: 10 * time.Millisecond})

	exec := NewGraphExecutor(registry, nil)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "bad-timeout", Type: "slow", Timeout: "not-a-duration"},
		},
	}

	result, err := exec.Execute(context.Background(), graph, "test-invalid-timeout")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusGreen {
		t.Fatalf("expected green with invalid timeout ignored, got %s", result.Status)
	}
}

// TestDeadLetterOnFailure verifies that a dead-letter event is published to the
// harmostes-dead-letter topic when a pipeline fails.
func TestDeadLetterOnFailure(t *testing.T) {
	registry := NewRegistry()
	registry.Register(newRecording("plugin", NodeResult{Status: StatusGreen}))
	registry.Register(newRecording("fail", NodeResult{Status: StatusFailed, Feedback: "gate rejected: lint errors"}))

	daprClient := newFakeDaprClient()
	exec := NewGraphExecutor(registry, daprClient)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "build", Type: "plugin"},
			{ID: "fail-node", Type: "fail"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "build", To: "fail-node"},
		},
	}

	result, err := exec.Execute(context.Background(), graph, "test-deadletter")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("expected pipeline failed, got %s", result.Status)
	}

	// Find the dead-letter message.
	var deadLetterFound bool
	for _, msg := range daprClient.published {
		if msg.Topic == DeadLetterTopic {
			deadLetterFound = true
			var dl DeadLetterEvent
			if err := json.Unmarshal([]byte(msg.Payload), &dl); err != nil {
				t.Fatalf("dead-letter unmarshal: %v", err)
			}
			if dl.Pipeline != "test-deadletter" {
				t.Errorf("dead-letter pipeline = %q, want %q", dl.Pipeline, "test-deadletter")
			}
			if dl.FailedNode != "fail-node" {
				t.Errorf("dead-letter failedNode = %q, want %q", dl.FailedNode, "fail-node")
			}
			if dl.Error == "" {
				t.Error("dead-letter error should not be empty")
			}
		}
	}
	if !deadLetterFound {
		t.Error("expected a dead-letter message on failure")
	}
}

// TestProvenanceInLifecycle verifies that lifecycle events carry the
// triggeredBy and triggerSource provenance fields.
func TestProvenanceInLifecycle(t *testing.T) {
	registry := NewRegistry()
	registry.Register(newRecording("plugin", NodeResult{Status: StatusGreen}))

	daprClient := newFakeDaprClient()
	exec := NewGraphExecutor(registry, daprClient, WithProvenance("alice", "webhook"))

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "build", Type: "plugin"},
		},
	}

	_, err := exec.Execute(context.Background(), graph, "test-provenance")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that lifecycle events have provenance fields.
	for _, msg := range daprClient.published {
		if msg.Topic != LifecycleTopic {
			continue
		}
		var ev LifecycleEvent
		if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
			t.Fatalf("lifecycle unmarshal: %v", err)
		}
		if ev.TriggeredBy != "alice" {
			t.Errorf("triggeredBy = %q, want %q", ev.TriggeredBy, "alice")
		}
		if ev.TriggerSource != "webhook" {
			t.Errorf("triggerSource = %q, want %q", ev.TriggerSource, "webhook")
		}
	}
}

// TestDeadLetterNotPublishedOnSuccess verifies that no dead-letter event is
// published when the pipeline succeeds.
func TestDeadLetterNotPublishedOnSuccess(t *testing.T) {
	registry := NewRegistry()
	registry.Register(newRecording("plugin", NodeResult{Status: StatusGreen}))

	daprClient := newFakeDaprClient()
	exec := NewGraphExecutor(registry, daprClient)

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "build", Type: "plugin"},
		},
	}

	_, err := exec.Execute(context.Background(), graph, "test-no-deadletter")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, msg := range daprClient.published {
		if msg.Topic == DeadLetterTopic {
			t.Error("dead-letter should not be published on success")
		}
	}
}
