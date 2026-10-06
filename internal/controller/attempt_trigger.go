package controller

// The consume-on-completion trigger (#647): an attempt's run reached
// terminal → its workflow is due now. This is the kernel's own vocabulary —
// Attempts, RunRecords, Jobs — with no gate, workflow-class, or verdict
// knowledge: every claim class consumes on the next sweep, and this only
// collapses the sweep's trigger latency from "next poll" (the armed
// carve-out, ≤ PollInterval) to event time. The observation itself stays
// where it belongs — the sweep consumes verdicts and releases dead runs —
// this file only decides WHEN the sweep is asked to look.

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// runTerminalPredicate lets through only Attempt updates in which a
// RunRecord TRANSITIONED to a terminal phase — the worker's
// RecordRunOutcome upsert (and the ledger's finalize paths). Every other
// Attempt status patch (hold notes, live-claim bookkeeping, envelope
// appends) must not wake the workflow's reconcile: the wake is the
// completion event, nothing else. Create/Delete/Generic events are
// dropped: runs start running, so a transition can only be observed as an
// update — a controller restart that misses one is re-covered by the armed
// poll carve-out (at-least-once with a poll backstop, by design).
type runTerminalPredicate struct{}

func (runTerminalPredicate) Create(event.CreateEvent) bool   { return false }
func (runTerminalPredicate) Delete(event.DeleteEvent) bool   { return false }
func (runTerminalPredicate) Generic(event.GenericEvent) bool { return false }

func (runTerminalPredicate) Update(e event.UpdateEvent) bool {
	oldA, okOld := e.ObjectOld.(*v1alpha1.Attempt)
	newA, okNew := e.ObjectNew.(*v1alpha1.Attempt)
	if !okOld || !okNew {
		return false
	}
	terminalBefore := make(map[string]bool, len(oldA.Status.Runs))
	for _, run := range oldA.Status.Runs {
		if v1alpha1.RunTerminalPhase(run.Phase) && !run.EndedAt.IsZero() {
			terminalBefore[run.Name] = true
		}
	}
	for _, run := range newA.Status.Runs {
		if v1alpha1.RunTerminalPhase(run.Phase) && !run.EndedAt.IsZero() && !terminalBefore[run.Name] {
			return true
		}
	}
	return false
}

// attemptTerminalToWorkflow maps a terminal-run Attempt event to its driving
// Workflow's reconcile request (spec.workflowRef, "namespace/name" — copied
// from the Workflow at attempt creation). Malformed refs yield no requests:
// the Attempt ledger is best-effort history (ADR-0005), never a trigger
// authority — an unparseable ref degrades to the poll backstop.
func (r *WorkflowReconciler) attemptTerminalToWorkflow(_ context.Context, obj client.Object) []reconcile.Request {
	at, ok := obj.(*v1alpha1.Attempt)
	if !ok || at.Spec.WorkflowRef == "" {
		return nil
	}
	ns, name, found := strings.Cut(at.Spec.WorkflowRef, "/")
	if !found || ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// terminalRunPending reports whether any live claim's attempt recorded a
// terminal run SINCE the workflow last dispatched — the watermark is
// LastRunAt, stamped by claimTriggerSlot on every won slot, so the same
// completion cannot win twice: after the trigger fires, EndedAt < LastRunAt
// and the condition de-arms itself whatever the sweep then finds (verdict
// consumed, dead run released, or an inconclusive strand). That strand case
// is why the watermark — not claim liveness — must carry the de-arming: a
// claim can stay live for the whole horizon, and keying on liveness alone
// would sweep every webhookMinTriggerInterval until it cleared.
//
// Read failure degrades to false — the armed poll carve-out remains the
// backstop, and a trigger must never be blocked by a ledger read. Gated on
// Status.ReviewReady so workflows the gate never armed pay no List at all.
//
// r11 companion: claimTriggerSlot evaluates this on the FRESH object inside
// the CAS (LastRunAt may have moved since the reconcile's copy); because
// both the due leg and the slot keying call THIS function, the two
// predicates cannot drift — the webhook annotation check's r11 rule,
// preserved by construction.
func (r *WorkflowReconciler) terminalRunPending(ctx context.Context, wf *v1alpha1.Workflow) bool {
	if wf.Status.ReviewReady == nil {
		return false
	}
	last := wf.Status.LastRunAt.Time
	if last.IsZero() {
		return false // never dispatched — the schedule/webhook paths own first dispatch
	}
	attempts, err := attempt.LiveReviewClaims(ctx, r.Client, wf)
	if err != nil {
		log.FromContext(ctx).Error(err, "terminal-run trigger: live-claim list failed — degrading to the poll backstop (#647)")
		return false
	}
	for i := range attempts {
		for _, run := range attempts[i].Status.Runs {
			if v1alpha1.RunTerminalPhase(run.Phase) && !run.EndedAt.IsZero() && run.EndedAt.After(last) {
				return true
			}
		}
	}
	return false
}
