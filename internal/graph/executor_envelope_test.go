package graph

import (
	"context"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ===========================================================================
// envelopeStatus mapper (ADR-0004 status vocabulary)
// ===========================================================================

func TestEnvelopeStatus_MapsInternalToCanonical(t *testing.T) {
	cases := []struct {
		in   NodeStatus
		want string
	}{
		{StatusGreen, v1alpha1.NodeResultStatusOK},
		{StatusFailed, v1alpha1.NodeResultStatusFailed},
		{StatusSkipped, v1alpha1.NodeResultStatusSkipped},
		{NodeStatus(""), v1alpha1.NodeResultStatusFailed}, // unknown → failed (safe default)
	}
	for _, c := range cases {
		if got := envelopeStatus(c.in); got != c.want {
			t.Errorf("envelopeStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ===========================================================================
// synthesizeEnvelope (ADR-0004)
// ===========================================================================

// TestSynthesizeEnvelope_Baseline stamps the kernel-authoritative fields even
// when the executor returned no enrichment — every finalized node gets a
// complete, uniform envelope.
func TestSynthesizeEnvelope_Baseline(t *testing.T) {
	e := NewGraphExecutor(NewRegistry(), nil,
		WithRunID("run-7"),
		WithProvenance("tibrez", "webhook"),
	)
	nr := NodeResult{Status: StatusGreen, Feedback: "docs synced"}
	env := e.synthesizeEnvelope("agent-1", "agent", nr)

	if env.NodeID != "agent-1" {
		t.Errorf("NodeID = %q, want agent-1", env.NodeID)
	}
	if env.RunID != "run-7" {
		t.Errorf("RunID = %q, want run-7", env.RunID)
	}
	if env.Status != v1alpha1.NodeResultStatusOK {
		t.Errorf("Status = %q, want ok", env.Status)
	}
	// No executor Summary → falls back to Feedback.
	if env.Summary != "docs synced" {
		t.Errorf("Summary = %q, want docs synced (feedback fallback)", env.Summary)
	}
	if env.Provenance.TriggeredBy != "tibrez" || env.Provenance.TriggerSource != "webhook" {
		t.Errorf("Provenance = %+v, want tibrez/webhook", env.Provenance)
	}
	if env.ProducedAt.IsZero() {
		t.Error("ProducedAt should be set")
	}
	if len(env.Claims) != 0 {
		t.Errorf("baseline envelope should have no claims, got %d", len(env.Claims))
	}
}

// TestSynthesizeEnvelope_ExecutorEnrichmentMerged proves executor-provided
// claims/artifacts/references/payload/summary are preserved, while the kernel
// still owns NodeID/RunID/Status/Provenance/ProducedAt.
func TestSynthesizeEnvelope_ExecutorEnrichmentMerged(t *testing.T) {
	e := NewGraphExecutor(NewRegistry(), nil, WithRunID("run-7"))
	executorEnv := &v1alpha1.NodeResultEnvelope{
		Summary: "pushed commit deadbeef",
		Claims: []v1alpha1.Claim{{
			Type: "repository.commit.created", Binding: "workspaceRepo",
			ExternalID: "deadbeef", TrustClass: v1alpha1.ClaimTrustObserved,
		}},
		Artifacts:  []v1alpha1.Artifact{{Name: "rig", Path: "raw/arch/x/rig.json"}},
		Payload:    []byte(`{"files":3}`),
		References: []v1alpha1.EvidenceReference{{Binding: "workspaceRepo", Kind: "commit", Identifier: "deadbeef"}},
	}
	nr := NodeResult{Status: StatusGreen, Envelope: executorEnv}
	env := e.synthesizeEnvelope("deploy-1", "plugin", nr)

	// Executor enrichment preserved.
	if env.Summary != "pushed commit deadbeef" {
		t.Errorf("Summary = %q, want executor-provided", env.Summary)
	}
	if len(env.Claims) != 1 || env.Claims[0].ExternalID != "deadbeef" {
		t.Errorf("Claims not preserved: %+v", env.Claims)
	}
	if len(env.Artifacts) != 1 || env.Artifacts[0].Name != "rig" {
		t.Errorf("Artifacts not preserved: %+v", env.Artifacts)
	}
	if string(env.Payload) != `{"files":3}` {
		t.Errorf("Payload not preserved: %s", env.Payload)
	}
	if len(env.References) != 1 || env.References[0].Identifier != "deadbeef" {
		t.Errorf("References not preserved: %+v", env.References)
	}
	// Kernel-authoritative fields stamped regardless.
	if env.NodeID != "deploy-1" || env.RunID != "run-7" || env.Status != v1alpha1.NodeResultStatusOK {
		t.Errorf("kernel fields not stamped: %+v", env)
	}
}

// ===========================================================================
// Integration: envelopes recorded into ExecutionResult + lifecycle events
// ===========================================================================

// TestExecute_EnvelopeRecordedForExecutedNode proves the kernel synthesizes +
// records an envelope for every executed node and that executor-provided
// enrichment (a claim) survives into ExecutionResult.NodeEnvelopes.
func TestExecute_EnvelopeRecordedForExecutedNode(t *testing.T) {
	execA := newRecording("typeA", NodeResult{
		Status:   StatusGreen,
		Feedback: "done",
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "repository.commit.created", Binding: "repo",
				ExternalID: "abc123", TrustClass: v1alpha1.ClaimTrustObserved,
			}},
		},
	})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA})
	g := v1alpha1.GraphSpec{Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "typeA"}}}

	e := NewGraphExecutor(registry, nil, WithRunID("run-9"), WithProvenance("alice", "manual"))
	result, err := e.Execute(context.Background(), g, "env-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env, ok := result.NodeEnvelopes["A"]
	if !ok {
		t.Fatal("no envelope recorded for executed node A")
	}
	if env.NodeID != "A" || env.RunID != "run-9" {
		t.Errorf("envelope identity = %+v, want A/run-9", env)
	}
	if env.Status != v1alpha1.NodeResultStatusOK {
		t.Errorf("envelope status = %q, want ok", env.Status)
	}
	if env.Provenance.TriggeredBy != "alice" || env.Provenance.TriggerSource != "manual" {
		t.Errorf("envelope provenance = %+v, want alice/manual", env.Provenance)
	}
	if len(env.Claims) != 1 || env.Claims[0].ExternalID != "abc123" {
		t.Errorf("executor-provided claim not preserved in envelope: %+v", env.Claims)
	}
}

// TestExecute_EnvelopeSynthesizedWhenExecutorProvidesNone proves backward
// compatibility: an executor that returns no Envelope still gets a synthesized
// baseline envelope recorded (status + provenance + summary-from-feedback).
func TestExecute_EnvelopeSynthesizedWhenExecutorProvidesNone(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen, Feedback: "ok"})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA})
	g := v1alpha1.GraphSpec{Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "typeA"}}}

	e := NewGraphExecutor(registry, nil, WithProvenance("system", "schedule"))
	result, err := e.Execute(context.Background(), g, "baseline-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env, ok := result.NodeEnvelopes["A"]
	if !ok {
		t.Fatal("no baseline envelope recorded for node A")
	}
	if env.Status != v1alpha1.NodeResultStatusOK {
		t.Errorf("status = %q, want ok", env.Status)
	}
	if env.Summary != "ok" {
		t.Errorf("summary = %q, want feedback fallback 'ok'", env.Summary)
	}
	if len(env.Claims) != 0 {
		t.Errorf("baseline envelope should have no claims, got %d", len(env.Claims))
	}
}

// TestExecute_EnvelopeForDeniedNode proves a capability-denied node still gets
// a recorded envelope (status failed), so the orchestration history reflects
// the policy refusal.
func TestExecute_EnvelopeForDeniedNode(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA})
	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "typeA", Requires: requires("repo", "repository.push")}},
	}
	e := NewGraphExecutor(registry, nil, WithBindings([]v1alpha1.ExternalSystemBinding{grant("repo", "repository.read")}))

	result, err := e.Execute(context.Background(), g, "denied-env-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env, ok := result.NodeEnvelopes["A"]
	if !ok {
		t.Fatal("no envelope recorded for denied node A")
	}
	if env.Status != v1alpha1.NodeResultStatusFailed {
		t.Errorf("denied node envelope status = %q, want failed", env.Status)
	}
}

// TestExecute_EnvelopeCountMatchesExecutedNodes confirms the result carries
// one envelope per executed node (every finalized node is recorded).
func TestExecute_EnvelopeCountMatchesExecutedNodes(t *testing.T) {
	execA := newRecording("typeA", NodeResult{Status: StatusGreen})
	execB := newRecording("typeB", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"typeA": execA, "typeB": execB})
	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "typeA"}, {ID: "B", Type: "typeB"}},
		Edges: []v1alpha1.EdgeSpec{{From: "A", To: "B"}},
	}
	e := NewGraphExecutor(registry, nil)
	result, err := e.Execute(context.Background(), g, "count-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.NodeEnvelopes) != 2 {
		t.Errorf("expected 2 envelopes (one per node), got %d: %+v", len(result.NodeEnvelopes), result.NodeEnvelopes)
	}
}
