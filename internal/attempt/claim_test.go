package attempt

import (
	"context"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

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
