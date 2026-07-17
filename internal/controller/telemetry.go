// telemetry.go holds the controller's OTel metrics (Phase 4): reconcile
// duration, runs scheduled, and an active-jobs gauge. Instruments are fetched
// from the global meter on each record (agent pattern) so a test that sets a
// manual reader sees them; reconciles are cheap. With telemetry disabled (no
// Init) the meter is no-op and these are free.
package controller

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/tibrezus/harmostes/internal/observability"
)

// recordWorkflowRunScheduled increments harmostes_workflow_runs_total{workflow,
// outcome="scheduled"}: the controller's scheduling signal. The run's actual
// outcome (green/skipped/failed) is determined later by the worker and lives on
// the harmostes.worker.run span's harmostes.outcome attribute — the controller
// only observes that it scheduled a run.
func recordWorkflowRunScheduled(ctx context.Context, workflow string) {
	c, _ := observability.Meter().Int64Counter("harmostes_workflow_runs_total",
		metric.WithDescription("Worker runs scheduled by the monitor controller (outcome=scheduled)."))
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("workflow", workflow),
		attribute.String("outcome", "scheduled")))
}

// recordReconcileSeconds records the reconcile wall-clock duration per workflow
// on harmostes_reconcile_seconds{workflow}.
func recordReconcileSeconds(ctx context.Context, workflow string, d time.Duration) {
	h, _ := observability.Meter().Float64Histogram("harmostes_reconcile_seconds",
		metric.WithDescription("Reconcile wall-clock duration."))
	h.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("workflow", workflow)))
}

// registerActiveJobsGauge registers harmostes_active_jobs: an observable gauge
// whose callback counts non-terminal (Pending/Running) harmostes worker Jobs
// across all Workflows in the cluster. The periodic reader polls it. Called once
// at setup; a no-op meter (telemetry disabled) returns an error on Register that
// we ignore — the gauge simply won't be observed.
func (r *WorkflowReconciler) registerActiveJobsGauge() {
	gauge, _ := observability.Meter().Int64ObservableGauge("harmostes_active_jobs",
		metric.WithDescription("Active (non-terminal) harmostes worker Jobs."),
		metric.WithUnit("{job}"))
	if _, err := observability.Meter().RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		var jobs batchv1.JobList
		if err := r.List(ctx, &jobs, client.MatchingLabels{"app.kubernetes.io/managed-by": "harmostes"}); err != nil {
			return err
		}
		active := 0
		for i := range jobs.Items {
			if jobs.Items[i].Status.Succeeded == 0 && jobs.Items[i].Status.Failed == 0 {
				active++
			}
		}
		o.ObserveInt64(gauge, int64(active))
		return nil
	}, gauge); err != nil {
		// No-op meter (telemetry disabled): the gauge is never observed. Not fatal.
		return
	}
}
