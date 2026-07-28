// Package claim implements the ADR-0004 trust + promotion policy: a pure
// engine that (a) enforces the trust boundary — non-deterministic nodes' claims
// are never authoritative — and (b) promotes reference-backed facts when a
// deterministic validator confirms them.
//
// Harmostes is a DETERMINISTIC FACT-PROMOTION SYSTEM. A Claim is a
// reference-backed fact (ADR-0004). Claims from non-deterministic nodes (LLM
// agents, external tools) carry TrustClass observed — they are proposals. Only
// a deterministic validator may elevate a claim to validated, and the kernel
// enforces this boundary unconditionally: a non-deterministic node that asserts
// its own claim as validated is demoted back to observed before the claim is
// recorded. The kernel never trusts tea leaves.
package claim

import (
	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ValidationScope declares what a deterministic validator (typically a gate)
// confirms. A claim matches the scope when its Type matches any ClaimType glob
// AND (Bindings is empty OR its Binding is listed in Bindings). This is the
// validator's "I deterministically verified these claim types on these
// surfaces" declaration.
type ValidationScope struct {
	// ClaimTypes are glob patterns matched against Claim.Type, e.g.
	// "wiki.page.*", "repository.commit.created", "release.tag.*".
	ClaimTypes []string `json:"claimTypes,omitempty"`

	// Bindings optionally restricts promotion to claims asserted against these
	// ExternalSystemBinding names. Empty = any binding.
	Bindings []string `json:"bindings,omitempty"`
}

// IsEmpty reports whether the scope matches nothing (no claim types). An empty
// scope is a no-op for promotion — backward compatible for gates that don't
// declare what they validate.
func (s ValidationScope) IsEmpty() bool {
	return len(s.ClaimTypes) == 0
}

func (s ValidationScope) matches(c v1alpha1.Claim) bool {
	if len(s.Bindings) > 0 && !contains(s.Bindings, c.Binding) {
		return false
	}
	for _, pat := range s.ClaimTypes {
		if MatchGlob(pat, c.Type) {
			return true
		}
	}
	return false
}

// Promotion records one claim promoted observed→validated by a validator.
type Promotion struct {
	// FromNodeID is the node that produced the (now-promoted) claim.
	FromNodeID string `json:"fromNodeID"`

	// ClaimIndex is the index of the claim within that node's envelope.
	ClaimIndex int `json:"claimIndex"`

	// Claim is the promoted claim (TrustClass is now validated).
	Claim v1alpha1.Claim `json:"claim"`

	// ValidatedBy is the validator node ID that promoted this claim.
	ValidatedBy string `json:"validatedBy"`
}

// Enforce applies the ADR-0004 trust boundary to an envelope in place. When the
// producing node is NON-deterministic, every claim it asserted as validated is
// demoted to observed and its ValidatedBy is cleared — a non-deterministic node
// cannot self-validate. Claims already observed, or claims from deterministic
// nodes, are untouched. Returns the number of claims demoted.
//
// This is the unconditional kernel guard: it runs on every synthesized envelope
// so the recorded orchestration history (ADR-0005) never contains a
// self-validated claim from an LLM/external tool.
func Enforce(deterministic bool, env *v1alpha1.NodeResultEnvelope) int {
	if deterministic || env == nil {
		return 0
	}
	demoted := 0
	for i := range env.Claims {
		if env.Claims[i].TrustClass == v1alpha1.ClaimTrustValidated {
			env.Claims[i].TrustClass = v1alpha1.ClaimTrustObserved
			env.Claims[i].ValidatedBy = ""
			demoted++
		}
	}
	return demoted
}

// Promote scans the accumulated run envelopes for observed claims matching the
// validator's scope and promotes them to validated, stamping ValidatedBy =
// validatorNodeID. Promotions are applied in place to the envelopes map. Returns
// the promotions performed. A no-op when the scope is empty (a deterministic
// node that declares no validation scope promotes nothing).
//
// Already-validated claims are not re-touched (idempotent). The validator's own
// envelope is included: a deterministic node may legitimately self-promote a
// claim it declared observed.
func Promote(envelopes map[string]v1alpha1.NodeResultEnvelope, validatorNodeID string, scope ValidationScope) []Promotion {
	if scope.IsEmpty() {
		return nil
	}
	var promotions []Promotion
	for nodeID, env := range envelopes {
		for i := range env.Claims {
			c := &env.Claims[i]
			if c.TrustClass != v1alpha1.ClaimTrustObserved {
				continue
			}
			if !scope.matches(*c) {
				continue
			}
			c.TrustClass = v1alpha1.ClaimTrustValidated
			c.ValidatedBy = validatorNodeID
			promotions = append(promotions, Promotion{
				FromNodeID:  nodeID,
				ClaimIndex:  i,
				Claim:       *c,
				ValidatedBy: validatorNodeID,
			})
		}
	}
	return promotions
}

// HasValidated reports whether any envelope carries at least one validated
// claim. Used by the Attempt phase machine (ADR-0005): a successful run that
// produced a validated claim promotes the Attempt to AttemptPhaseValidated.
func HasValidated(envs []v1alpha1.NodeResultEnvelope) bool {
	for _, env := range envs {
		for _, c := range env.Claims {
			if c.TrustClass == v1alpha1.ClaimTrustValidated {
				return true
			}
		}
	}
	return false
}

// MatchGlob reports whether s matches a glob pattern where '*' matches any run
// of characters (including empty). Intentionally simple (no '?', no character
// classes) — claim type patterns are like "wiki.page.*" or
// "repository.commit.created". Implements the classic two-way wildcard match.
func MatchGlob(pattern, s string) bool {
	pi, si := 0, 0
	star := -1
	match := 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			match = si
			pi++
		case pi < len(pattern) && pattern[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			match++
			si = match
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
