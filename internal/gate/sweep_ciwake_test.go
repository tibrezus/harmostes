package gate

// Host-native CI wakes (#556): a check_suite/workflow_run/status event
// carries (repo, sha) but no PR number. The wake rides GateWake.Repo, the
// sweep re-evaluates the ARMED claims (section A) and dispatches on green —
// the instant dispatch path that replaces the standing cron. These tests
// pin that contract at the gate boundary.

import (
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// A queued (armed, never-dispatched) claim whose CI has gone green since the
// arming sweep: the CI wake sweep must complete the dispatch — instantly,
// in this sweep — exactly like the cron sweep it replaces.
func TestCIWakeSweepDispatchesArmedClaimAtSha(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim)
	// The CI wake: PR empty — the payload had no PR number, only repo+sha.
	deps.Wake = GateWake{Repo: "git.rezus.cloud/tibrez/rhesadox", Action: v1alpha1.CIWakeAction, Revision: "deadbeef123"}

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("CI wake must dispatch the armed claim whose CI went green, got %d dispatches", len(out))
	}
	if out[0].Attempt != claim.Name {
		t.Fatalf("dispatch must complete the EXISTING attempt %s, got %s", claim.Name, out[0].Attempt)
	}
	if out[0].Envelope.HeadSHA != "deadbeef123" {
		t.Fatalf("envelope head = %q, want the armed sha", out[0].Envelope.HeadSHA)
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastWake != "git.rezus.cloud/tibrez/rhesadox@deadbeef123 (ci_completed)" {
		t.Fatalf("aggregates must record the CI wake, got %+v", st.last.ReviewReady)
	}
}

// A poll sweep (no wake) must not blank the previous wake from the status —
// the field explains what scheduled the run a human is looking at.
func TestPollSweepPreservesLastWake(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim)
	deps.Wake = GateWake{Repo: "git.rezus.cloud/tibrez/rhesadox", Action: v1alpha1.CIWakeAction, Revision: "deadbeef123"}
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("wake sweep: %v", err)
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastWake == "" {
		t.Fatalf("wake sweep must record LastWake, got %+v", st.last.ReviewReady)
	}

	// Second sweep, no wake at all (cron-era call shape / backoff pass).
	deps2, ctx2 := gateEnv(t, wf, st, claim)
	deps2.Wake = GateWake{}
	if _, err := RunReviewGateSweep(ctx2, deps2, wf); err != nil {
		t.Fatalf("poll sweep: %v", err)
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastWake == "" {
		t.Fatalf("a no-wake sweep must preserve the previous LastWake, got %+v", st.last.ReviewReady)
	}
}

// A CI wake for a DIFFERENT sha must not dispatch the claim — the wake is
// (repo, sha)-keyed: section A still re-evaluates (and the evaluator sees
// the armed sha's CI state), but the dispatch must carry the armed head.
// Here the API reports the label/CI of the armed head as green, so the
// dispatch proceeds with the ARMED sha — a wake for another sha of the same
// repo is a no-op trigger for this claim, never a wrong-head dispatch.
func TestCIWakeOtherShaStillArmedHeadDispatch(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim)
	deps.Wake = GateWake{Repo: "git.rezus.cloud/tibrez/rhesadox", Action: v1alpha1.CIWakeAction, Revision: "ffffffffffff"}

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, d := range out {
		if d.Envelope.HeadSHA != "deadbeef123" {
			t.Fatalf("a CI wake must never dispatch a head other than the armed sha, got %q", d.Envelope.HeadSHA)
		}
	}
}
