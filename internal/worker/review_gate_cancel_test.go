package worker

// Cancel-on-supersede (#402): a claim the gate released as superseded/closed
// leaves its review Job RUNNING — this pass deletes it and finalizes the
// ledger before the dead-head review burns the run bound. The tests pin the
// contract edges: only cancellation-reason releases match, live (and revived)
// claims never do, horizon releases are spared on purpose, the knob can turn
// the pass off, a failed job list skips it (fail-closed polarity), and no
// dead-dispatch counter ever moves.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
)

// RunRecordAlias keeps the fixtures readable without a second import alias
// for the API package.
type RunRecordAlias = v1alpha1.RunRecord

// releasedClaimFixture builds a claim whose era the gate already released:
// the release marker is STAMPED (absence means live, r8 P1), the status
// ledger says Released with the given reason, and a run record is still
// "running" — the pre-cancel shape of a superseded in-flight review.
func releasedClaimFixture(wf *v1alpha1.Workflow, pr, sha, reason string, dispatchedAt time.Time) *v1alpha1.Attempt {
	at := claimFixture(wf, pr, sha, dispatchedAt.Add(-time.Minute), &dispatchedAt)
	at.UID = k8stypes.UID("attempt-uid-" + at.Name)
	at.Labels[v1alpha1.ReviewClaimLabel] = v1alpha1.ReviewClaimReleased
	at.Status.Review.Released = true
	at.Status.Review.ReleaseReason = reason
	at.Status.Phase = v1alpha1.AttemptPhaseReconciling
	at.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "running", StartedAt: metav1.NewTime(dispatchedAt)}}
	return at
}

// reviewJobFixture builds a live review Job owned by the attempt: the labels
// ListActiveJobs selects on, plus the controller ownerRef BuildJob sets —
// the cancel pass refuses Jobs whose controller owner does not match the
// named Attempt (forged/re-pointed label defence).
func reviewJobFixture(wf *v1alpha1.Workflow, at *v1alpha1.Attempt) *batchv1.Job {
	controller := true
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: at.Name + "-job", Namespace: at.Namespace,
		UID: k8stypes.UID("job-uid-" + at.Name),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "harmostes.dev/v1alpha1", Kind: "Attempt",
			Name: at.Name, UID: at.UID, Controller: &controller,
		}},
		Labels: map[string]string{
			"app.kubernetes.io/name": "harmostes",
			"harmostes.dev/workflow": wf.Name,
			v1alpha1.AttemptLabel:    at.Name,
		},
	}}
}

func jobExists(t *testing.T, ctx context.Context, deps GateDeps, wf *v1alpha1.Workflow, name string) bool {
	t.Helper()
	var j batchv1.Job
	err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: name}, &j)
	return err == nil
}

// The headline: a superseded in-flight review's Job is deleted and the
// attempt reaches a terminal phase — with NO dead-dispatch strike (the
// breaker must not spend budget on a decision the gate itself made).
func TestCancelOnSupersedeDeletesJobOfSupersededClaim(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#101", "deadbeef321", "superseded", disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("the superseded claim's Job must be deleted, not left burning the run bound")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseSuperseded {
		t.Fatalf("phase = %q, want superseded (no forever-reconciling husk)", got.Status.Phase)
	}
	if got.Status.Review.DeadDispatches != 0 {
		t.Fatalf("DeadDispatches = %d, want 0 — cancelling a decided-obsolete run is not a dead dispatch", got.Status.Review.DeadDispatches)
	}
	if got.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("DispatchLostReleases = %d, want 0 — the churn budget must not move", got.Status.Review.DispatchLostReleases)
	}
	if !got.Status.Review.Released || got.Status.Review.ReleaseReason != v1alpha1.ReleaseReasonSuperseded {
		t.Fatalf("release record mutated: Released=%v reason=%q", got.Status.Review.Released, got.Status.Review.ReleaseReason)
	}
	if len(got.Status.Runs) != 1 || got.Status.Runs[0].Phase != "failed" || got.Status.Runs[0].EndedAt.IsZero() {
		t.Fatalf("run record not finalized: %+v", got.Status.Runs)
	}
}

// The #410 trigger, end to end through the sweep: the claim was dispatched
// at a head the PR has since moved past (the forge answers with the NEW
// head), the run is fresh and well inside every bound — but its verdict can
// never land, so the gate supersedes it and the cancel pass deletes the Job
// in the SAME sweep. This is the ztphk/PR-2084 live case (24 minutes burned
// at a dead head because the dispatched branch had no head-move handling).
func TestSweepSupersedesDispatchedClaimOnHeadMove(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t) // serves the PR at deadbeef123
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-5 * time.Minute) // fresh dispatch, far inside DispatchTimeout
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#101", "cafe000", now.Add(-6*time.Minute), &disp)
	claim.UID = k8stypes.UID("attempt-uid-" + claim.Name)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("the stale-head Job must be deleted in the same sweep — the verdict could not land")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if !got.Status.Review.Released || got.Status.Review.ReleaseReason != v1alpha1.ReleaseReasonSuperseded {
		t.Fatalf("the moved-head release must be a supersession, got Released=%v reason=%q", got.Status.Review.Released, got.Status.Review.ReleaseReason)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseSuperseded {
		t.Fatalf("phase = %q, want superseded", got.Status.Phase)
	}
	if got.Status.Review.DeadDispatches != 0 || got.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("breaker budgets moved: DeadDispatches=%d DispatchLostReleases=%d — a supersession is not a death", got.Status.Review.DeadDispatches, got.Status.Review.DispatchLostReleases)
	}
	// The sweep must record the release even when a later section's failure
	// overwrites the aggregates (last-decision-wins): the durable proof is
	// the claim's own release record, not the aggregate tail.
	if !got.Status.Review.Released || got.Status.Review.ReleaseReason != v1alpha1.ReleaseReasonSuperseded {
		t.Fatalf("the moved-head release must be a supersession, got Released=%v reason=%q", got.Status.Review.Released, got.Status.Review.ReleaseReason)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseSuperseded {
		t.Fatalf("phase = %q, want superseded", got.Status.Phase)
	}
	if got.Status.Review.DeadDispatches != 0 || got.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("breaker budgets moved: DeadDispatches=%d DispatchLostReleases=%d — a supersession is not a death", got.Status.Review.DeadDispatches, got.Status.Review.DispatchLostReleases)
	}
}

// A closed PR's in-flight review cancels too, but the terminal phase is
// failed (the review is dead, not replaced).
func TestCancelOnSupersedeClosedReasonFailsPhase(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#102", "deadbeef432", v1alpha1.ReleaseReasonPRClosed, disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a closed PR's in-flight Job must be cancelled")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseFailed {
		t.Fatalf("phase = %q, want failed for a closed-PR cancellation", got.Status.Phase)
	}
	if got.Status.Review.DeadDispatches != 0 {
		t.Fatalf("DeadDispatches = %d, want 0", got.Status.Review.DeadDispatches)
	}
}

// Horizon releases are spared on purpose: that verdict may still be valid at
// the pinned head — ADR-0006 lets it post and the gate consumes it.
func TestCancelOnSupersedeSparesHorizonReleases(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#104", "deadbeef654", v1alpha1.ReleaseReasonHorizon, disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a horizon-released claim's Job must NOT be cancelled — the verdict may still land")
	}
}

// A live claim's Job is never touched: only the claim's own release passes
// decide liveness; the cancel pass reads the CURRENT attempt state, so a
// same-head revival (Released=false again) is invisible to it. The claim is
// pinned at the head the forge still serves — a MOVED head would make the
// gate itself supersede the claim (#410), which is a different test.
func TestCancelOnSupersedeSparesLiveClaims(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#105", "deadbeef123", disp.Add(-time.Minute), &disp)
	claim.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "running"}}
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a live claim's in-flight Job must never be cancelled")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase == v1alpha1.AttemptPhaseSuperseded || got.Status.Phase == v1alpha1.AttemptPhaseFailed {
		t.Fatalf("live claim phase mutated to %q", got.Status.Phase)
	}
}

// The chart knob turns the pass off entirely (pre-#402 behavior).
func TestCancelOnSupersedeDisabled(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#106", "deadbeef87a", "superseded", disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = true

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("with the knob off, the Job must be left alone (pre-#402 behavior)")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseReconciling {
		t.Fatalf("knob-off must also leave the ledger alone (the reconciling husk IS the pre-#402 behavior), got %q", got.Status.Phase)
	}
}

// Idempotence: a second sweep over the cancelled state is a clean no-op —
// the Job is gone from the snapshot after sweep 1, so nothing re-finalizes
// or re-counts.
func TestCancelOnSupersedeSecondSweepNoop(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#107", "deadbeef98b", "superseded", disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	for i := 1; i <= 2; i++ {
		if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("job resurrected — DeleteJob must be sticky in the fake client")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseSuperseded || got.Status.Review.DeadDispatches != 0 {
		t.Fatalf("second sweep mutated state: phase=%q dead=%d", got.Status.Phase, got.Status.Review.DeadDispatches)
	}
}

// A review the worker already recorded honestly (finished naturally between
// the job snapshot and the cancel pass) keeps its own outcome: the pass moves
// the stale phase but does NOT stamp the death-observer message over it.
func TestCancelOnSupersedePreservesWorkerWrittenOutcome(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#109", "deadbeef10d", "superseded", disp)
	claim.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "succeeded", EndedAt: metav1.NewTime(disp.Add(time.Minute))}}
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("the stale Job must still be cancelled")
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Message == "" || strings.Contains(got.Status.Message, "death observer") {
		t.Fatalf("expected the distinct phase-only note, got %q (the death-observer message must not be stamped over a worker-written outcome)", got.Status.Message)
	}
	if got.Status.Runs[0].Phase != "succeeded" {
		t.Fatalf("worker-written run outcome mutated: %+v", got.Status.Runs[0])
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseSuperseded {
		t.Fatalf("phase = %q, want superseded (the stale phase still moves)", got.Status.Phase)
	}
}

// The headline pairing (AC #5): one sweep arms the NEW head (labeled scan,
// CI green) AND deletes the dead head's Job — supersession and cancellation
// converge inside a single runGate call, not across sweeps.
func TestCancelOnSupersedeFastPathWithinOneSweep(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 101) // scan: PR #101 labeled, green at deadbeef123
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	// The OLD head's claim, released superseded by an earlier wake, Job still live.
	old := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#101", "oldsha111", "superseded", disp)
	oldJob := reviewJobFixture(wf, old)
	deps, ctx := gateEnv(t, wf, st, old, oldJob)
	deps.DisableCancelOnSupersede = false

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The same sweep armed the new head for dispatch.
	if len(out) != 1 || out[0].Envelope.HeadSHA != "deadbeef123" {
		t.Fatalf("the sweep must arm exactly the green new head, got %+v", out)
	}
	newObj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "deadbeef123", Source: "webhook"})
	var na v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: attempt.AttemptName(wf.Name, attempt.Identity(newObj))}, &na); err != nil {
		t.Fatalf("get new-head claim: %v", err)
	}
	if na.Status.Review == nil || na.Status.Review.Released {
		t.Fatal("the new-head claim must be live after the sweep")
	}
	// …and the old head's Job is already gone — no cross-sweep tail of waste.
	if jobExists(t, ctx, deps, wf, oldJob.Name) {
		t.Fatal("the dead head's Job must be cancelled in the SAME sweep that armed the new head")
	}
}

// A Job whose controller ownerRef points at a DIFFERENT attempt (re-pointed
// or forged label) is skipped — the label alone never earns a deletion.
func TestCancelOnSupersedeSkipsForgedOwner(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#111", "deadbeef21e", "superseded", disp)
	claim.UID = "the-real-attempt-uid"
	job := reviewJobFixture(wf, claim)
	job.OwnerReferences[0].UID = "a-different-attempt-uid" // re-pointed
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a Job owned by another attempt must not be deleted off a copied label")
	}
}

// A pointer-invalid release is bookkeeping-only: the underlying review may
// be alive and verdict-bearing, so the pass must not cancel it (r3 P4 —
// the overload that made "closed" destructive).
func TestCancelOnSupersedeSparesPointerInvalidReleases(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#112", "deadbeef32f", v1alpha1.ReleaseReasonPointerInvalid, disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a pointer-invalid release must not cancel the in-flight review")
	}
}

// The revival-window guard (r12 must-fix 1 shape): FinalizeCancelledClaim on
// a claim that is LIVE again (revived between the gate's read and the
// finalize) is a no-op — the ledger never tells a live review it is dead.
func TestFinalizeCancelledClaimNoopOnLiveClaim(t *testing.T) {
	clearTriggerEnv(t)
	wf := gateWorkflow()
	disp := time.Now().Add(-30 * time.Minute)
	live := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#113", "deadbeef43a", disp.Add(-time.Minute), &disp)
	live.Status.Phase = v1alpha1.AttemptPhaseReconciling
	live.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "running"}}
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, live)

	if err := attempt.FinalizeCancelledClaim(ctx, deps.Client, wf.Namespace, live.Name, v1alpha1.ReleaseReasonSuperseded); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: live.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseReconciling || got.Status.Runs[0].Phase != "running" {
		t.Fatalf("a live claim's ledger was mutated: phase=%q run=%q", got.Status.Phase, got.Status.Runs[0].Phase)
	}
}

// classifyRelease decides which reason strings authorize a DELETION — it
// must have direct coverage, not just transitive (r4 P8 gap 1). The table
// pins the prose-matching contract AND the trap the r4 review named: a
// "merged" standdown reason deliberately classifies as standdown (the
// verdict may still land), not as a cancellation.
func TestClassifyReleaseVocabulary(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pull request closed", v1alpha1.ReleaseReasonPRClosed},
		{"closed by author", v1alpha1.ReleaseReasonPRClosed},
		{"pull request merged", "standdown"}, // merged ≠ closed: the verdict may still land
		{"label absent (verdict posted — consumed)", "consumed"},
		{"horizon exceeded while ambiguous", v1alpha1.ReleaseReasonHorizon},
		{"dispatch presumed dead without a verdict", v1alpha1.ReleaseReasonDispatchTimeout},
		{"head moved while review in flight (dispatched at cafe000, PR now at deadbeef12) — verdict could not land", v1alpha1.ReleaseReasonSuperseded}, // #410
		{"fresh review request", "standdown"},
	}
	for _, tc := range cases {
		if got := classifyRelease(tc.in); got != tc.want {
			t.Errorf("classifyRelease(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The other half of the ownerRef defence: a Job with NO controller ref at
// all is skipped, not deleted (BuildJob always sets one, so this is the
// forged-object shape).
func TestCancelOnSupersedeSkipsOwnerlessJob(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#114", "deadbeef54b", "superseded", disp)
	job := reviewJobFixture(wf, claim)
	job.OwnerReferences = nil
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.DisableCancelOnSupersede = false

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("an ownerless Job must not be deleted off a copied label")
	}
}

// DeleteJob failure → the ledger stays untouched: a finalize without a
// delete would claim a death the Job never saw. The `continue` at the
// failure site is the guarantee; this pins it.
func TestCancelOnSupersedeDeleteFailureLeavesLedger(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#115", "deadbeef65c", "superseded", disp)
	claim.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "running"}}
	job := reviewJobFixture(wf, claim)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		WithRuntimeObjects(claim, job).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return fmt.Errorf("delete blip")
			}
			return cl.Delete(ctx, obj, opts...)
		}}).
		Build()
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf, DisableCancelOnSupersede: false}
	ctx := context.Background()

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Message != "" || got.Status.Runs[0].Phase != "running" {
		t.Fatalf("delete failed but the ledger was finalized anyway: msg=%q run=%q", got.Status.Message, got.Status.Runs[0].Phase)
	}
}

// Fail-closed: a JobList failure latches for the whole sweep — an observer
// that cannot see Jobs must not delete them. The next healthy sweep converges.
func TestCancelOnSupersedeJobListFailureSkips(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#108", "deadbeef09c", "superseded", disp)
	job := reviewJobFixture(wf, claim)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		WithRuntimeObjects(claim, job).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*batchv1.JobList); ok {
				return fmt.Errorf("api blip")
			}
			return cl.List(ctx, list, opts...)
		}}).
		Build()
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf, DisableCancelOnSupersede: false}
	ctx := context.Background()

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a failed job list must skip the cancel pass (unknown must fail closed)")
	}
}
