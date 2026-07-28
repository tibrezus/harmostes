package claim

import (
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func claim(typ, binding, trust string) v1alpha1.Claim {
	return v1alpha1.Claim{Type: typ, Binding: binding, ExternalID: typ + "-" + binding, TrustClass: trust}
}

// ===========================================================================
// MatchGlob
// ===========================================================================

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"wiki.page.*", "wiki.page.updated", true},
		{"wiki.page.*", "wiki.page.created", true},
		{"wiki.page.*", "wiki.page", false},
		{"repository.commit.created", "repository.commit.created", true},
		{"repository.commit.created", "repository.commit.pushed", false},
		{"*.created", "wiki.page.created", true},
		{"*", "anything", true},
		{"*", "", true},
		{"release.tag.*", "release.tag.v1", true},
		{"a*b", "axxxb", true},
		{"a*b", "ab", true},
		{"a*b", "ac", false},
		{"a*b*c", "axxbyyc", true},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.s); got != c.want {
			t.Errorf("MatchGlob(%q,%q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// ===========================================================================
// Enforce — the trust boundary (demotion)
// ===========================================================================

func TestEnforce_NonDeterministicDemotesValidated(t *testing.T) {
	env := &v1alpha1.NodeResultEnvelope{
		NodeID: "agent-1",
		Claims: []v1alpha1.Claim{
			claim("repository.commit.created", "repo", v1alpha1.ClaimTrustValidated),
			claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustObserved),
		},
	}
	n := Enforce(false, env)
	if n != 1 {
		t.Errorf("demoted count = %d, want 1", n)
	}
	if env.Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("claim 0 should be demoted to observed, got %q", env.Claims[0].TrustClass)
	}
	if env.Claims[0].ValidatedBy != "" {
		t.Errorf("claim 0 ValidatedBy should be cleared, got %q", env.Claims[0].ValidatedBy)
	}
	if env.Claims[1].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("claim 1 (already observed) should be untouched, got %q", env.Claims[1].TrustClass)
	}
}

func TestEnforce_DeterministicLeavesValidatedAlone(t *testing.T) {
	env := &v1alpha1.NodeResultEnvelope{
		Claims: []v1alpha1.Claim{claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustValidated)},
	}
	if n := Enforce(true, env); n != 0 {
		t.Errorf("deterministic node: demoted %d, want 0", n)
	}
	if env.Claims[0].TrustClass != v1alpha1.ClaimTrustValidated {
		t.Errorf("deterministic claim should stay validated, got %q", env.Claims[0].TrustClass)
	}
}

func TestEnforce_NoClaims(t *testing.T) {
	env := &v1alpha1.NodeResultEnvelope{NodeID: "x"}
	if n := Enforce(false, env); n != 0 {
		t.Errorf("no claims: demoted %d, want 0", n)
	}
}

func TestEnforce_NilSafe(t *testing.T) {
	if n := Enforce(false, nil); n != 0 {
		t.Errorf("nil envelope: demoted %d, want 0", n)
	}
}

// ===========================================================================
// Promote — gates as fact-promoters
// ===========================================================================

func TestPromote_WithinScope(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"deploy": {NodeID: "deploy", Claims: []v1alpha1.Claim{
			claim("repository.commit.created", "repo", v1alpha1.ClaimTrustObserved),
		}},
		"agent": {NodeID: "agent", Claims: []v1alpha1.Claim{
			claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustObserved),
		}},
	}
	promotions := Promote(envs, "gate-1", ValidationScope{ClaimTypes: []string{"wiki.page.*"}})

	if len(promotions) != 1 {
		t.Fatalf("expected 1 promotion, got %d: %+v", len(promotions), promotions)
	}
	p := promotions[0]
	if p.FromNodeID != "agent" || p.ValidatedBy != "gate-1" {
		t.Errorf("promotion = %+v", p)
	}
	if envs["agent"].Claims[0].TrustClass != v1alpha1.ClaimTrustValidated {
		t.Errorf("agent claim should be promoted to validated")
	}
	if envs["agent"].Claims[0].ValidatedBy != "gate-1" {
		t.Errorf("agent claim ValidatedBy = %q, want gate-1", envs["agent"].Claims[0].ValidatedBy)
	}
	// Out-of-scope claim untouched.
	if envs["deploy"].Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("deploy claim (out of scope) should stay observed")
	}
}

func TestPromote_BindingFilter(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"a": {Claims: []v1alpha1.Claim{
			claim("repository.commit.created", "repo", v1alpha1.ClaimTrustObserved),
		}},
		"b": {Claims: []v1alpha1.Claim{
			claim("repository.commit.created", "staging", v1alpha1.ClaimTrustObserved),
		}},
	}
	promotions := Promote(envs, "gate", ValidationScope{
		ClaimTypes: []string{"repository.commit.*"},
		Bindings:   []string{"repo"},
	})
	if len(promotions) != 1 || promotions[0].FromNodeID != "a" {
		t.Fatalf("binding filter should promote only 'a', got %+v", promotions)
	}
	if envs["b"].Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("'b' (wrong binding) should stay observed")
	}
}

func TestPromote_EmptyScopeIsNoOp(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"a": {Claims: []v1alpha1.Claim{claim("x.y.z", "b", v1alpha1.ClaimTrustObserved)}},
	}
	if promotions := Promote(envs, "gate", ValidationScope{}); promotions != nil {
		t.Errorf("empty scope should promote nothing, got %+v", promotions)
	}
	if envs["a"].Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Errorf("empty scope should leave claim observed")
	}
}

func TestPromote_AlreadyValidatedNotRetouched(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"a": {Claims: []v1alpha1.Claim{
			claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustValidated),
		}},
	}
	if promotions := Promote(envs, "gate-2", ValidationScope{ClaimTypes: []string{"wiki.*"}}); len(promotions) != 0 {
		t.Errorf("already-validated claim should not be re-promoted, got %+v", promotions)
	}
	if envs["a"].Claims[0].ValidatedBy != "" {
		t.Errorf("already-validated claim ValidatedBy should be unchanged (no re-stamp)")
	}
}

func TestPromote_Idempotent(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"a": {Claims: []v1alpha1.Claim{claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustObserved)}},
	}
	scope := ValidationScope{ClaimTypes: []string{"wiki.*"}}
	_ = Promote(envs, "gate", scope)
	second := Promote(envs, "gate", scope)
	if len(second) != 0 {
		t.Errorf("second promote should be a no-op, got %+v", second)
	}
}

// ===========================================================================
// HasValidated
// ===========================================================================

func TestHasValidated(t *testing.T) {
	envs := []v1alpha1.NodeResultEnvelope{
		{Claims: []v1alpha1.Claim{claim("a.b.c", "x", v1alpha1.ClaimTrustObserved)}},
	}
	if HasValidated(envs) {
		t.Error("observed-only should not count as validated")
	}
	envs = append(envs, v1alpha1.NodeResultEnvelope{
		Claims: []v1alpha1.Claim{claim("d.e.f", "y", v1alpha1.ClaimTrustValidated)},
	})
	if !HasValidated(envs) {
		t.Error("one validated claim should count as validated")
	}
	if HasValidated(nil) {
		t.Error("nil should not count as validated")
	}
}

// ===========================================================================
// End-to-end: Enforce then Promote (the full trust pipeline)
// ===========================================================================

// TestPipeline_EnforceThenPromote proves the full ADR-0004 pipeline: a
// non-deterministic node asserts a validated claim, the kernel demotes it to
// observed (Enforce), then a deterministic gate promotes it back to validated
// (Promote) — but ONLY through the deterministic validator, never self-asserted.
func TestPipeline_EnforceThenPromote(t *testing.T) {
	agentEnv := &v1alpha1.NodeResultEnvelope{
		NodeID: "agent-1",
		Claims: []v1alpha1.Claim{
			// LLM agent self-asserts "validated" — must be demoted.
			claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustValidated),
		},
	}
	// 1. Kernel enforces: non-deterministic → demoted.
	if n := Enforce(false, agentEnv); n != 1 {
		t.Fatalf("expected 1 demotion, got %d", n)
	}
	if agentEnv.Claims[0].TrustClass != v1alpha1.ClaimTrustObserved {
		t.Fatal("claim should be demoted to observed before any promotion")
	}

	// 2. Deterministic gate (wiki-lint) succeeds and promotes within scope.
	envs := map[string]v1alpha1.NodeResultEnvelope{"agent-1": *agentEnv}
	promotions := Promote(envs, "gate-wiki", ValidationScope{ClaimTypes: []string{"wiki.page.*"}})
	if len(promotions) != 1 {
		t.Fatalf("expected 1 promotion after demotion, got %d", len(promotions))
	}
	if envs["agent-1"].Claims[0].TrustClass != v1alpha1.ClaimTrustValidated {
		t.Error("claim should be validated through the gate")
	}
	if envs["agent-1"].Claims[0].ValidatedBy != "gate-wiki" {
		t.Errorf("ValidatedBy = %q, want gate-wiki", envs["agent-1"].Claims[0].ValidatedBy)
	}
	// The claim is now authoritative for the Attempt phase.
	if !HasValidated([]v1alpha1.NodeResultEnvelope{envs["agent-1"]}) {
		t.Error("HasValidated should be true after gate promotion")
	}
}

// determinism check for Promote output ordering (promotion order follows map
// iteration, which is non-deterministic) — sort for stable assertions where
// order matters. This test just confirms Promote doesn't double-count.
func TestPromote_NoDoubleCountAcrossScopes(t *testing.T) {
	envs := map[string]v1alpha1.NodeResultEnvelope{
		"a": {Claims: []v1alpha1.Claim{claim("wiki.page.updated", "wiki", v1alpha1.ClaimTrustObserved)}},
	}
	scopes := []ValidationScope{
		{ClaimTypes: []string{"wiki.*"}},
		{ClaimTypes: []string{"*.updated"}},
	}
	var total []Promotion
	for _, s := range scopes {
		total = append(total, Promote(envs, "gate", s)...)
	}
	if len(total) != 1 {
		t.Errorf("overlapping scopes should not double-promote; got %d promotions", len(total))
	}
}
