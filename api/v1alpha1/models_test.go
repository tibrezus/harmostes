package v1alpha1

import (
	"testing"
	"time"
)

// ResolveModel is the run-start model selection (#494): first window
// containing `now` wins (in the window's own timezone, midnight-wrap
// allowed), else the base model. A malformed window can never fail a run —
// it just never matches.
func TestAgentSpec_ResolveModel(t *testing.T) {
	// 23:00 in Paris is inside 16:00→02:00 Europe/Paris (midnight wrap).
	at2300Paris := time.Date(2026, 9, 13, 23, 0, 0, 0, paris())
	// 03:00 Paris is outside it.
	at0300Paris := time.Date(2026, 9, 14, 3, 0, 0, 0, paris())
	// The SAME instants in UTC prove the window is honored in the window's
	// own zone, not the evaluator's: 21:00Z = 23:00 Paris (in), 01:00Z = 03:00 Paris (out).
	at2100UTC := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	at0100UTC := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)

	spec := AgentSpec{
		Model: "litellm/ali/anthropic/qwen3.8-flash",
		Models: []ModelWindow{{
			Model: "litellm/ali/anthropic/deepseek-v4.1-flash",
			Start: "16:00",
			End:   "02:00",
			TZ:    "Europe/Paris",
		}},
	}

	if got := spec.ResolveModel(at2300Paris); got != "litellm/ali/anthropic/deepseek-v4.1-flash" {
		t.Errorf("23:00 Paris: got %q, want the window model", got)
	}
	if got := spec.ResolveModel(at0300Paris); got != spec.Model {
		t.Errorf("03:00 Paris: got %q, want the base model", got)
	}
	// Same instants evaluated against UTC clocks — the window's zone rules.
	if got := spec.ResolveModel(at2100UTC); got != "litellm/ali/anthropic/deepseek-v4.1-flash" {
		t.Errorf("21:00Z (23:00 Paris): got %q, want the window model", got)
	}
	if got := spec.ResolveModel(at0100UTC); got != spec.Model {
		t.Errorf("01:00Z (03:00 Paris): got %q, want the base model", got)
	}

	// Boundary: the window is [Start, End) — start inclusive, end exclusive.
	at1600 := time.Date(2026, 9, 13, 16, 0, 0, 0, paris())
	at0200 := time.Date(2026, 9, 14, 2, 0, 0, 0, paris())
	if got := spec.ResolveModel(at1600); got != "litellm/ali/anthropic/deepseek-v4.1-flash" {
		t.Errorf("16:00 sharp: got %q, want the window model", got)
	}
	if got := spec.ResolveModel(at0200); got != spec.Model {
		t.Errorf("02:00 sharp: got %q, want the base model (end exclusive)", got)
	}

	// First match wins — order is the contract.
	first := spec
	first.Models = []ModelWindow{
		{Model: "first", Start: "00:00", End: "23:59", TZ: "UTC"},
		{Model: "second", Start: "10:00", End: "11:00", TZ: "UTC"},
	}
	if got := first.ResolveModel(time.Date(2026, 9, 13, 10, 30, 0, 0, time.UTC)); got != "first" {
		t.Errorf("overlapping windows: got %q, want the first match", got)
	}
}

// A malformed window (unknown zone, bad HH:MM) must never fail a run: it
// cannot match, so the base model carries the schedule's weight.
func TestAgentSpec_ResolveModel_MalformedWindowsDegrade(t *testing.T) {
	spec := AgentSpec{
		Model: "base",
		Models: []ModelWindow{
			{Model: "bogus-zone", Start: "16:00", End: "02:00", TZ: "Mars/Olympus"},
			{Model: "bogus-time", Start: "25:99", End: "02:00", TZ: "UTC"},
		},
	}
	if got := spec.ResolveModel(time.Date(2026, 9, 13, 23, 0, 0, 0, time.UTC)); got != "base" {
		t.Errorf("malformed windows resolved %q, want the base model", got)
	}

	// No windows at all: the base model, always.
	bare := AgentSpec{Model: "base"}
	if got := bare.ResolveModel(time.Now()); got != "base" {
		t.Errorf("window-less spec resolved %q", got)
	}
}

// The template→instance overlay carries the schedule (field-wise, like
// model) — an instance inherits the template's windows unless it sets its own.
func TestApplyTemplateDefaults_CarriesModelSchedule(t *testing.T) {
	tmpl := &WorkflowTemplate{Spec: WorkflowTemplateSpec{
		Agent: AgentSpec{
			Model:  "base",
			Models: []ModelWindow{{Model: "windowed", Start: "16:00", End: "02:00", TZ: "Europe/Paris"}},
		},
	}}
	wf := &Workflow{}
	ApplyTemplateDefaults(wf, tmpl)
	if len(wf.Spec.Agent.Models) != 1 || wf.Spec.Agent.Models[0].Model != "windowed" {
		t.Fatalf("template schedule not overlaid: %+v", wf.Spec.Agent.Models)
	}

	// Instance-set schedule wins as a whole (no per-window merge).
	wf2 := &Workflow{Spec: WorkflowSpec{Agent: AgentSpec{
		Models: []ModelWindow{{Model: "instance-window", Start: "01:00", End: "03:00"}},
	}}}
	ApplyTemplateDefaults(wf2, tmpl)
	if len(wf2.Spec.Agent.Models) != 1 || wf2.Spec.Agent.Models[0].Model != "instance-window" {
		t.Errorf("instance schedule must win whole: %+v", wf2.Spec.Agent.Models)
	}
}

func paris() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err) // tzdata is in the test binary image
	}
	return loc
}
