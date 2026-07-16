package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestBuildResource(t *testing.T) {
	res, err := buildResource(Config{
		Component: "harmostes-worker", Version: "0.7.1",
		PodName: "harmostes-x-abc", PodNamespace: "harmostes",
	})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	got := map[string]string{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	for k, want := range map[string]string{
		"service.name":        "harmostes-worker",
		"service.namespace":   "harmostes",
		"service.version":     "0.7.1",
		"k8s.pod.name":        "harmostes-x-abc",
		"k8s.namespace.name":  "harmostes",
	} {
		if got[k] != want {
			t.Errorf("resource %s = %q, want %q", k, got[k], want)
		}
	}
}

func TestBuildResourceOmitsEmpty(t *testing.T) {
	res, err := buildResource(Config{Component: "harmostes-controller"})
	if err != nil {
		t.Fatalf("buildResource: %v", err)
	}
	got := map[string]bool{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = true
	}
	// version/k8s.* are optional — absent when not provided.
	for _, k := range []string{"service.version", "k8s.pod.name", "k8s.namespace.name"} {
		if got[k] {
			t.Errorf("resource should omit %s when unset", k)
		}
	}
}

// TestInitDisabled: with no OTLPEndpoint, telemetry is off — Init succeeds and
// Shutdown is a safe no-op. (Local dev + unit tests never need a collector.)
func TestInitDisabled(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{Component: "harmostes-worker"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("disabled shutdown should be a no-op, got %v", err)
	}
	if Tracer() == nil || Meter() == nil {
		t.Error("Tracer()/Meter() must never be nil")
	}
}

// TestFlushEmitsSpansBeforeShutdown is the regression guard for the dropped-
// telemetry bug: a span emitted into a batch processor (which only exports on a
// timer or ForceFlush) is NOT lost when the process shuts down — Init's shutdown
// calls ForceFlush first. The worker's finish() relies on this guarantee.
func TestFlushEmitsSpansBeforeShutdown(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	// A long batch timeout means the span sits buffered until ForceFlush —
	// mirroring the production batcher's behaviour mid-run.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(time.Hour)),
	)
	defer tp.Shutdown(context.Background())

	_, span := tp.Tracer("test").Start(context.Background(), "harmostes.worker.run")
	span.End()

	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("expected 0 spans exported before flush, got %d (buffer should hold them)", n)
	}
	// ForceFlush is exactly what Init's shutdown invokes before Shutdown().
	if err := tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("expected 1 span flushed on shutdown, got %d — telemetry would be DROPPED", n)
	}
}

// TestLoggerInjectsTraceContext: JSON logs carry trace_id/span_id when a span is
// active in the record's context (so logs join the trace in the backend), and
// omit them when no span is active.
func TestLoggerInjectsTraceContext(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger("harmostes-worker", &buf)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer tp.Shutdown(context.Background())
	ctx, span := tp.Tracer("t").Start(context.Background(), "x")
	defer span.End()

	lg.InfoContext(ctx, "running", "workflow", "w", "phase", "agent")

	rec := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log is not valid JSON: %v\n%s", err, buf.String())
	}
	if rec["component"] != "harmostes-worker" {
		t.Errorf("component = %v, want harmostes-worker", rec["component"])
	}
	tid, ok := rec["trace_id"].(string)
	if !ok || tid == "" {
		t.Errorf("expected non-empty trace_id, got %v", rec["trace_id"])
	}
	sid, ok := rec["span_id"].(string)
	if !ok || sid == "" {
		t.Errorf("expected non-empty span_id, got %v", rec["span_id"])
	}

	// No active span → no trace fields (plain Info carries no context).
	buf.Reset()
	lg.Info("no span here")
	rec = map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log is not valid JSON: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("did not expect trace_id without an active span, got %v", rec["trace_id"])
	}
}
