//go:build integration

package integration

// The lost-update race (#318): two overlapping writers on the SAME
// Implementation Attempt. JSON merge-patch replaces arrays wholesale, so a
// writer that read the attempt before a competitor wrote sends its own full
// Runs/NodeResults array — silently reverting the competitor's record —
// unless its patch carries a resourceVersion precondition and it re-reads
// on conflict. The ledger's single write primitive (patchAttemptStatus)
// carries that discipline; this test PROVES it deterministically: a hook
// client runs a complete competing write inside the original writer's
// get→patch window (not a timing lottery), then lets the original patch
// proceed. Without the discipline the competitor's run vanishes; with it
// both survive because the conflict forces a fresh read.

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// hookClient fires interfere exactly once, just before the first Status
// patch passes through — i.e. inside the writer's read-modify-write window.
type hookClient struct {
	client.Client
	once      sync.Once
	interfere func()
}

func (h *hookClient) Status() client.StatusWriter {
	return &hookStatusWriter{delegate: h.Client.Status(), h: h}
}

type hookStatusWriter struct {
	delegate client.StatusWriter
	h        *hookClient
}

func (w *hookStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.h.once.Do(w.h.interfere)
	return w.delegate.Patch(ctx, obj, patch, opts...)
}

// Create/Update complete SubResourceWriter; the ledger only patches status.
func (w *hookStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.delegate.Create(ctx, obj, subResource, opts...)
}

func (w *hookStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.delegate.Update(ctx, obj, opts...)
}

// TestConcurrentLedgerWritersBothSurvive: run-a's outcome write is
// interleaved with a complete run-b outcome write. Both runs — and both
// envelopes — must be present afterwards. Before the single write
// primitive gained the optimistic lock + retry, run-b was deterministically
// reverted by run-a's stale-array merge patch.
func TestConcurrentLedgerWritersBothSurvive(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	wf := fixtureWorkflow(t, c, "ledger-race")
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "1aceb000", Source: "webhook"})
	a, _, err := attempt.ResolveOrCreate(ctx, c, obj, attempt.ResolveOptions{
		Namespace: "default", WorkflowRef: "default/ledger-race", Owner: wf, Scheme: k8s.Scheme(),
	})
	if err != nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}

	now := metav1.NewTime(time.Now())
	interfere := func() {
		// The competitor: a FULL outcome write for run-b through the raw
		// client, landing entirely inside run-a's get→patch window.
		err := attempt.RecordRunOutcome(ctx, c, "default", a.Name, attempt.RunOutcome{
			RunName: "run-b",
			Phase:   "succeeded",
			Envelopes: []v1alpha1.NodeResultEnvelope{{
				NodeID: "gate", RunID: "run-b", Status: "ok",
				Summary: "competing writer", ProducedAt: now,
			}},
		})
		if err != nil {
			t.Errorf("competing write (run-b): %v", err)
		}
	}
	hooked := &hookClient{Client: c, interfere: interfere}

	if err := attempt.RecordRunOutcome(ctx, hooked, "default", a.Name, attempt.RunOutcome{
		RunName: "run-a",
		Phase:   "failed",
		Envelopes: []v1alpha1.NodeResultEnvelope{{
			NodeID: "prepare", RunID: "run-a", Status: "ok",
			Summary: "hooked writer", ProducedAt: now,
		}},
	}); err != nil {
		t.Fatalf("hooked write (run-a): %v", err)
	}

	var got v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: a.Name}, &got); err != nil {
		t.Fatalf("get attempt: %v", err)
	}

	phases := map[string]string{}
	for _, r := range got.Status.Runs {
		phases[r.Name] = r.Phase
	}
	if len(phases) != 2 || phases["run-a"] != "failed" || phases["run-b"] != "succeeded" {
		t.Fatalf("lost-update: runs = %v (want run-a=failed AND run-b=succeeded) — a concurrent writer's terminal record was reverted", phases)
	}
	if got.Status.TotalRuns() != 2 {
		t.Errorf("TotalRuns = %d, want 2", got.Status.TotalRuns())
	}
	if len(got.Status.NodeResults) != 2 {
		t.Errorf("envelopes = %d, want 2 (run-a's stale array must not revert run-b's envelope)", len(got.Status.NodeResults))
	}
}

// TestConcurrentDeadDispatchAccounting (#328): the breaker counter lives on
// the same claim the gate re-arms, so a death recording must survive a
// re-arm racing it — the same lost-update class as #318, but scalar. The
// hook client runs a complete dispatched-death (dispatch → dead release)
// inside an ArmClaim's get→patch window. The optimistic lock forces the arm
// to re-read; the death's count must survive both the conflict and the
// re-arm's Released=false reset.
func TestConcurrentDeadDispatchAccounting(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	wf := fixtureWorkflow(t, c, "breaker-race")
	const pr = "git.rezus.cloud/tibrez/rhesadox#328"
	const sha = "3aceb000"

	arm := func(cl client.Client) string {
		t.Helper()
		at, err := attempt.ArmClaim(ctx, cl, k8s.Scheme(), wf, pr, sha, "needs-review", false)
		if err != nil {
			t.Fatalf("ArmClaim: %v", err)
		}
		return at.Name
	}
	name := arm(c)

	// Death #1 recorded inside a racing re-arm's write window.
	disp1 := false
	interfere := func() {
		if disp1 {
			return
		}
		disp1 = true
		if err := attempt.MarkClaimDispatched(ctx, c, "default", name); err != nil {
			t.Errorf("competing dispatch: %v", err)
		}
		if _, _, err := attempt.ReleaseClaimDead(ctx, c, "default", name, "dispatch-lost"); err != nil {
			t.Errorf("competing dead release: %v", err)
		}
	}
	hooked := &hookClient{Client: c, interfere: interfere}
	arm(hooked)

	var got v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &got); err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if got.Status.Review.DeadDispatches != 1 {
		t.Fatalf("dead dispatches after racing re-arm = %d, want 1 — the death was reverted by the arm's stale patch", got.Status.Review.DeadDispatches)
	}

	// Death #2 lands on the re-armed claim: the count accumulates across
	// full arm→dispatch→death cycles.
	if err := attempt.MarkClaimDispatched(ctx, c, "default", name); err != nil {
		t.Fatalf("dispatch 2: %v", err)
	}
	if _, _, err := attempt.ReleaseClaimDead(ctx, c, "default", name, "dispatch-timeout"); err != nil {
		t.Fatalf("dead release 2: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &got); err != nil {
		t.Fatalf("get attempt 2: %v", err)
	}
	if got.Status.Review.DeadDispatches != 2 {
		t.Fatalf("dead dispatches after second death = %d, want 2", got.Status.Review.DeadDispatches)
	}

	// A head change resolves a NEW attempt (objective identity includes the
	// SHA, ADR-0005) — its claim must open with a fresh counter while the
	// old attempt keeps its history.
	fresh, err := attempt.ArmClaim(ctx, c, k8s.Scheme(), wf, pr, "newer-head", "needs-review", false)
	if err != nil {
		t.Fatalf("arm at new head: %v", err)
	}
	if fresh.Name == name {
		t.Fatal("a new head must resolve a new attempt, got the same one")
	}
	// ArmClaim returns the pre-patch snapshot (create-time status is empty
	// on the real API server, #277) — re-read for the persisted claim.
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: fresh.Name}, &got); err != nil {
		t.Fatalf("get fresh attempt: %v", err)
	}
	if got.Status.Review == nil || got.Status.Review.DeadDispatches != 0 {
		t.Fatalf("fresh head's claim must open with a nil/zero counter, got %+v", got.Status.Review)
	}
}
