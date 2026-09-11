package attempt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The dead-dispatch breaker (#328): dispatched reviews that die without a
// verdict accumulate on the claim; automatic re-arm is refused at the
// threshold, and only a new head or an explicit human re-request resets it.
// Observed live before the breaker: 19 dead runs over 12 hours on one PR
// (rhesadox#1800) — every mechanism individually correct, composed a
// livelock.

// armFor arms (or re-arms) and returns the deterministic attempt name —
// the handle every later claim write needs.
func armFor(t *testing.T, ctx context.Context, c client.Client, wf *v1alpha1.Workflow, pr, sha, label string, human bool) (string, error) {
	t.Helper()
	at, err := ArmClaim(ctx, c, wfScheme(t), wf, pr, sha, label, human)
	if err != nil {
		return "", err
	}
	return at.Name, nil
}

// resolveForTest re-resolves the deterministic attempt for (wf, sha) —
// the same key ArmClaim uses — so tests read back persisted state.
func resolveForTest(t *testing.T, ctx context.Context, c client.Client, wf *v1alpha1.Workflow, sha string) (*v1alpha1.Attempt, error) {
	t.Helper()
	obj := DeriveObjective(wf, TriggerContext{Revision: sha})
	a, _, err := ResolveOrCreate(ctx, c, obj, ResolveOptions{
		Namespace:   "harmostes",
		WorkflowRef: "harmostes/" + wf.Name,
		Scheme:      wfScheme(t),
	})
	return a, err
}

func TestArmClaim_BreakerOpensAndResets(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#1800"
	const sha = "b41fb712deadbeef"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}

	// Three dispatched deaths (job-death / dispatch-timeout releases).
	for i := 1; i <= v1alpha1.MaxDeadDispatchesPerHead; i++ {
		if err := MarkClaimDispatched(ctx, c, "harmostes", name); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
		if _, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-lost"); err != nil {
			t.Fatalf("dead release %d: %v", i, err)
		}
		// Re-arm between cycles (what the backlog sweep does).
		if i < v1alpha1.MaxDeadDispatchesPerHead {
			if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); err != nil {
				t.Fatalf("re-arm %d: %v", i, err)
			}
		}
	}

	// The breaker refuses the next automatic arm...
	_, err = armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if !errors.Is(err, ErrDeadDispatchBreaker) {
		t.Fatalf("automatic arm at threshold: err = %v, want ErrDeadDispatchBreaker", err)
	}
	if !strings.Contains(err.Error(), "explicit label request") {
		t.Errorf("breaker error must name the escape hatches: %v", err)
	}

	// ...the refused arm must not clobber the claim state.
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := a.Status.Review.DeadDispatches; got != v1alpha1.MaxDeadDispatchesPerHead {
		t.Errorf("dead dispatches after refused arm = %d, want %d (evidence preserved)", got, v1alpha1.MaxDeadDispatchesPerHead)
	}
	if !a.Status.Review.Released {
		t.Error("refused arm must leave the released state untouched")
	}

	// An explicit human re-request (re-label) IS the override: arm succeeds,
	// counter resets, phase returns to reconciling.
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", true); err != nil {
		t.Fatalf("human-override arm: %v", err)
	}
	a, _ = resolveForTest(t, ctx, c, wf, sha)
	if got := a.Status.Review.DeadDispatches; got != 0 {
		t.Errorf("human override must reset the counter, got %d", got)
	}
	if a.Status.Phase != v1alpha1.AttemptPhaseReconciling {
		t.Errorf("re-armed phase = %q, want reconciling", a.Status.Phase)
	}
}

func TestArmClaim_HeadChangeResetsBreaker(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#1801"

	name, err := armFor(t, ctx, c, wf, pr, "sha-v1", "needs-review", false)
	if err != nil {
		t.Fatalf("arm v1: %v", err)
	}
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead; i++ {
		_ = MarkClaimDispatched(ctx, c, "harmostes", name)
		_, _, _ = ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-timeout")
		_, _ = armFor(t, ctx, c, wf, pr, "sha-v1", "needs-review", false)
	}
	if _, err := armFor(t, ctx, c, wf, pr, "sha-v1", "needs-review", false); !errors.Is(err, ErrDeadDispatchBreaker) {
		t.Fatalf("breaker should be open at v1: %v", err)
	}

	// A new push is a new claim: fresh counter, no breaker.
	if _, err := armFor(t, ctx, c, wf, pr, "sha-v2", "needs-review", false); err != nil {
		t.Fatalf("arm v2 after head change: %v", err)
	}
	a, _ := resolveForTest(t, ctx, c, wf, "sha-v2")
	if a.Status.Review.HeadSHA != "sha-v2" || a.Status.Review.DeadDispatches != 0 {
		t.Errorf("v2 claim = %+v, want fresh counter at sha-v2", a.Status.Review)
	}
}

func TestReleaseClaimDead_LedgerFinalization(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()

	name, err := armFor(t, ctx, c, wf, "git.rezus.cloud/tibrez/rhesadox#1802", "sha-x", "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	// The worker records the run start, then is SIGKILLed at the run bound —
	// the record stays "running" and the phase stays reconciling.
	_ = RecordRunStarted(ctx, c, "harmostes", name, "run-dead-1")
	_ = MarkClaimDispatched(ctx, c, "harmostes", name)

	if _, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-lost"); err != nil {
		t.Fatalf("dead release: %v", err)
	}

	a, _ := resolveForTest(t, ctx, c, wf, "sha-x")
	r := a.Status.Review
	if !r.Released || r.ReleaseReason != "dispatch-lost" || r.DeadDispatches != 1 {
		t.Errorf("claim after death = released:%v reason:%q dead:%d", r.Released, r.ReleaseReason, r.DeadDispatches)
	}
	if a.Status.Phase != v1alpha1.AttemptPhaseFailed {
		t.Errorf("phase = %q, want failed (the gate is the death observer)", a.Status.Phase)
	}
	if !strings.Contains(a.Status.Message, "run ended without a verdict") {
		t.Errorf("message = %q, want the honest no-verdict reason", a.Status.Message)
	}
	for _, run := range a.Status.Runs {
		if run.Name == "run-dead-1" && run.Phase != "failed" {
			t.Errorf("stale running record not finalized: %+v", run)
		}
	}
}

func TestReleaseClaimDead_PreservesWorkerWrittenFailure(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()

	name, err := armFor(t, ctx, c, wf, "git.rezus.cloud/tibrez/rhesadox#1803", "sha-y", "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	_ = RecordRunStarted(ctx, c, "harmostes", name, "run-dead-2")
	_ = MarkClaimDispatched(ctx, c, "harmostes", name)
	// The worker exits gracefully: it writes its own specific outcome.
	_ = patchAttemptStatus(ctx, c, "harmostes", name, func(s *v1alpha1.AttemptStatus) {
		s.Phase = v1alpha1.AttemptPhaseFailed
		s.Message = "node agent failed: agent failed after 4 attempt(s), 18166 in / 4708 out"
		for i := range s.Runs {
			if s.Runs[i].Name == "run-dead-2" {
				s.Runs[i].Phase = "failed"
			}
		}
	})

	if _, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-timeout"); err != nil {
		t.Fatalf("dead release: %v", err)
	}

	a, _ := resolveForTest(t, ctx, c, wf, "sha-y")
	if !strings.Contains(a.Status.Message, "4 attempt(s)") {
		t.Errorf("worker-written message destroyed: %q", a.Status.Message)
	}
	if a.Status.Review.DeadDispatches != 1 {
		t.Errorf("dead dispatches = %d, want 1 (timeout death still counts)", a.Status.Review.DeadDispatches)
	}
}

// Negative control: if the timeout death stopped counting, the breaker would
// never open through the dispatch-timeout path — this test fails.
func TestReleaseClaimDead_TimeoutDeathsCountTowardBreaker(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#1804"
	const sha = "sha-z"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead; i++ {
		_ = MarkClaimDispatched(ctx, c, "harmostes", name)
		if _, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-timeout"); err != nil {
			t.Fatalf("dead release %d: %v", i+1, err)
		}
		if i < v1alpha1.MaxDeadDispatchesPerHead-1 {
			_, _ = armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
		}
	}
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); !errors.Is(err, ErrDeadDispatchBreaker) {
		t.Fatalf("breaker must open on dispatch-timeout deaths alone: %v", err)
	}
}

func TestReleaseClaimDead_NeverDispatchedDoesNotCount(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()

	name, err := armFor(t, ctx, c, wf, "git.rezus.cloud/tibrez/rhesadox#1805", "sha-q", "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Armed-queued death (sweep died before its Job landed): DispatchedAt
	// is nil — infrastructure weather, not a dead review. The primitive's
	// guard makes the mis-call harmless by construction.
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead+1; i++ {
		if _, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-lost"); err != nil {
			t.Fatalf("release %d: %v", i+1, err)
		}
		_, _ = armFor(t, ctx, c, wf, "git.rezus.cloud/tibrez/rhesadox#1805", "sha-q", "needs-review", false)
	}
	a, _ := resolveForTest(t, ctx, c, wf, "sha-q")
	if a.Status.Review.DeadDispatches != 0 {
		t.Errorf("never-dispatched deaths must not count, got %d", a.Status.Review.DeadDispatches)
	}
}

// TestArmClaim_ReusesSameHeadEra (#343 fix 1): arming the same (pr, head)
// again must REFUSE to create a fresh era — the live claim is reused with
// its ArmedSince intact (the horizon and verdict window stay anchored), and
// a dispatch-lost-released claim is revived into the same era rather than
// replaced. The observed churn: every sweep supersede-recreated the claim,
// resetting ArmedSince, so neither the verdict window nor the horizon could
// ever fire.
func TestArmClaim_ReusesSameHeadEra(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#1864"
	const sha = "5472b055cafe0001"

	name1, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}
	a1, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if a1.Name != name1 {
		t.Fatalf("identity drift: %s != %s", a1.Name, name1)
	}
	// Backdate the era clock EXPLICITLY: metav1.Time has second granularity,
	// so two arms in the same wall-clock second would make a reset
	// indistinguishable from an anchor — the vacuous pass that shipped r2.
	backdate := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	a1.Status.Review.ArmedSince = &backdate
	if err := c.Status().Update(ctx, a1); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// firstArm = the BACKDATED clock: a revival must preserve exactly this.
	firstArm := backdate.Time

	// Sweep 2: same (pr, head) → the SAME attempt, ArmedSince NOT reset.
	name2, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("second arm: %v", err)
	}
	if name2 != name1 {
		t.Fatalf("churn: second arm created %s instead of reusing %s", name2, name1)
	}
	a2, _ := resolveForTest(t, ctx, c, wf, sha)
	// Second-granularity compare: serialization strips sub-second precision,
	// but a CLOCK RESET still fails this — the anchored value is 1h in the
	// past, a reset is "now". Not vacuous (r3 P8a): the eras differ by an hour.
	trunc := func(t time.Time) time.Time { return t.Truncate(time.Second) }
	if !trunc(a2.Status.Review.ArmedSince.Time).Equal(trunc(firstArm)) {
		t.Fatalf("ArmedSince slid: %v → %v (the horizon/verdict window must stay anchored)", firstArm, a2.Status.Review.ArmedSince.Time)
	}

	// The never-dispatched release (reDispatchGrace) must NOT start a new
	// era either: reviving the released claim keeps the original clock.
	if err := ReleaseClaim(ctx, c, "harmostes", name1, "dispatch-lost"); err != nil {
		t.Fatalf("release: %v", err)
	}
	name3, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm after dispatch-lost release: %v", err)
	}
	if name3 != name1 {
		t.Fatalf("churn: arm after dispatch-lost created %s instead of reviving %s", name3, name1)
	}
	a3, _ := resolveForTest(t, ctx, c, wf, sha)
	if a3.Status.Review.Released {
		t.Fatal("revived claim must be live")
	}
	if !trunc(a3.Status.Review.ArmedSince.Time).Equal(trunc(firstArm)) {
		t.Fatalf("ArmedSince slid across release: %v → %v", firstArm, a3.Status.Review.ArmedSince.Time)
	}
}

// TestArmClaim_HorizonDismissalChurnGuard (#343 fix 3): after the horizon
// dismisses a head's era, automatic re-arm of the SAME head is refused; a
// human request overrides; a new head is a new era and proceeds.
func TestArmClaim_HorizonDismissalChurnGuard(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	// The horizon leg is the operator's number (r12 P1): nil reviewReady
	// must skip the leg rather than silently borrow the 6h default — so the
	// test configures one explicitly, like the gate's workflows do.
	wf.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Horizon: "6h"}
	const pr = "git.rezus.cloud/tibrez/rhesadox#1864"
	const sha = "5472b055cafe0002"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}
	if err := ReleaseClaim(ctx, c, "harmostes", name, "horizon"); err != nil {
		t.Fatalf("horizon release: %v", err)
	}
	// Backdate the released era (r5: a same-second clock made the
	// fresh-clock assertion below vacuous — it passed for the fixture's
	// microseconds, not because the code reset anything).
	rel, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	old := metav1.NewTime(time.Now().Add(-7 * time.Hour))
	rel.Status.Review.ArmedSince = &old
	if err := c.Status().Update(ctx, rel); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	_, err = armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if !errors.Is(err, ErrRecentlyDismissed) {
		t.Fatalf("want ErrRecentlyDismissed, got %v", err)
	}

	// Human request overrides — and yields a WORKING era: fresh clock (the
	// old one was already past the horizon), not a revival of the expired
	// one (r2 F1: the override was accepted before, but the revived era was
	// born expired and stood down again on the next sweep).
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", true); err != nil {
		t.Fatalf("human re-request must override the guard: %v", err)
	}
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(a.Status.Review.ArmedSince.Time); age > time.Minute {
		t.Fatalf("overridden era must start a FRESH clock, got ArmedSince age %v", age)
	}

	// The dismissal is PERSISTED (r11 must-fix 1): the revival that answered
	// it cleared Released/ReleaseReason, yet the next AUTOMATIC arm within
	// the horizon window is still refused. This is the leg the old era-state
	// check could not express — it fired exactly once, on the arm right
	// after the release, and #343's criterion rode the counter alone after
	// that. Mutation check: reverting the guard to Released&&horizon turns
	// this assertion red.
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); !errors.Is(err, ErrRecentlyDismissed) {
		t.Fatalf("automatic re-arm after human revival must stay refused while the persisted dismissal is fresh, got %v", err)
	}

	// Time is the dismissal's only eraser: past the horizon the automatic
	// path proceeds (still bounded by the counter leg).
	a, err = resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	expired := metav1.NewTime(time.Now().Add(-7 * time.Hour))
	a.Status.Review.DismissedAt = &expired
	if err := c.Status().Update(ctx, a); err != nil {
		t.Fatalf("expire dismissal: %v", err)
	}
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("automatic arm must proceed once the persisted dismissal expires, got %v", err)
	}

	// A different head is a new era — no guard.
	const newHead = "aaaaaaaaface0003"
	if _, err := armFor(t, ctx, c, wf, pr, newHead, "needs-review", false); err != nil {
		t.Fatalf("new head must arm past the guard: %v", err)
	}
}

// TestArmClaim_RevivedEraClearsPhantomDispatch (#344 F2): the job-death pass
// releases dispatched claims as dispatch-lost WITH the death counted; a
// revival must not carry the dead dispatch forward — DispatchedAt cleared
// (no phantom capacity/breaker strike), the death count kept (honest
// ledger), ArmedSince KEPT (anchored — r6 pins the r3 contract: a
// dispatch-lost revival must NOT reset the clock, or the verdict window
// stretches with every infra blip; a HUMAN re-request is the reset path).
func TestArmClaim_RevivedEraClearsPhantomDispatch(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#1864"
	const sha = "5472b055cafe0004"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}
	if err := MarkClaimDispatched(ctx, c, "harmostes", name); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Backdate the armed clock so "KEPT" is observable.
	armed, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	anchored := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	past := metav1.NewTime(anchored)
	armed.Status.Review.ArmedSince = &past
	if err := c.Status().Update(ctx, armed); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	recorded, _, err := ReleaseClaimDead(ctx, c, "harmostes", name, "dispatch-lost")
	if err != nil || !recorded {
		t.Fatalf("dead release: recorded=%v err=%v", recorded, err)
	}

	name2, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("revival arm: %v", err)
	}
	if name2 != name {
		t.Fatalf("same (pr, head) must reuse the era, got %s", name2)
	}
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	r := a.Status.Review
	if r.Released {
		t.Fatal("revived era must be live")
	}
	if r.DispatchedAt != nil {
		t.Fatalf("revived era carries a phantom dispatch: DispatchedAt=%v", r.DispatchedAt)
	}
	if r.DeadDispatches != 1 {
		t.Fatalf("the recorded death must stay in the ledger, got %d", r.DeadDispatches)
	}
	// r6 (r5 mutation probe): the automatic dispatch-lost revival KEEPS
	// the anchored clock — assert the KEPT backdated value explicitly, so
	// a reset regression fails here instead of hiding behind same-second
	// fixture timing. Dispatched deaths ride the BREAKER counter
	// (DeadDispatches); the never-dispatched counter is untouched here.
	if r.DispatchLostReleases != 0 {
		t.Fatalf("dispatched deaths must not bump the never-dispatched counter, got %d", r.DispatchLostReleases)
	}
	if got := r.ArmedSince.Time; got.Before(anchored.Add(-time.Second)) || got.After(anchored.Add(time.Second)) {
		t.Fatalf("dispatch-lost revival must KEEP the anchored clock %v, got %v", anchored, got)
	}
}

// ── r4 BLOCKER: bounded dispatch-lost reuse — the release/revive cycle
// must converge into a refused arm within MaxDispatchLostReleases sweeps,
// and a request-shaped wake resets the budget. ──
func TestArmClaim_DispatchLostCycleConverges(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2001"
	const sha = "5472b055cafe2001"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm 0: %v", err)
	}
	// Simulate the never-dispatched release/revive cycle. Era reuse must
	// keep the SAME attempt (no attempt spam) and each cycle accumulates
	// the consecutive-release counter. The Nth release is the LAST that
	// may be revived: the arm after it is the refusal (convergence).
	for i := 1; i < v1alpha1.MaxDispatchLostReleases; i++ {
		if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
		revived, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
		if err != nil {
			t.Fatalf("cycle %d revive: %v", i, err)
		}
		if revived != name {
			t.Fatalf("cycle %d: era reuse must keep the same attempt, got %s", i, revived)
		}
	}
	// The final release exhausts the budget; the next automatic arm is refused.
	if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
		t.Fatalf("final release: %v", err)
	}
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); !errors.Is(err, ErrChurnBudgetExhausted) {
		t.Fatalf("want ErrChurnBudgetExhausted (converged after %d releases), got %v", v1alpha1.MaxDispatchLostReleases, err)
	}

	// Counter visible on the claim (the "3 consecutive releases" is
	// provable from the object, not just from behavior).
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := a.Status.Review.DispatchLostReleases; got != v1alpha1.MaxDispatchLostReleases {
		t.Fatalf("DispatchLostReleases = %d, want %d", got, v1alpha1.MaxDispatchLostReleases)
	}

	// A request-shaped wake resets the budget (fix 3's contract).
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", true); err != nil {
		t.Fatalf("human override: %v", err)
	}
	a, _ = resolveForTest(t, ctx, c, wf, sha)
	if got := a.Status.Review.DispatchLostReleases; got != 0 {
		t.Fatalf("counter after human wake = %d, want 0", got)
	}
}

// ── r7 P2: the release counter is per-POINTER. Attempt identity carries no
// PR number, so two pointers sharing one head resolve to ONE attempt
// object — a pointer change must reset the counter, or the second
// pointer's first automatic arm is refused on the first pointer's churn
// evidence (the "labeled PR not reviewed" symptom #328/#343 prevent). ──
func TestArmClaim_PointerChangeResetsDispatchLostCounter(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const prA = "git.rezus.cloud/tibrez/rhesadox#2100"
	const prB = "git.rezus.cloud/tibrez/rhesadox#2101"
	const sha = "5472b055cafe2100" // one head, two PR numbers (closed + re-opened)

	name, err := armFor(t, ctx, c, wf, prA, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm A: %v", err)
	}
	// Pointer A burns its budget: MaxDispatchLostReleases consecutive
	// never-dispatched releases, revived between each (the churn cycle).
	for i := 0; i < v1alpha1.MaxDispatchLostReleases; i++ {
		if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
			t.Fatalf("release %d: %v", i+1, err)
		}
		if i < v1alpha1.MaxDispatchLostReleases-1 {
			if _, err := armFor(t, ctx, c, wf, prA, sha, "needs-review", false); err != nil {
				t.Fatalf("revive %d: %v", i+1, err)
			}
		}
	}
	// The converged guard: pointer A's automatic re-arm is refused.
	if _, err := armFor(t, ctx, c, wf, prA, sha, "needs-review", false); !errors.Is(err, ErrChurnBudgetExhausted) {
		t.Fatalf("pointer A exhausted budget must refuse auto re-arm, got %v", err)
	}

	// Pointer B (same head, different PR number) asks for review. Its FIRST
	// automatic arm must succeed: it produced none of the refusal evidence.
	atB, err := armFor(t, ctx, c, wf, prB, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("pointer B first auto arm must not inherit A's churn evidence: %v", err)
	}
	if atB != name {
		t.Fatalf("same head must resolve to the same attempt object, got %s want %s", atB, name)
	}
	a, _ := resolveForTest(t, ctx, c, wf, sha)
	if a.Status.Review.PR != prB {
		t.Fatalf("claim must now carry pointer B's PR, got %q", a.Status.Review.PR)
	}
	if got := a.Status.Review.DispatchLostReleases; got != 0 {
		t.Fatalf("pointer change must reset the release counter, got %d", got)
	}
}

// (#353 finding 2) TestArmClaim_TieBreakPrefersLiveEra was removed: with
// pointer-local identity there is only ever one object per head, so no era
// tie-break exists to test — the release→revive-into-the-same-attempt
// shape it actually pinned is covered by TestArmClaim_ReusesSameHeadEra.

// TestLiveReviewClaims_ExcludesWrongKindAndLabelless pins the kind filter's
// EXCLUSION side (r11 pillar 8): the objective-kind leg is client-side by
// design (must-fix 2 — server-side would couple the bound to the worker
// image's rollout), so this is the assertion that keeps it honest: a
// wrong-kind or kind-less Attempt sharing the workflow label must NOT
// enter capacity arithmetic or supersede.
func TestLiveReviewClaims_ExcludesWrongKindAndLabelless(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2202"
	const sha = "5472b055cafe2202"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}

	mk := func(nameSuffix, kind string, withKind bool) *v1alpha1.Attempt {
		labels := map[string]string{
			v1alpha1.WorkflowLabel: wf.Name,
		}
		if withKind {
			labels[v1alpha1.ObjectiveKindLabel] = kind
		}
		a := &v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-" + nameSuffix,
				Namespace: "harmostes",
				Labels:    labels,
			},
			Spec: v1alpha1.AttemptSpec{
				WorkflowRef: "harmostes/" + wf.Name,
			},
		}
		a.Status.Review = &v1alpha1.ReviewClaimStatus{PR: pr, HeadSHA: "other"}
		return a
	}
	wrong := mk("wrong-kind", "fork-sync", true)
	if err := c.Create(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	kindless := mk("no-kind", "", false)
	if err := c.Create(ctx, kindless); err != nil {
		t.Fatal(err)
	}

	claims, err := LiveReviewClaims(ctx, c, wf)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(claims) != 1 || claims[0].Name != name {
		t.Fatalf("wrong-kind and kind-less Attempts must be excluded client-side, got %d claim(s): %+v", len(claims), claims)
	}
}

// TestArmClaim_LabelFailureAbortsBeforeStatusCommit pins the arm's write
// order (r8 P1): the live-marker removal is RV-preconditioned and runs
// BEFORE the status patch, so a failing label write aborts the arm
// PRE-COMMIT — the claim stays released (invisible, harmless) instead of
// being stranded live-but-unlabeled (invisible to the gate forever,
// DispatchLostReleases frozen below the breaker, pointer never re-armed).
func TestArmClaim_LabelFailureAbortsBeforeStatusCommit(t *testing.T) {
	ctx := context.Background()
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2201"
	const sha = "5472b055cafe2201"

	// Build the client manually: first arm lands (with the released marker
	// set by a release), then every metadata patch fails.
	c := newFakeClient(t)
	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Wrap: metadata (main-resource) patches fail from here on; status
	// subresource patches keep working so the failure isolates the label.
	failing := &labelFailClient{Client: c}
	if _, err := ArmClaim(ctx, failing, wfScheme(t), wf, pr, sha, "needs-review", false); err == nil {
		t.Fatal("arm must fail when the live-marker removal fails")
	}
	// PRE-COMMIT proof: the status still says released — nothing leaked.
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Status.Review.Released {
		t.Fatal("failed label write must abort BEFORE the status patch — claim must remain released")
	}
}

// labelFailClient fails every main-resource Patch (metadata: the release
// marker) while passing status-subresource patches through.
type labelFailClient struct {
	client.Client
}

func (f *labelFailClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return errors.New("label write forced-failure (r8 P1 test)")
}

// TestMarkClaimReleased_SkipsLiveStatus (#353 finding 1): ReleaseClaim
// commits status Released=true, THEN stamps the marker. The marker write
// must be conditional on a FRESH status read — stamping a claim whose
// status was revived (Released=false) between the two writes would make a
// live claim invisible to LiveReviewClaims forever (liveDispatched
// undercounts → over-dispatch). This test drives the conditional directly
// (live status, marker absent); it does NOT reproduce the interleaving
// itself.
func TestMarkClaimReleased_SkipsLiveStatus(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2210"
	const sha = "5472b055cafe2210"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}

	// The race shape: the revival already committed (status live, marker
	// absent) when the delayed release's marker write runs.
	if err := markClaimReleased(ctx, c, "harmostes", name); err != nil {
		t.Fatalf("markClaimReleased: %v", err)
	}
	// Re-Get AFTER the call: a pre-call snapshot would make the label
	// assertion true regardless of what markClaimReleased does (#353
	// finding 1 — the old tautology).
	var fresh v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "harmostes", Name: name}, &fresh); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Labels[v1alpha1.ReviewClaimLabel]; got != "" {
		t.Fatalf("a live-status claim must not gain the release marker, got %q", got)
	}
	claims, err := LiveReviewClaims(ctx, c, wf)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cl := range claims {
		if cl.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatal("INVISIBLE LIVE CLAIM: status says live but the marker hid it from the gate's list")
	}
}

// TestMarkClaimReleased_StatusReleasedMarkerAbsent (direction B): the
// reverse divergence — status says released, marker absent — must stay
// excluded from the live list (the client-side Released re-check), and a
// legitimate release after a status commit still stamps the marker.
func TestMarkClaimReleased_StatusReleasedMarkerAbsent(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2211"
	const sha = "5472b055cafe2211"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
		t.Fatalf("release: %v", err)
	}
	claims, err := LiveReviewClaims(ctx, c, wf)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("status-released claims must stay excluded even while the marker is mid-flight, got %d", len(claims))
	}
}

// TestArmClaim_CreateRaceLoserDoesNotInheritEvidence (r12 P4/P8): the
// AlreadyExists re-get makes the loser of a create race arm the WINNER's
// object — whose evidence (counters, DismissedAt) belongs to a DIFFERENT
// pointer. sameClaim is the only guard; this pins it: the loser's arm runs
// the pointer-change reset, not an inheritance.
func TestArmClaim_CreateRaceLoserDoesNotInheritEvidence(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	wf.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Horizon: "6h"}
	const prWinner = "git.rezus.cloud/tibrez/rhesadox#2212"
	const prLoser = "git.rezus.cloud/tibrez/rhesadox#2213"
	const sha = "5472b055cafe2212"

	// The winner's era, churned to the refusal boundary.
	winnerName, err := armFor(t, ctx, c, wf, prWinner, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("winner arm: %v", err)
	}
	for i := 0; i < v1alpha1.MaxDispatchLostReleases; i++ {
		if err := ReleaseClaim(ctx, c, "harmostes", winnerName, v1alpha1.ReleaseReasonDispatchLost); err != nil {
			t.Fatalf("winner release %d: %v", i+1, err)
		}
	}

	// The loser resolves the SAME derived name (same head) with a DIFFERENT
	// PR: the AlreadyExists re-get hands it the winner's object, and
	// sameClaim=false must reset the winner's evidence instead of
	// inheriting it — the loser's arm must SUCCEED.
	loserName, err := armFor(t, ctx, c, wf, prLoser, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("loser arm must not inherit the winner's churn evidence: %v", err)
	}
	if loserName != winnerName {
		t.Fatalf("same head must resolve to one object, got %s want %s", loserName, winnerName)
	}
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status.Review.PR != prLoser {
		t.Fatalf("claim must carry the loser pointer, got %q", a.Status.Review.PR)
	}
	if got := a.Status.Review.DispatchLostReleases; got != 0 {
		t.Fatalf("pointer change must reset the release counter, got %d", got)
	}
}

// ── #391: an UNSTAMPED budget must age out on the claim's own clock.
// Pre-142 workers released dispatch-lost without stamping
// LastDispatchLostAt; with nil the refusal stood forever and the message's
// "wait out the horizon" remedy was a lie (verified live on rhesadox#2028).
func TestArmClaim_UnstampedBudgetAgesOutWithClaim(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2028"
	const sha = "371be9d900002028"

	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Forge the pre-142 shape: strikes present, stamp absent, claim YOUNG
	// (CreationTimestamp is fresh from the arm — recency without a stamp is
	// presumed from the claim's age).
	a, err := resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	a.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases
	a.Status.Review.LastDispatchLostAt = nil
	if err := c.Status().Update(ctx, a); err != nil { // status subresource: plain Update drops .Status
		t.Fatalf("forge pre-142 strikes: %v", err)
	}
	a, err = resolveForTest(t, ctx, c, wf, sha) // fresh RV for the metadata write
	if err != nil {
		t.Fatalf("resolve after strike forge: %v", err)
	}
	a.CreationTimestamp = metav1.NewTime(time.Now()) // fake client leaves it zero — the fallback clock needs a real age
	if err := c.Update(ctx, a); err != nil {
		t.Fatalf("forge claim age: %v", err)
	}
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); !errors.Is(err, ErrChurnBudgetExhausted) {
		t.Fatalf("young unstamped budget must still refuse, got %v", err)
	}

	// Age the CLAIM (the fallback clock): the horizon passes → the budget
	// self-clears and the automatic arm proceeds.
	a, err = resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	a.CreationTimestamp = metav1.NewTime(time.Now().Add(-8 * time.Hour))
	if err := c.Update(ctx, a); err != nil {
		t.Fatalf("backdate claim: %v", err)
	}
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("unstamped budget must age out with the claim, got %v", err)
	}
	a, err = resolveForTest(t, ctx, c, wf, sha)
	if err != nil {
		t.Fatalf("resolve after arm: %v", err)
	}
	if a.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("the arm must reset the stale budget, got %d", a.Status.Review.DispatchLostReleases)
	}
}
