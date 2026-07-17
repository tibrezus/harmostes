package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tibrezus/harmostes/internal/k8s"
)

// withManualMeter installs a manual-reader meter provider as the global meter,
// returning the reader + a collect helper. Restored on cleanup.
func withManualMeter(t *testing.T) (*sdkmetric.ManualReader, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background())
	})
	return reader, func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not recorded (have: %v)", name, metricNames(rm))
	return metricdata.Metrics{}
}

func metricNames(rm metricdata.ResourceMetrics) []string {
	var out []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out = append(out, m.Name)
		}
	}
	return out
}

// TestControllerMetricsRecorded: the reconcile + run-scheduled recorders emit
// harmostes_workflow_runs_total{outcome=scheduled} + harmostes_reconcile_seconds.
func TestControllerMetricsRecorded(t *testing.T) {
	_, collect := withManualMeter(t)
	ctx := context.Background()

	recordWorkflowRunScheduled(ctx, "llm-wiki")
	recordReconcileSeconds(ctx, "llm-wiki", 100*time.Millisecond)

	rm := collect()

	// workflow_runs_total{outcome=scheduled} == 1
	runs := findMetric(t, rm, "harmostes_workflow_runs_total")
	sum, ok := runs.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("workflow_runs_total data = %T, want Sum[int64]", runs.Data)
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("workflow_runs_total data points = %d, want 1", len(sum.DataPoints))
	}
	if sum.DataPoints[0].Value != 1 {
		t.Errorf("workflow_runs_total value = %d, want 1", sum.DataPoints[0].Value)
	}

	// reconcile_seconds has one recorded point.
	rec := findMetric(t, rm, "harmostes_reconcile_seconds")
	hist, ok := rec.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("reconcile_seconds data = %T, want Histogram[float64]", rec.Data)
	}
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 {
		t.Errorf("reconcile_seconds points = %+v, want 1 observation", hist.DataPoints)
	}
}

// TestActiveJobsGauge: the observable gauge counts only non-terminal harmostes
// worker Jobs (Succeeded==0 && Failed==0). Exercises the RegisterCallback path
// against a fake client, then collects via the manual reader.
func TestActiveJobsGauge(t *testing.T) {
	_, collect := withManualMeter(t)

	managedBy := map[string]string{"app.kubernetes.io/managed-by": "harmostes"}
	active := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "harmostes-wf-active", Namespace: "harmostes", Labels: managedBy}}
	terminal := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "harmostes-wf-done", Namespace: "harmostes", Labels: managedBy}}
	terminal.Status.Succeeded = 1

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithObjects(active, terminal).
		Build()

	r := &WorkflowReconciler{Client: cl, Scheme: k8s.Scheme()}
	r.registerActiveJobsGauge()

	rm := collect()
	g := findMetric(t, rm, "harmostes_active_jobs")
	gauge, ok := g.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("active_jobs data = %T, want Gauge[int64]", g.Data)
	}
	if len(gauge.DataPoints) != 1 {
		t.Fatalf("active_jobs data points = %d, want 1", len(gauge.DataPoints))
	}
	// one active (terminal excluded)
	if got, want := gauge.DataPoints[0].Value, int64(1); got != want {
		t.Errorf("active_jobs = %d, want %d", got, want)
	}
}
