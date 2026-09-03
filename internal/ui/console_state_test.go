package ui

// The console state vocabulary is the UI's backbone: every chip, filter, and
// rank reads from groupState/wallState. These tests pin the classifiers —
// including the unknown-state fall-throughs the reviewers flagged as the
// silent-degrade hazard (#326).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The status filter (the feature the tabs render) is Go logic — tested at
// the handler level over the fake client: selection correctness, tab-count
// consistency, and unknown values behaving as "all".
func TestHandleAttemptList_StatusFilter(t *testing.T) {
	// review builds a claim whose DERIVED state is the given vocabulary.
	review := func(state, pr string) *v1alpha1.ReviewClaimStatus {
		switch state {
		case claimDispatchLost:
			return &v1alpha1.ReviewClaimStatus{PR: pr, Released: true, ReleaseReason: "dispatch-lost"}
		case claimInFlight:
			return &v1alpha1.ReviewClaimStatus{PR: pr, DispatchedAt: &metav1.Time{Time: time.Now()}}
		case claimVerdict:
			return &v1alpha1.ReviewClaimStatus{PR: pr, Released: true, ReleaseReason: "consumed"}
		default:
			return &v1alpha1.ReviewClaimStatus{PR: pr}
		}
	}
	mk := func(name, owner, phase string, rc *v1alpha1.ReviewClaimStatus) *v1alpha1.Attempt {
		return &v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "test-ns",
				Labels:            map[string]string{v1alpha1.OwnerLabel: owner},
				CreationTimestamp: metav1.Now(),
			},
			Spec: v1alpha1.AttemptSpec{WorkflowRef: "wf", Objective: v1alpha1.ObjectiveSpec{
				Kind: v1alpha1.ObjectiveKindPRReview,
			}},
			Status: v1alpha1.AttemptStatus{Phase: phase, Review: rc},
		}
	}

	s := adminTestServer(t,
		mk("attempt-wf-lost", "tibrez", "reconciling", review(claimDispatchLost, "1")),
		mk("attempt-wf-flight", "tibrez", "reconciling", review(claimInFlight, "2")),
		mk("attempt-wf-verdict", "tibrez", "validated", review(claimVerdict, "3")),
	)

	get := func(status string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/runs?window=all&status="+status, nil).WithContext(
			withIdentity(context.Background(), idWith("tibrez", "harmostes-admins")))
		rec := httptest.NewRecorder()
		s.handleAttemptList(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := get("failed")
	if code != http.StatusOK {
		t.Fatalf("status filter failed: %d", code)
	}
	if !strings.Contains(body, "attempt-wf-lost") || strings.Contains(body, "attempt-wf-flight") {
		t.Error("failed tab must select only dispatch-lost groups")
	}
	_, body = get("inflight")
	if !strings.Contains(body, "attempt-wf-flight") || strings.Contains(body, "attempt-wf-lost") {
		t.Error("inflight tab must select only in-flight groups")
	}
	_, body = get("verdicts")
	if !strings.Contains(body, "attempt-wf-verdict") || strings.Contains(body, "attempt-wf-lost") {
		t.Error("verdicts tab must select only verdict groups")
	}
	// Unknown values behave as "all": every group renders.
	_, body = get("garbage")
	for _, want := range []string{"attempt-wf-lost", "attempt-wf-flight", "attempt-wf-verdict"} {
		if !strings.Contains(body, want) {
			t.Errorf("unknown status must render all; missing %q", want)
		}
	}
}

// The breaker's evidence must be visible where the operator looks (#328):
// a claim with dead dispatches renders the counter, and a stood-down head
// names its escape hatches.
func TestAttemptDetail_RendersDeadDispatchCounter(t *testing.T) {
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{Name: "attempt-brk", Namespace: "test-ns",
			Labels:            map[string]string{v1alpha1.OwnerLabel: "tibrez"},
			CreationTimestamp: metav1.Now()},
		Spec: v1alpha1.AttemptSpec{WorkflowRef: "wf", Objective: v1alpha1.ObjectiveSpec{
			Kind: v1alpha1.ObjectiveKindPRReview}},
		Status: v1alpha1.AttemptStatus{Phase: v1alpha1.AttemptPhaseFailed,
			Message: "run ended without a verdict (dispatch-lost)",
			Review: &v1alpha1.ReviewClaimStatus{PR: "git.rezus.cloud/tibrez/rhesadox#1800",
				HeadSHA: "b41fb712", Released: true, ReleaseReason: "dispatch-lost",
				DeadDispatches: v1alpha1.MaxDeadDispatchesPerHead}},
	}
	wf := &v1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: "test-ns"}}
	s := adminTestServer(t, wf, att)
	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-brk", nil).WithContext(
		withIdentity(context.Background(), idWith("tibrez", "harmostes-admins")))
	req.SetPathValue("name", "attempt-brk")
	rec := httptest.NewRecorder()
	s.handleAttemptDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail render: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Dead Dispatches") || !strings.Contains(body, "3/3") {
		t.Error("dead-dispatch counter must render with the budget")
	}
	if !strings.Contains(body, "new push or re-label to retry") {
		t.Error("stood-down head must name the escape hatches")
	}
}
