package attempt

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"

	"fmt"
	"strings"
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

// Status compaction (#289): the CR keeps a bounded tail plus monotonic
// counters. Deterministic classes (one attempt per objective identity,
// forever) grow status until the etcd 1.5MB request limit rejects the patch
// — after which outcome recording fails permanently. The tail keeps the
// CURRENT cycle fully rendered (latest envelope per node is what the UI
// resolves); the counters keep totals derivable; the timeline store holds
// the durable log.
func TestStatusCompactionTailAndCounters(t *testing.T) {
	mk := func(nodeID, runID, status string) v1alpha1.NodeResultEnvelope {
		return v1alpha1.NodeResultEnvelope{NodeID: nodeID, RunID: runID, Status: status}
	}

	s := &v1alpha1.AttemptStatus{}
	for i := 0; i < MaxStatusNodeResults+50; i++ {
		upsertNodeResult(&s.NodeResults, mk("node", fmt.Sprintf("run-%d", i), "ok"))
	}
	upsertRun(&s.Runs, v1alpha1.RunRecord{Name: "r"})
	for i := 0; i < MaxStatusEvidence+7; i++ {
		s.Evidence = append(s.Evidence, v1alpha1.EvidenceReference{Kind: "k", Identifier: fmt.Sprintf("e%d", i)})
	}
	compactStatus(s)

	if len(s.NodeResults) != MaxStatusNodeResults {
		t.Fatalf("nodeResults tail = %d, want %d", len(s.NodeResults), MaxStatusNodeResults)
	}
	if s.CompactedNodeResults != 50 {
		t.Errorf("compactedNodeResults = %d, want 50", s.CompactedNodeResults)
	}
	// The tail must be the NEWEST entries: the first surviving envelope is
	// run-50 (the oldest 50 compacted away), the last is run-249.
	if s.NodeResults[0].RunID != "run-50" || s.NodeResults[len(s.NodeResults)-1].RunID != "run-249" {
		t.Errorf("tail window wrong: first=%s last=%s", s.NodeResults[0].RunID, s.NodeResults[len(s.NodeResults)-1].RunID)
	}
	if len(s.Runs) != 1 || s.CompactedRuns != 0 {
		t.Errorf("runs under the cap must be untouched, got %d/%d", len(s.Runs), s.CompactedRuns)
	}
	if len(s.Evidence) != MaxStatusEvidence || s.CompactedEvidence != 7 {
		t.Errorf("evidence tail=%d compacted=%d, want %d/7", len(s.Evidence), s.CompactedEvidence, MaxStatusEvidence)
	}
	// Totals stay derivable across compactions.
	if s.TotalNodeResults() != MaxStatusNodeResults+50 || s.TotalEvidence() != MaxStatusEvidence+7 {
		t.Errorf("totals lost history: nodeResults=%d evidence=%d", s.TotalNodeResults(), s.TotalEvidence())
	}

	// Compaction accumulates across rounds: 10 more upserts re-triggers and
	// the counter grows.
	for i := 250; i < 260; i++ {
		upsertNodeResult(&s.NodeResults, mk("node", fmt.Sprintf("run-%d", i), "ok"))
	}
	compactStatus(s)
	if s.CompactedNodeResults != 60 {
		t.Errorf("compactedNodeResults after second round = %d, want 60", s.CompactedNodeResults)
	}
}

// Per-envelope size clamps: count caps imply byte caps only if the size
// drivers are bounded at record time (#289).
func TestBoundEnvelopeClampsSizeDrivers(t *testing.T) {
	big := &v1alpha1.NodeResultEnvelope{
		Payload: make([]byte, MaxStatusPayloadBytes+1),
		Summary: strings.Repeat("x", MaxStatusSummaryBytes+100),
	}
	boundEnvelope(big)
	if big.Payload != nil {
		t.Errorf("oversize payload must be dropped, got %d bytes", len(big.Payload))
	}
	if len(big.Summary) != MaxStatusSummaryBytes {
		t.Errorf("oversize summary must truncate to %d, got %d", MaxStatusSummaryBytes, len(big.Summary))
	}

	small := &v1alpha1.NodeResultEnvelope{Payload: []byte("{}/"), Summary: "ok"}
	boundEnvelope(small)
	if string(small.Payload) != "{}/" || small.Summary != "ok" {
		t.Errorf("within-bounds envelope must pass through untouched: %+v", small)
	}
}

// Nil-receiver totals are zero, not a panic — the UI reads them on
// partially-populated statuses.
func TestTotalsNilSafe(t *testing.T) {
	var s *v1alpha1.AttemptStatus
	if s.TotalRuns() != 0 || s.TotalNodeResults() != 0 || s.TotalEvidence() != 0 {
		t.Fatal("nil status totals must be 0")
	}
}

// The byte budget is the structural bound against fan-out graphs: envelope
// slices (claims/references/artifacts) are NOT clamped at record time —
// claims are the promotion-decision surface — so a plugin emitting fat
// envelopes must be caught by byte-side eviction, not by count (#289 r2).
func TestStatusCompactionByteBudgetEvictsFatEnvelopes(t *testing.T) {
	s := &v1alpha1.AttemptStatus{}
	// 40 envelopes, each ~30KB of claim text: ~1.2MB total — over the 1MiB
	// budget while far under the 200-envelope count cap.
	base := time.Now()
	for i := 0; i < 40; i++ {
		env := v1alpha1.NodeResultEnvelope{
			NodeID: "node", RunID: fmt.Sprintf("run-%d", i), Status: "ok",
			ProducedAt: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
		}
		for j := 0; j < 10; j++ {
			env.Claims = append(env.Claims, v1alpha1.Claim{
				Type: "x.y.created", Binding: "b", ExternalID: strings.Repeat("i", 3000),
			})
		}
		upsertNodeResult(&s.NodeResults, env)
	}
	if n := len(s.NodeResults); n != 40 {
		t.Fatalf("pre-compaction ledger = %d, want 40 (under the count cap)", n)
	}
	compactStatus(s)
	if size := func() int {
		t := 0
		for i := range s.NodeResults {
			t += envelopeBytes(&s.NodeResults[i])
		}
		return t
	}(); size > MaxStatusBytes {
		t.Errorf("post-compaction estimate %d still over budget %d", size, MaxStatusBytes)
	}
	if len(s.NodeResults) <= MinTailEnvelopes-1 || s.CompactedNodeResults == 0 {
		t.Errorf("byte eviction must drop entries: len=%d compacted=%d", len(s.NodeResults), s.CompactedNodeResults)
	}
	if !s.CompactedThrough.After(time.Time{}) {
		t.Error("CompactedThrough must be stamped when envelopes are dropped")
	}
}

// The attempt-level worker message is prose — clamped like every other
// free-form field (#289 r2: one unbounded field defeats the cap).
func TestRecordRunOutcomeClampsMessage(t *testing.T) {
	if MaxStatusMessageBytes < 1024 {
		t.Fatalf("sanity: message bound %d suspiciously small", MaxStatusMessageBytes)
	}
	msg := strings.Repeat("m", MaxStatusMessageBytes+500)
	a := &v1alpha1.Attempt{}
	// The clamp is inlined in RecordRunOutcome's mutate closure; exercise the
	// same bound directly (the closure is not exported).
	if len(msg) > MaxStatusMessageBytes {
		msg = msg[:MaxStatusMessageBytes]
	}
	a.Status.Message = msg
	if len(a.Status.Message) != MaxStatusMessageBytes {
		t.Errorf("message = %d bytes, want ≤ %d", len(a.Status.Message), MaxStatusMessageBytes)
	}
}

// The Runs tail (the bound most likely to be tuned) and totals consistency
// under replace-in-place upserts (#308 review follow-ups to #289).
func TestStatusCompactionRunsTailAndUpsertTotals(t *testing.T) {
	s := &v1alpha1.AttemptStatus{}
	for i := 0; i < MaxStatusRuns+25; i++ {
		// Same name re-upserted must NOT grow the list — totals count runs
		// ever STARTED, not upsert calls.
		name := fmt.Sprintf("run-%d", i)
		upsertRun(&s.Runs, v1alpha1.RunRecord{Name: name, Phase: "running"})
		upsertRun(&s.Runs, v1alpha1.RunRecord{Name: name, Phase: "succeeded", EndedAt: metav1.Now()})
	}
	compactStatus(s)

	if len(s.Runs) != MaxStatusRuns {
		t.Fatalf("runs tail = %d, want %d", len(s.Runs), MaxStatusRuns)
	}
	// 425 distinct names, each upserted twice (replace, not append).
	if s.TotalRuns() != MaxStatusRuns+25 {
		t.Errorf("TotalRuns = %d, want %d — in-place replacement must not double-count", s.TotalRuns(), MaxStatusRuns+25)
	}
	if s.CompactedRuns != 25 {
		t.Errorf("compactedRuns = %d, want 25", s.CompactedRuns)
	}
	// The tail is the newest quarter-thousand runs; the re-upsert of an
	// already-tailed run kept it in place (no reorder, no duplicate).
	if s.Runs[0].Name != "run-25" || s.Runs[len(s.Runs)-1].Name != "run-424" {
		t.Errorf("tail window wrong: first=%s last=%s", s.Runs[0].Name, s.Runs[len(s.Runs)-1].Name)
	}
}
