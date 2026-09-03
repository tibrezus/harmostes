package ui

// The console state vocabulary is the UI's backbone: every chip, filter, and
// rank reads from groupState/wallState. These tests pin the classifiers —
// including the unknown-state fall-throughs the reviewers flagged as the
// silent-degrade hazard (#326).

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestClaimState_VocabularyIsTyped(t *testing.T) {
	// The classifier matches constants exactly; if claimState's copy drifts,
	// this catches it at the source.
	cases := []struct {
		reason string
		want   string
	}{
		{"consumed", claimVerdict},
		{"superseded", claimSuperseded},
		{"dispatch-timeout", claimExpired},
		{"dispatch-lost", claimDispatchLost},
		{"horizon", claimHorizon},
		{"anything-new", claimReleased},
	}
	for _, c := range cases {
		got := claimState(&v1alpha1.ReviewClaimStatus{Released: true, ReleaseReason: c.reason})
		if got != c.want {
			t.Errorf("claimState(released, %q) = %q, want %q", c.reason, got, c.want)
		}
	}
	if got := claimState(&v1alpha1.ReviewClaimStatus{DispatchedAt: &metav1.Time{}}); got != claimInFlight {
		t.Errorf("claimState(dispatched) = %q, want %q", got, claimInFlight)
	}
	if got := claimState(&v1alpha1.ReviewClaimStatus{}); got != claimQueued {
		t.Errorf("claimState(fresh) = %q, want %q", got, claimQueued)
	}
}

func TestGroupState_ExactMatching(t *testing.T) {
	mk := func(review bool, claim, phase string) attemptGroup {
		return attemptGroup{IsReview: review, ClaimState: claim, LatestPhase: phase}
	}
	cases := []struct {
		name string
		g    attemptGroup
		want string
	}{
		{"dispatch lost is failed-class", mk(true, claimDispatchLost, ""), "dispatch lost"},
		{"in flight", mk(true, claimInFlight, ""), "in flight"},
		{"verdict", mk(true, claimVerdict, ""), "verdict"},
		{"queued", mk(true, claimQueued, ""), "queued"},
		{"horizon falls to queued", mk(true, claimHorizon, ""), "queued"},
		{"unknown claim falls to queued, never a wrong state", mk(true, "some future state", ""), "queued"},
		{"failed phase", mk(false, "", "failed"), "failed"},
		{"reconciling phase", mk(false, "", "reconciling"), "reconciling"},
		{"validated phase", mk(false, "", "validated"), "validated"},
		{"superseded phase", mk(false, "", "superseded"), "superseded"},
		{"unknown phase passes through verbatim", mk(false, "", "draining"), "draining"},
	}
	for _, c := range cases {
		if got := groupState(c.g); got != c.want {
			t.Errorf("%s: groupState = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWallState_MatchesGroupState(t *testing.T) {
	// The wall and the runs list must speak identically for the same data.
	groups := []attemptGroup{
		{IsReview: true, ClaimState: claimDispatchLost},
		{IsReview: true, ClaimState: claimInFlight},
		{IsReview: true, ClaimState: claimVerdict},
		{IsReview: true, ClaimState: claimQueued},
		{LatestPhase: "failed"},
		{LatestPhase: "reconciling"},
		{LatestPhase: "validated"},
	}
	for _, g := range groups {
		w := wallGroup{IsReview: g.IsReview, ClaimState: g.ClaimState, Phase: g.LatestPhase}
		if wallState(w) != groupState(g) {
			t.Errorf("wallState(%+v) = %q, groupState = %q — vocabularies diverged", g, wallState(w), groupState(g))
		}
	}
}

func TestStateRank_FailedFirst(t *testing.T) {
	if !(stateRank("failed") < stateRank("in flight") &&
		stateRank("dispatch lost") < stateRank("reconciling") &&
		stateRank("reconciling") < stateRank("verdict") &&
		stateRank("verdict") < stateRank("superseded")) {
		t.Error("rank order must be: failed < in-flight < verdict < history")
	}
}

func TestChipState_KnownAndFallthrough(t *testing.T) {
	if got := chipState("failed"); got != "failed" {
		t.Errorf("chipState(failed) = %q", got)
	}
	if got := chipState("validated"); got != "validated" {
		t.Errorf("chipState(validated) = %q", got)
	}
	if got := chipState("totally-new-phase"); got != "reconciling" {
		t.Errorf("chipState(unknown) = %q, want reconciling (in-motion default)", got)
	}
}

func TestShortAttemptName(t *testing.T) {
	cases := map[string]string{
		"attempt-pr-review-rhesadox-4a2c1107f6806": "pr-review-rhesadox · 4a2c1107",
		"attempt-merge-sync-demo-e5f6":             "merge-sync-demo · e5f6",
		"attempt-solo":                             "solo",
		"no-prefix-at-all":                         "no-prefix-at · all", // non-CR input: shortens at the last dash
	}
	for in, want := range cases {
		if got := shortAttemptName(in); got != want {
			t.Errorf("shortAttemptName(%q) = %q, want %q", in, got, want)
		}
	}
}
