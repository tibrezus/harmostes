package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceparentRoundTrip locks the Phase 4 cross-pod handoff contract: a
// context carrying a sampled, recording span yields a W3C traceparent
// (TraceparentFromContext — what the controller stamps on the worker Job), and
// extracting it into a fresh context (ContextWithTraceparent — what the worker
// does) restores the parent so a span started there is a child of the original.
func TestTraceparentRoundTrip(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(context.Background())
	})

	ctx, parent := tp.Tracer("harmostes").Start(context.Background(), "controller.reconcile")
	tpStr := TraceparentFromContext(ctx)
	parent.End()

	if !parent.SpanContext().IsSampled() {
		t.Fatal("setup: parent span not sampled (propagator would not emit a traceparent)")
	}
	if tpStr == "" {
		t.Fatal("TraceparentFromContext returned empty for a sampled recording span")
	}

	// Extracting the traceparent into a fresh context restores the parent linkage.
	childCtx := ContextWithTraceparent(context.Background(), tpStr)
	sc := trace.SpanContextFromContext(childCtx)
	if !sc.IsValid() {
		t.Fatal("ContextWithTraceparent did not restore a valid parent SpanContext")
	}
	if sc.TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("restored trace ID = %s, want %s", sc.TraceID(), parent.SpanContext().TraceID())
	}
	if sc.SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("restored parent span ID = %s, want %s (the reconcile span)", sc.SpanID(), parent.SpanContext().SpanID())
	}

	// Empty traceparent is a no-op — no parent injected (the local-dev / no-Init path).
	if sc := trace.SpanContextFromContext(ContextWithTraceparent(context.Background(), "")); sc.IsValid() {
		t.Error("empty traceparent should not inject a parent SpanContext")
	}
}
