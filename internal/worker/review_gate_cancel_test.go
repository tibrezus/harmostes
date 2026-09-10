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
	deps.CancelOnSupersede = true

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
	claim := releasedClaimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#102", "deadbeef432", v1alpha1.ReleaseReasonClosed, disp)
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.CancelOnSupersede = true

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
	deps.CancelOnSupersede = true

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a horizon-released claim's Job must NOT be cancelled — the verdict may still land")
	}
}

// A live claim's Job is never touched: only the claim's own release passes
// decide liveness; the cancel pass reads the CURRENT attempt state, so a
// same-head revival (Released=false again) is invisible to it.
func TestCancelOnSupersedeSparesLiveClaims(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	disp := time.Now().Add(-30 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#105", "deadbeef789", disp.Add(-time.Minute), &disp)
	claim.Status.Runs = []RunRecordAlias{{Name: "run-1", Phase: "running"}}
	job := reviewJobFixture(wf, claim)
	deps, ctx := gateEnv(t, wf, st, claim, job)
	deps.CancelOnSupersede = true

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
	deps.CancelOnSupersede = false

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
	deps.CancelOnSupersede = true

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
	deps.CancelOnSupersede = true

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
	if got.Status.Message != "" {
		t.Fatalf("death-observer message stamped over a worker-written outcome: %q", got.Status.Message)
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
	deps.CancelOnSupersede = true

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
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf, CancelOnSupersede: true}
	ctx := context.Background()

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !jobExists(t, ctx, deps, wf, job.Name) {
		t.Fatal("a failed job list must skip the cancel pass (unknown must fail closed)")
	}
}
