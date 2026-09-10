package worker

import (
	"context"
	"errors"

	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// recordReviewGate increments harmostes_review_gate_total{workflow,
// repo, reason="dismissed"|"breaker"} (r6 P7): the churn guard and the
// dead-dispatch breaker were the only new stops with no metric — a loop
// converged into refusals looks healthier on trigger_slot_total than it
// is, and the post-deploy question for #343 is exactly "which heads did we
// refuse, and how often?". A dismissal on a labeled PR is asked-for work
// being dropped; it must be graphable, not buried in pod logs. workflow
// is the Workflow NAME (r7 P7) — every other harmostes series keys
// workflow on the workflow name; a repo pointer under that key silently
// splits dashboards. The pointer travels as its own `repo` attribute.
func recordReviewGate(ctx context.Context, workflow, repo string, err error) {
	reason := "other"
	switch {
	case errors.Is(err, attempt.ErrRecentlyDismissed):
		reason = "dismissed"
	case errors.Is(err, attempt.ErrChurnBudgetExhausted):
		reason = "budget"
	case errors.Is(err, attempt.ErrDeadDispatchBreaker):
		reason = "breaker"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		reason = "arm-error"
	}
	recordReviewGateReason(ctx, workflow, repo, reason)
}

// recordReviewGateReason increments the review-gate counter under an exact
// reason — the non-sentinel signals (sweep-abort, scan-error) that are not
// errors surfaced through ArmClaim but states of the sweep itself.
func recordReviewGateReason(ctx context.Context, workflow, repo, reason string) {
	c, _ := observability.Meter().Int64Counter("harmostes_review_gate_total",
		metric.WithDescription("Review-gate arm refusals and sweep degradations per workflow "+
			"(dismissed = horizon guard, budget = dispatch-lost churn budget, breaker = dead-dispatch guard, "+
			"arm-error = ctx-bound arm, scan-error = labeled scan failed, sweep-abort = sweep deadline cancelled it, "+
			"cancel = superseded/closed review Job deleted, #402)."))
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow", workflow),
		attribute.String("repo", repo),
		attribute.String("reason", reason)))
}
