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
// reason="dismissed"|"breaker"} (r6 P7): the churn guard and the dead-dispatch
// breaker were the only new stops with no metric — a loop converged into
// refusals looks healthier on trigger_slot_total than it is, and the
// post-deploy question for #343 is exactly "which heads did we refuse, and
// how often?". A dismissal on a labeled PR is asked-for work being dropped;
// it must be graphable, not buried in pod logs.
func recordReviewGate(ctx context.Context, repo string, err error) {
	reason := "other"
	switch {
	case errors.Is(err, attempt.ErrRecentlyDismissed):
		reason = "dismissed"
	case errors.Is(err, attempt.ErrDeadDispatchBreaker):
		reason = "breaker"
	}
	c, _ := observability.Meter().Int64Counter("harmostes_review_gate_total",
		metric.WithDescription("Review-gate arm refusals per workflow (dismissed = churn guard, breaker = dead-dispatch guard)."))
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow", repo),
		attribute.String("reason", reason)))
}
