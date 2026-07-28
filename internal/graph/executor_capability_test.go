package graph

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// bindings helper for capability-enforcement tests.
func grant(name string, caps ...string) v1alpha1.ExternalSystemBinding {
	return v1alpha1.ExternalSystemBinding{Name: name, Granted: caps}
}

// requires helper: a node that requests a capability against a binding.
func requires(binding, capability string) []v1alpha1.CapabilityRequirement {
	return []v1alpha1.CapabilityRequirement{{Binding: binding, Capability: capability}}
}

// TestExecute_CapabilityDeniedFailsPipeline proves the kernel refuses to
// execute a node whose Surface Capability requirements are not satisfied by the
// Workflow's bindings (ADR-0003): the denied node is never executed, the
// pipeline fails, and the failure feedback names the policy violation.
func TestExecute_CapabilityDeniedFailsPipeline(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	execB := newRecording("typeB", NodeResult{Status: StatusGreen}) // would succeed if allowed to run
	registry := registryWith(map[string]NodeExecutor{"typeA": execA, "typeB": execB})

	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "typeA"},
			{ID: "B", Type: "typeB", Requires: requires("repo", "repository.push")},
		},
		Edges: []v1alpha1.EdgeSpec{{From: "A", To: "B"}},
	}

	// Binding "repo" grants only read, but B requires push → denied.
	exec := NewGraphExecutor(registry, nil, WithBindings([]v1alpha1.ExternalSystemBinding{
		grant("repo", "repository.read"),
	}))
	result, err := exec.Execute(context.Background(), g, "policy-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Status != StatusFailed {
		t.Errorf("pipeline status = %s, want failed (capability denied)", result.Status)
	}
	if execA.visitCount() != 1 {
		t.Errorf("node A should execute once, got %d", execA.visitCount())
	}
	if execB.visitCount() != 0 {
		t.Errorf("node B must NOT execute when denied, got %d visits", execB.visitCount())
	}
	bRes, ok := result.NodeResults["B"]
	if !ok || bRes.Status != StatusFailed {
		t.Fatalf("denied node B result = %+v, want failed", result.NodeResults["B"])
	}
	if !strings.Contains(strings.ToLower(bRes.Feedback), "capability policy") {
		t.Errorf("denied node feedback should mention capability policy: %q", bRes.Feedback)
	}
	if !strings.Contains(bRes.Feedback, "repository.push") {
		t.Errorf("denied node feedback should name the capability: %q", bRes.Feedback)
	}
}

// TestExecute_CapabilitySatisfiedExecutes proves that when the binding grants
// the requested capability, the node executes normally and the pipeline
// succeeds. This is the positive control for the enforcement path.
func TestExecute_CapabilitySatisfiedExecutes(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	execB := newRecording("typeB", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA, "typeB": execB})

	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "typeA"},
			{ID: "B", Type: "typeB", Requires: requires("repo", "repository.push")},
		},
		Edges: []v1alpha1.EdgeSpec{{From: "A", To: "B"}},
	}

	exec := NewGraphExecutor(registry, nil, WithBindings([]v1alpha1.ExternalSystemBinding{
		grant("repo", "repository.read", "repository.push"),
	}))
	result, err := exec.Execute(context.Background(), g, "policy-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("pipeline status = %s, want green (capability satisfied)", result.Status)
	}
	if execB.visitCount() != 1 {
		t.Errorf("node B should execute once when authorized, got %d", execB.visitCount())
	}
}

// TestExecute_CapabilityDeniedWithoutBindings proves enforcement is always-on
// for nodes that carry Requires: with no bindings declared at all, a node
// requesting a capability is refused (the binding is "not declared"). This
// confirms the kernel refuses unauthorized execution rather than silently
// allowing it.
func TestExecute_CapabilityDeniedWithoutBindings(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA})

	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "typeA", Requires: requires("repo", "repository.read")},
		},
	}
	// No WithBindings option at all.
	exec := NewGraphExecutor(registry, nil)
	result, err := exec.Execute(context.Background(), g, "policy-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Errorf("pipeline status = %s, want failed (no bindings declared)", result.Status)
	}
	if execA.visitCount() != 0 {
		t.Errorf("node A must NOT execute when its binding is undeclared, got %d", execA.visitCount())
	}
}

// TestExecute_CapabilityDeniedRoutedByFailedEdge proves a denied node honors a
// when:failed edge: the pipeline can route around a policy-denied node just as
// it routes around an execution failure. Here A→B (denied) carries a
// when:failed edge to C, which runs and the pipeline completes green.
func TestExecute_CapabilityDeniedRoutedByFailedEdge(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	execB := newRecording("typeB", NodeResult{Status: StatusGreen})
	execC := newRecording("typeC", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{
		"typeA": execA, "typeB": execB, "typeC": execC,
	})

	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "A", Type: "typeA"},
			{ID: "B", Type: "typeB", Requires: requires("repo", "repository.push")},
			{ID: "C", Type: "typeC"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "A", To: "B"},
			{From: "B", To: "C", When: "failed"}, // route around the denied node
		},
	}

	exec := NewGraphExecutor(registry, nil, WithBindings([]v1alpha1.ExternalSystemBinding{
		grant("repo", "repository.read"),
	}))
	result, err := exec.Execute(context.Background(), g, "policy-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// B is denied (never runs); the failed edge lets C run; pipeline completes.
	if execB.visitCount() != 0 {
		t.Errorf("denied node B must not execute, got %d", execB.visitCount())
	}
	if execC.visitCount() != 1 {
		t.Errorf("node C should run once via the failed edge, got %d", execC.visitCount())
	}
	if result.Status != StatusGreen {
		t.Errorf("pipeline status = %s, want green (failed edge handled the denial)", result.Status)
	}
}

// TestExecute_NoRequiresUnaffected proves backward compatibility: a graph
// whose nodes carry no Requires behaves identically with or without WithBindings
// — enforcement never fires.
func TestExecute_NoRequiresUnaffected(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA})
	g := v1alpha1.GraphSpec{Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "typeA"}}}

	// With bindings declared but node requests nothing.
	exec := NewGraphExecutor(registry, nil, WithBindings([]v1alpha1.ExternalSystemBinding{
		grant("repo", "repository.read"),
	}))
	result, err := exec.Execute(context.Background(), g, "policy-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("pipeline status = %s, want green (no Requires)", result.Status)
	}
	if execA.visitCount() != 1 {
		t.Errorf("node A should execute once, got %d", execA.visitCount())
	}
}
