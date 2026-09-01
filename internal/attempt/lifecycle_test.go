package attempt

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The ledger is keyed by (NodeID, RunID): new keys append (history preserved
// across retries — one pod = one run = one envelope, ADR-0007), existing keys
// replace in place (idempotent arrival + idempotent outcome upsert).
func TestUpsertNodeResult_LedgerSemantics(t *testing.T) {
	var ledger []v1alpha1.NodeResultEnvelope
	mk := func(nodeID, runID, status string) v1alpha1.NodeResultEnvelope {
		return v1alpha1.NodeResultEnvelope{NodeID: nodeID, RunID: runID, Status: status}
	}

	upsertNodeResult(&ledger, mk("prepare", "run-1", "ok"))
	upsertNodeResult(&ledger, mk("agent", "run-2", "ok"))
	if len(ledger) != 2 {
		t.Fatalf("ledger = %d entries, want 2 after two new keys", len(ledger))
	}

	// Same (NodeID, RunID) replaces in place — no duplicate, no reorder.
	replacement := mk("prepare", "run-1", "ok")
	replacement.Summary = "re-recorded"
	upsertNodeResult(&ledger, replacement)
	if len(ledger) != 2 || ledger[0].Summary != "re-recorded" {
		t.Errorf("same-key upsert: ledger = %+v, want in-place replacement", ledger)
	}

	// Retry: same node, different run (pod) — history is preserved.
	upsertNodeResult(&ledger, mk("agent", "run-3", "failed"))
	if len(ledger) != 3 {
		t.Fatalf("retry appended: ledger = %d entries, want 3", len(ledger))
	}
	if ledger[2].NodeID != "agent" || ledger[2].RunID != "run-3" {
		t.Errorf("retry envelope position/content wrong: %+v", ledger[2])
	}
}

// Incremental arrival end to end: envelopes land as nodes complete; the
// outcome upsert is then a no-op for already-recorded envelopes (no
// duplicates), while genuinely new envelopes still append.
func TestUpsertNodeResult_IncrementalArrivalThenOutcome(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	wf := wikiWorkflow()
	obj := DeriveObjective(wf, TriggerContext{Revision: "abc"})
	opts := ResolveOptions{Namespace: "harmostes", Owner: wf, Scheme: wfScheme(t)}
	a, _, _ := ResolveOrCreate(ctx, c, obj, opts)
	_ = RecordRunStarted(ctx, c, "harmostes", a.Name, "run-1")

	prepareEnv := v1alpha1.NodeResultEnvelope{
		NodeID: "prepare", RunID: "run-1", Status: v1alpha1.NodeResultStatusOK,
		ProducedAt: metav1.NewTime(time.Now()),
	}
	agentEnv := v1alpha1.NodeResultEnvelope{
		NodeID: "agent", RunID: "run-2", Status: v1alpha1.NodeResultStatusOK,
		ProducedAt: metav1.NewTime(time.Now().Add(time.Second)),
	}
	if err := UpsertNodeResult(ctx, c, "harmostes", a.Name, prepareEnv); err != nil {
		t.Fatalf("upsert prepare: %v", err)
	}
	if err := UpsertNodeResult(ctx, c, "harmostes", a.Name, agentEnv); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}

	// Outcome re-records the same envelopes (plus none new): the ledger must
	// not duplicate.
	if err := RecordRunOutcome(ctx, c, "harmostes", a.Name, RunOutcome{
		RunName: "run-2", Phase: "succeeded", Envelopes: []v1alpha1.NodeResultEnvelope{prepareEnv, agentEnv},
	}); err != nil {
		t.Fatalf("outcome: %v", err)
	}

	got := getAttempt(t, ctx, c, a)
	if len(got.Status.NodeResults) != 2 {
		t.Errorf("ledger = %d entries after incremental + outcome, want 2 (idempotent): %+v",
			len(got.Status.NodeResults), got.Status.NodeResults)
	}
}
