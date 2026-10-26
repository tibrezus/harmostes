package v1alpha1

import (
	"strings"
	"testing"
	"time"
)

// AgentProfile is the three-tier split's declarative surface (ADR-0011
// amendment 2026-10-26, #785). These tests pin the two consuming behaviors of
// phase 1a — profile-first model resolution and the compatibility statement —
// plus the inert-by-default contract (no profile → the spec path is
// byte-identical to the pre-profile behavior, which models_test.go pins).

func TestAgentProfileResolveModelFirstMatchWins(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC) // 17:30 UTC
	p := AgentProfileSpec{
		Models: []ModelWindow{
			{Model: "night-model", Start: "16:00", End: "02:00", TZ: "Europe/Paris"}, // 17:30 UTC = 18:30 Paris → inside
			{Model: "never-reached", Start: "00:00", End: "23:59", TZ: "UTC"},
		},
	}
	if got := p.ResolveModel(now); got != "night-model" {
		t.Fatalf("ResolveModel = %q, want night-model (first match wins)", got)
	}
}

func TestAgentProfileResolveModelNoMatchReturnsEmpty(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	p := AgentProfileSpec{
		Models: []ModelWindow{{Model: "night-model", Start: "16:00", End: "02:00", TZ: "UTC"}},
	}
	if got := p.ResolveModel(now); got != "" {
		t.Fatalf("ResolveModel = %q, want \"\" outside every window (caller falls back to the template)", got)
	}
	// No windows at all: same contract.
	if got := (AgentProfileSpec{}).ResolveModel(now); got != "" {
		t.Fatalf("empty profile ResolveModel = %q, want \"\"", got)
	}
}

func TestAgentProfileResolveModelMalformedWindowNeverMatches(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC)
	p := AgentProfileSpec{
		Models: []ModelWindow{
			{Model: "bad-zone", Start: "00:00", End: "23:59", TZ: "Mars/Olympus"},
			{Model: "bad-minutes", Start: "25:00", End: "26:00", TZ: "UTC"},
		},
	}
	if got := p.ResolveModel(now); got != "" {
		t.Fatalf("malformed windows must never match (#494 contract), got %q", got)
	}
}

// The inert-by-default chain, pinned end to end: profile absent/empty →
// AgentSpec.ResolveModel alone decides; profile window matched → it wins; no
// profile window matched → the spec's own resolution (windows, then base).
func TestResolutionChainProfileFirstTemplateFallback(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC) // 18:30 Paris
	spec := AgentSpec{
		Model:  "base-model",
		Models: []ModelWindow{{Model: "spec-window", Start: "00:00", End: "23:59", TZ: "UTC"}},
	}
	resolve := func(p *AgentProfileSpec) string {
		if p != nil {
			if m := p.ResolveModel(now); m != "" {
				return m
			}
		}
		return spec.ResolveModel(now)
	}

	if got := resolve(nil); got != "spec-window" {
		t.Fatalf("no profile: got %q, want the spec's own resolution %q (byte-identical to pre-profile behavior)", got, "spec-window")
	}
	profileHit := AgentProfileSpec{Models: []ModelWindow{{Model: "profile-model", Start: "16:00", End: "02:00", TZ: "Europe/Paris"}}}
	if got := resolve(&profileHit); got != "profile-model" {
		t.Fatalf("profile window matched: got %q, want profile-model (profile wins)", got)
	}
	profileMiss := AgentProfileSpec{Models: []ModelWindow{{Model: "profile-model", Start: "03:00", End: "04:00", TZ: "UTC"}}}
	if got := resolve(&profileMiss); got != "spec-window" {
		t.Fatalf("profile missed: got %q, want the template fallback %q", got, "spec-window")
	}
}

func TestAgentSpecResolveModelUnchangedAfterFactoring(t *testing.T) {
	// The factoring moved AgentSpec.ResolveModel's body into the shared
	// helper — pin one representative case on each branch so the refactor
	// cannot silently change the template surface.
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC)
	a := AgentSpec{Model: "base", Models: []ModelWindow{{Model: "w", Start: "16:00", End: "02:00", TZ: "Europe/Paris"}}}
	if got := a.ResolveModel(now); got != "w" {
		t.Fatalf("in-window: got %q, want w", got)
	}
	if got := a.ResolveModel(now.Add(12 * time.Hour)); got != "base" {
		t.Fatalf("out-of-window: got %q, want base", got)
	}
}

func TestCompatibilityCause(t *testing.T) {
	tests := []struct {
		name      string
		stmt      string
		kernel    string
		wantCause bool // true = refuses (cause non-empty)
		wantMalf  bool // true = the cause names the STATEMENT as malformed
	}{
		{"absent never refuses", "", "0.0.1", false, false},
		{"satisfied equal", "kernel >=1.4.0", "1.4.0", false, false},
		{"satisfied newer", "kernel >=1.4.0", "1.4.1", false, false},
		{"refused older", "kernel >=1.5.0", "1.4.1", true, false},
		{"v-prefixed kernel accepted", "kernel >=1.5.0", "v1.4.1", true, false},
		{"v-prefixed statement accepted", "kernel >=v1.4.0", "1.4.1", false, false},
		{"wrong resource name", "cluster >=1.5.0", "1.4.1", true, true},
		{"wrong operator", "kernel >1.5.0", "1.4.1", true, true},
		{"non-semver want", "kernel >=latest", "1.4.1", true, true},
		{"non-semver kernel", "kernel >=1.5.0", "dev", true, false},
		{"whitespace tolerated", "  kernel >= 1.5.0  ", "1.4.1", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := AgentProfileSpec{Compatibility: tc.stmt}
			cause := p.CompatibilityCause(tc.kernel)
			if tc.wantCause != (cause != "") {
				t.Fatalf("CompatibilityCause(%q, kernel %q) = %q, wantCause=%v", tc.stmt, tc.kernel, cause, tc.wantCause)
			}
			if tc.wantMalf && !strings.Contains(cause, "malformed") && !strings.Contains(cause, "not a semantic version") {
				t.Fatalf("cause %q must name the statement/shape as malformed", cause)
			}
		})
	}
}
