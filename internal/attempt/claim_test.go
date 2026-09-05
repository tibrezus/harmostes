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
	if !strings.Contains(err.Error(), "re-apply the label") {
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
	const pr = "git.rezus.cloud/tibrez/rhesadox#1864"
	const sha = "5472b055cafe0002"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("first arm: %v", err)
	}
	if err := ReleaseClaim(ctx, c, "harmostes", name, "horizon"); err != nil {
		t.Fatalf("horizon release: %v", err)
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
// ledger), ArmedSince fresh (new era).
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
	if age := time.Since(r.ArmedSince.Time); age > time.Minute {
		t.Fatalf("revived era must start a fresh clock, got %v", age)
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
	if _, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false); !errors.Is(err, ErrRecentlyDismissed) {
		t.Fatalf("want ErrRecentlyDismissed (converged after %d releases), got %v", v1alpha1.MaxDispatchLostReleases, err)
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

// ── r4 P2: the era tie-break is TOTAL — a live claim beats a released one
// at the same clock, so a tie never resurrects a just-released claim. ──
func TestArmClaim_TieBreakPrefersLiveEra(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	const pr = "git.rezus.cloud/tibrez/rhesadox#2002"
	const sha = "5472b055cafe2002"

	name, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Release it, then re-arm: the revival is the SAME (now live) object.
	if err := ReleaseClaim(ctx, c, "harmostes", name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
		t.Fatalf("release: %v", err)
	}
	revived, err := armFor(t, ctx, c, wf, pr, sha, "needs-review", false)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived != name {
		t.Fatalf("tie must resolve to the one same-head era, got %s", revived)
	}
	a, _ := resolveForTest(t, ctx, c, wf, sha)
	if a.Status.Review.Released {
		t.Fatal("the surviving era must be LIVE — a tie must not resurrect the released object")
	}
}
