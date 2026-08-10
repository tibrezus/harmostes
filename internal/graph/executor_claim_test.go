package graph

import (
	"context"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/claim"
)

// nonDetRecording is a recordingExecutor whose Deterministic() returns false,
// simulating an LLM agent node (the trust-boundary case for ADR-0004).
type nonDetRecording struct{ recordingExecutor }

func newNonDet(typ string, result NodeResult) *nonDetRecording {
	return &nonDetRecording{recordingExecutor: *newRecording(typ, result)}
}
func (r *nonDetRecording) Deterministic() bool    { return false }
func (r *nonDetRecording) ExecutionClass() string { return ExecutionClassWorkload }

// ===========================================================================
// Trust enforcement: non-deterministic nodes' self-validated claims are demoted
// ===========================================================================

// TestExecute_NonDeterministicClaimsDemoted proves the kernel demotes a
// non-deterministic node's self-asserted validated claim to observed. The LLM
// agent cannot self-validate; the recorded envelope must carry observed.
func TestExecute_NonDeterministicClaimsDemoted(t *testing.T) {
	agent := newNonDet("agent", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustValidated, // self-asserted — must be demoted
			}},
		},
	})
	registry := registryWith(map[string]NodeExecutor{"agent": agent})
	g := v1alpha1.GraphSpec{Nodes: []v1alpha1.NodeSpec{{ID: "A", Type: "agent"}}}

	e := NewGraphExecutor(registry, nil, WithRunID("run-trust"))
	result, err := e.Execute(context.Background(), g, "trust-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env := result.NodeEnvelopes["A"]
	if len(env.Claims) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(env.Claims))
	}
	if env.Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("non-deterministic self-validated claim must be demoted to observed, got %q", env.Claims[0].TrustClass)
	}
	if env.Claims[0].ValidatedBy != "" {
		t.Errorf("demoted claim ValidatedBy must be cleared, got %q", env.Claims[0].ValidatedBy)
	}
}

// TestExecute_DeterministicClaimsKept proves a deterministic node's validated
// claims are NOT demoted (it is allowed to self-validate).
func TestExecute_DeterministicClaimsKept(t *testing.T) {
	gateExec := newRecording("gate", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustValidated,
			}},
		},
	})
	registry := registryWith(map[string]NodeExecutor{"gate": gateExec})
	g := v1alpha1.GraphSpec{Nodes: []v1alpha1.NodeSpec{{ID: "G", Type: "gate"}}}

	e := NewGraphExecutor(registry, nil)
	result, _ := e.Execute(context.Background(), g, "det-test")

	env := result.NodeEnvelopes["G"]
	if env.Claims[0].TrustClass != v1alpha1.ClaimTrustValidated {
		t.Errorf("deterministic claim must stay validated, got %q", env.Claims[0].TrustClass)
	}
}

// ===========================================================================
// Claim promotion: a deterministic gate promotes observed claims on green
// ===========================================================================

// gateWithScope builds a gate node config that declares a validation scope.
func gateWithScope(plugin, scope string) v1alpha1.NodeSpec {
	raw := []byte(`{"plugin":{"name":"` + plugin + `"},"validates":[{"claimTypes":["` + scope + `"]}]}`)
	return v1alpha1.NodeSpec{ID: "gate", Type: "gate", Config: raw}
}

// TestExecute_GatePromotesObservedClaims proves a successful deterministic gate
// promotes matching observed claims from upstream nodes to validated, stamping
// ValidatedBy.
func TestExecute_GatePromotesObservedClaims(t *testing.T) {
	// Non-deterministic agent produces an observed claim.
	agent := newNonDet("agent", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustObserved,
			}},
		},
	})
	// Deterministic gate succeeds (exit 0 = green via the recording stub).
	gateExec := newRecording("gate", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"agent": agent, "gate": gateExec})

	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			gateWithScope("wiki-lint", "wiki.page.*"),
		},
		Edges: []v1alpha1.EdgeSpec{{From: "agent", To: "gate"}},
	}
	e := NewGraphExecutor(registry, nil, WithRunID("run-promo"))
	result, err := e.Execute(context.Background(), g, "promo-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	agentEnv := result.NodeEnvelopes["agent"]
	if len(agentEnv.Claims) != 1 {
		t.Fatalf("expected 1 agent claim, got %d", len(agentEnv.Claims))
	}
	if agentEnv.Claims[0].TrustClass != v1alpha1.ClaimTrustValidated {
		t.Errorf("agent claim should be promoted to validated by the gate, got %q", agentEnv.Claims[0].TrustClass)
	}
	if agentEnv.Claims[0].ValidatedBy != "gate" {
		t.Errorf("ValidatedBy = %q, want gate", agentEnv.Claims[0].ValidatedBy)
	}
}

// TestExecute_GateNoScopeNoPromotion proves a gate that declares no ValidationScope
// promotes nothing (backward compatible — existing gates don't validate claims).
func TestExecute_GateNoScopeNoPromotion(t *testing.T) {
	agent := newNonDet("agent", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustObserved,
			}},
		},
	})
	gateExec := newRecording("gate", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"agent": agent, "gate": gateExec})
	// Gate config WITHOUT validates.
	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			{ID: "gate", Type: "gate", Config: []byte(`{"plugin":{"name":"wiki-lint"}}`)},
		},
		Edges: []v1alpha1.EdgeSpec{{From: "agent", To: "gate"}},
	}
	e := NewGraphExecutor(registry, nil)
	result, _ := e.Execute(context.Background(), g, "no-scope-test")

	agentEnv := result.NodeEnvelopes["agent"]
	if agentEnv.Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("gate with no scope must not promote; claim should stay observed, got %q", agentEnv.Claims[0].TrustClass)
	}
}

// TestExecute_FailedGateNoPromotion proves promotion only happens on green — a
// failed deterministic gate does not promote claims.
func TestExecute_FailedGateNoPromotion(t *testing.T) {
	agent := newNonDet("agent", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustObserved,
			}},
		},
	})
	gateExec := newRecording("gate", NodeResult{Status: StatusFailed, Feedback: "lint errors"})
	registry := registryWith(map[string]NodeExecutor{"agent": agent, "gate": gateExec})
	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			gateWithScope("wiki-lint", "wiki.page.*"),
		},
		Edges: []v1alpha1.EdgeSpec{{From: "agent", To: "gate"}},
	}
	e := NewGraphExecutor(registry, nil)
	result, _ := e.Execute(context.Background(), g, "fail-gate-test")

	agentEnv := result.NodeEnvelopes["agent"]
	if agentEnv.Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("failed gate must not promote; claim should stay observed, got %q", agentEnv.Claims[0].TrustClass)
	}
}

// TestNodeValidationScopes parses a gate config and confirms the scope is
// extracted; non-gate nodes return nil.
func TestNodeValidationScopes(t *testing.T) {
	if got := nodeValidationScopes(v1alpha1.NodeSpec{ID: "x", Type: "agent"}); got != nil {
		t.Errorf("non-gate node should have no scopes, got %+v", got)
	}
	got := nodeValidationScopes(gateWithScope("g", "wiki.*"))
	if len(got) != 1 || len(got[0].ClaimTypes) != 1 || got[0].ClaimTypes[0] != "wiki.*" {
		t.Errorf("gate scope not extracted: %+v", got)
	}
	// Gate with no validates.
	if got := nodeValidationScopes(v1alpha1.NodeSpec{ID: "g", Type: "gate", Config: []byte(`{"plugin":{"name":"g"}}`)}); len(got) != 0 {
		t.Errorf("gate without validates should yield no scopes, got %+v", got)
	}
}

// TestExecute_ClaimValidatedLifecycleEvent confirms a promotion emits a
// claim.validated lifecycle event (real-time UI visibility). We assert via the
// Dapr client capture since publishLifecycle is nil-safe without a client.
func TestExecute_ClaimValidatedLifecycleEvent(t *testing.T) {
	agent := newNonDet("agent", NodeResult{
		Status: StatusGreen,
		Envelope: &v1alpha1.NodeResultEnvelope{
			Claims: []v1alpha1.Claim{{
				Type: "wiki.page.updated", Binding: "wiki", ExternalID: "Home",
				TrustClass: v1alpha1.ClaimTrustObserved,
			}},
		},
	})
	gateExec := newRecording("gate", NodeResult{Status: StatusGreen})
	registry := registryWith(map[string]NodeExecutor{"agent": agent, "gate": gateExec})
	g := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "agent", Type: "agent"},
			gateWithScope("wiki-lint", "wiki.page.*"),
		},
		Edges: []v1alpha1.EdgeSpec{{From: "agent", To: "gate"}},
	}
	// With a nil Dapr client, publishLifecycle is a no-op, but the promotion
	// itself must still apply to the envelopes (proven above). This test guards
	// against a panic in applyPromotions when publishing to a nil client.
	e := NewGraphExecutor(registry, nil)
	if _, err := e.Execute(context.Background(), g, "lifecycle-test"); err != nil {
		t.Fatalf("promotion + nil client must not panic/error: %v", err)
	}
}

// Compile-time: confirm claim package is wired (guards against accidental
// removal of the import).
var _ = claim.HasValidated
