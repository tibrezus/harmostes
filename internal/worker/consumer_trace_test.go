package worker

// The trace hop (#928): the consumer pod is the middle of every event-armed
// wake — controller publishes, daprd delivers, the consumer dispatches, the
// Job runs. Before #928 that hop emitted no span: the run's root nested
// directly under the controller's reconcile span and the dispatch decision
// (gate sweep, capacity, Attempt mint, Job create) was invisible. These tests
// pin the nesting contract, including the degenerate cases where the middle
// pod is un-instrumented.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/tibrezus/harmostes/internal/observability"
)

// validTraceparent asserts the string handed to the worker Job is something the
// worker can actually extract — validity is decided by the W3C propagator, the
// same extractor the worker uses, not a hand-rolled parser.
func validTraceparent(s string) bool {
	return trace.SpanContextFromContext(
		otel.GetTextMapPropagator().Extract(context.Background(),
			propagation.MapCarrier{"traceparent": s})).IsValid()
}

// triggerBody builds the CloudEvent daprd hands the consumer, mirroring the
// controller's publishTrigger payload.
func triggerBody(payloadTraceparent string) string {
	data := map[string]any{
		"workflow":    "pr-review-harmostes",
		"namespace":   "harmostes",
		"triggerType": "webhook",
		"revision":    "abc123",
		"attemptName": "attempt-1",
	}
	if payloadTraceparent != "" {
		data["traceparent"] = payloadTraceparent
	}
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return `{"specversion":"1.0","type":"harmostes.trigger","source":"harmostes-controller","id":"t-1","datacontenttype":"application/json","data":` + string(b) + `}`
}

// postTrigger runs one delivery through the handler and returns the recorded
// spans plus the RunRequest the dispatch received.
func postTrigger(t *testing.T, headerTraceparent, payloadTraceparent string) ([]tracetest.SpanStub, RunRequest) {
	t.Helper()
	// The tracer must be installed BEFORE the handler runs: a span started
	// against the default no-op provider records nothing.
	exp := withTestTracer(t)
	var got RunRequest
	c := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, req RunRequest) error {
			got = req
			return nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(triggerBody(payloadTraceparent)))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	if headerTraceparent != "" {
		req.Header.Set("traceparent", headerTraceparent)
	}
	rr := httptest.NewRecorder()
	c.handleTrigger(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	return exp.GetSpans(), got
}

// A delivery carrying daprd's propagated traceparent nests the dispatch under
// it, and the dispatched run nests under the DISPATCH span — not under the
// controller's publisher span. That double-nesting is the whole point: the
// trace reads controller.reconcile → daprd → dispatch → run.
func TestHandleTrigger_NestsDispatchUnderDeliveryTraceparent(t *testing.T) {
	// The parent is sampled and recording: withTestTracer's provider is
	// always-sampling, so a span started from the extracted context shares
	// the trace id and names the extracted span id as its parent.
	const inbound = "00-494842a177fb3b502116542189731478-6f3b1f1c2d3e4f50-01"
	spans, req := postTrigger(t, inbound, "00-11111111111111111111111111111111-2222222222222222-01")

	d := spanByName(spans, "worker.trigger.dispatch")
	if d.Name != "worker.trigger.dispatch" {
		t.Fatal("no worker.trigger.dispatch span: the hop emits nothing, the trace is broken (#928)")
	}
	if d.SpanContext.TraceID().String() != "494842a177fb3b502116542189731478" {
		t.Errorf("dispatch trace id = %s, want the inbound trace id — the hop dropped the trace", d.SpanContext.TraceID().String())
	}
	if d.Parent.SpanID().String() != "6f3b1f1c2d3e4f50" {
		t.Errorf("dispatch parent span id = %s, want the delivery span id (daprd's), not the payload stamp's", d.Parent.SpanID().String())
	}
	if !validTraceparent(req.Traceparent) {
		t.Fatalf("dispatched run got a malformed traceparent %q", req.Traceparent)
	}
	if !strings.HasPrefix(req.Traceparent, "00-494842a177fb3b502116542189731478-") {
		t.Errorf("dispatched run traceparent = %q, want the dispatch span's own traceparent on the inbound trace", req.Traceparent)
	}
	if req.Traceparent == inbound {
		t.Error("the run was handed the PARENT's traceparent: the run would be a sibling of the dispatch, not its child")
	}
	if req.Traceparent == "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Error("the run inherited the stale payload stamp: the dispatch hop is skipped in the trace")
	}
}

// Dapr >=1.19 starts a fresh root span for deliveries that carry no context.
// When the header is absent the payload stamp is the only parent available,
// so the dispatch nests under it rather than orphaning the run.
func TestHandleTrigger_FallsBackToPayloadStamp(t *testing.T) {
	const payload = "00-55555555555555555555555555555555-6666666666666666-01"
	spans, req := postTrigger(t, "", payload)

	d := spanByName(spans, "worker.trigger.dispatch")
	if d.Name != "worker.trigger.dispatch" {
		t.Fatal("no dispatch span recorded")
	}
	if d.SpanContext.TraceID().String() != "55555555555555555555555555555555" {
		t.Errorf("dispatch trace id = %s, want the payload's trace id", d.SpanContext.TraceID().String())
	}
	if !strings.HasPrefix(req.Traceparent, "00-55555555555555555555555555555555-") {
		t.Errorf("dispatched run traceparent = %q, want the dispatch span's on the payload trace", req.Traceparent)
	}
}

// No context anywhere: the dispatch becomes the trace ROOT, and the run nests
// under it. The traceparent is the dispatch span's own — a real span, not an
// invented one — and the trace id is the dispatch's, so nothing references a
// span that never existed.
func TestHandleTrigger_NoContextIsOwnRoot(t *testing.T) {
	spans, req := postTrigger(t, "", "")

	d := spanByName(spans, "worker.trigger.dispatch")
	if d.Name != "worker.trigger.dispatch" {
		t.Fatal("no dispatch span recorded")
	}
	if d.Parent.IsValid() {
		t.Errorf("dispatch has a parent (%s) with no inbound context — a fabricated link", d.Parent.SpanID().String())
	}
	if req.Traceparent == "" {
		t.Fatal("the run got no traceparent: it would start a disconnected trace")
	}
	if !strings.HasPrefix(req.Traceparent, "00-"+d.SpanContext.TraceID().String()+"-") {
		t.Errorf("dispatched run traceparent = %q, want the root dispatch span's own context", req.Traceparent)
	}
}

// A malformed delivery header must not outrank a valid payload stamp: the
// W3C propagator ignores malformed values, so blindly preferring the header
// would orphan the run from the trace the controller started.
func TestHandleTrigger_MalformedHeaderFallsBackToPayload(t *testing.T) {
	const payload = "00-77777777777777777777777777777777-8888888888888888-01"
	spans, req := postTrigger(t, "not-a-traceparent", payload)

	d := spanByName(spans, "worker.trigger.dispatch")
	if d.SpanContext.TraceID().String() != "77777777777777777777777777777777" {
		t.Errorf("dispatch trace id = %s, want the payload's: a malformed header won", d.SpanContext.TraceID().String())
	}
	if !strings.HasPrefix(req.Traceparent, "00-77777777777777777777777777777777-") {
		t.Errorf("dispatched run traceparent = %q, want the dispatch span's on the payload trace", req.Traceparent)
	}
}

// Attributes the operator needs to read the hop: which workflow, which wake
// class, which source.
func TestHandleTrigger_DispatchSpanCarriesIdentity(t *testing.T) {
	spans, _ := postTrigger(t, "", "")
	d := spanByName(spans, "worker.trigger.dispatch")
	for key, want := range map[string]string{
		"harmostes.workflow":     "pr-review-harmostes",
		"harmostes.trigger.type": "webhook",
	} {
		v, ok := attrString(d, key)
		if !ok {
			t.Errorf("dispatch span missing attribute %q", key)
		} else if want != "" && v != want {
			t.Errorf("%s = %q, want %q", key, v, want)
		}
	}
}

// A failed dispatch must be visible as an error span, not silently logged:
// the trace is where a wedged wake is diagnosed.
func TestHandleTrigger_DispatchFailureMarksSpanError(t *testing.T) {
	c := NewConsumer(ConsumerConfig{
		RunFunc: func(_ context.Context, _ RunRequest) error {
			return context.DeadlineExceeded
		},
	})
	exp := withTestTracer(t)
	req := httptest.NewRequest(http.MethodPost, "/triggers", strings.NewReader(triggerBody("")))
	req.Body = http.MaxBytesReader(nil, req.Body, 1<<20)
	rr := httptest.NewRecorder()
	c.handleTrigger(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	d := spanByName(exp.GetSpans(), "worker.trigger.dispatch")
	if d.Name != "worker.trigger.dispatch" {
		t.Fatal("no dispatch span recorded")
	}
	if d.Status.Code != codes.Error {
		t.Errorf("dispatch span status = %v, want error — a failed wake is invisible in the trace", d.Status.Code)
	}
}

// The reconciliation floor (#556) is a pass over the armed fleet, not a
// delivery. Its sweeps nest under one sweep root so a stalled armed fleet
// reads as a trace per pass instead of orphaned dispatches.
func TestArmedBackoffLoop_SweepRootNestsDispatches(t *testing.T) {
	c := NewConsumer(ConsumerConfig{
		FastPoll:              time.Millisecond,
		ArmedWaitingWorkflows: func(_ context.Context) []string { return []string{"pr-review-harmostes"} },
		RunFunc: func(_ context.Context, req RunRequest) error {
			if req.Source != "fast-poll" {
				t.Errorf("source = %q, want fast-poll", req.Source)
			}
			return nil
		},
	})
	exp := withTestTracer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.armedBackoffLoop(ctx)

	sweep := spanByName(exp.GetSpans(), "worker.fast-poll.sweep")
	if sweep.Name != "worker.fast-poll.sweep" {
		t.Fatal("no sweep root span: the reconciliation floor emits nothing")
	}
	nested := 0
	for _, s := range exp.GetSpans() {
		if s.Name == "worker.trigger.dispatch" && s.Parent.SpanID() == sweep.SpanContext.SpanID() {
			nested++
		}
	}
	if nested == 0 {
		t.Error("fast-poll dispatches did not nest under the sweep root")
	}
}

// The reconciliation floor's invariant is that an idle fleet runs ZERO passes
// (#556). The sweep root must honour it: an unconditional span would emit an
// empty `worker.fast-poll.sweep` every cadence — ~2,880/day per pod at the
// chart default — pure noise drowning the traces that matter.
func TestArmedBackoffLoop_IdleFleetEmitsNoSpans(t *testing.T) {
	c := NewConsumer(ConsumerConfig{
		FastPoll:              time.Millisecond,
		ArmedWaitingWorkflows: func(_ context.Context) []string { return nil },
		RunFunc: func(_ context.Context, _ RunRequest) error {
			t.Error("an idle fleet must dispatch nothing")
			return nil
		},
	})
	exp := withTestTracer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.armedBackoffLoop(ctx)
	if spans := exp.GetSpans(); len(spans) != 0 {
		t.Fatalf("idle fleet emitted %d spans (%v) — the sweep root opens on empty passes",
			len(spans), spanNames(spans))
	}
}

// A mutation probe in test form: the guard must not swallow the cadence.
// An empty armed set is the stalled case and still earns the backoff, so the
// loop keeps ticking without emitting anything.
func TestArmedBackoffLoop_IdlePassStillBacksOff(t *testing.T) {
	passes := 0
	c := NewConsumer(ConsumerConfig{
		FastPoll:              10 * time.Millisecond,
		ArmedWaitingWorkflows: func(_ context.Context) []string { passes++; return nil },
		RunFunc:               func(_ context.Context, _ RunRequest) error { return nil },
	})
	exp := withTestTracer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.armedBackoffLoop(ctx)
	if passes == 0 {
		t.Fatal("the idle pass stopped ticking — the guard swallowed the cadence loop")
	}
	if len(exp.GetSpans()) != 0 {
		t.Fatalf("idle passes emitted spans: %v", spanNames(exp.GetSpans()))
	}
}

// The pool pod's logger must be the trace-aware one. slog.Default() drops the
// traceHandler wrapper, so dispatch records (job created, session cap full,
// deduped) never carry trace_id/span_id and never join the dispatch span —
// the logs explaining the hop would be unlinked text (#928). This pins the
// construction, not just the import.
func TestConsumerLoggerIsTraceAware(t *testing.T) {
	var buf bytes.Buffer
	lg := observability.NewLogger("harmostes-consumer", &buf)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) }()

	ctx, span := observability.Tracer().Start(context.Background(), "worker.trigger.dispatch")
	defer span.End()
	lg.InfoContext(ctx, "dispatch")

	rec := map[string]any{}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log is not JSON: %v", err)
	}
	if rec["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("dispatch log carries no trace_id (%v) — it cannot be joined to the dispatch span", rec["trace_id"])
	}
	if rec["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("dispatch log span_id = %v, want the dispatch span's", rec["span_id"])
	}
}

// dispatchTraceparent is the fallback rule in isolation: a recording span
// yields its own traceparent; an unrecorded one must not silently drop the
// inherited context.
func TestDispatchTraceparent(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prevTP) })

	ctx, span := observability.Tracer().Start(context.Background(), "worker.trigger.dispatch")
	defer span.End()
	if got := dispatchTraceparent(ctx, "00-inherited"); !validTraceparent(got) {
		t.Fatalf("with a recording span, traceparent = %q — must be the span's own", got)
	}
	if got := dispatchTraceparent(context.Background(), "00-inherited"); got != "00-inherited" {
		t.Fatalf("no recording span must NOT drop the inherited context: got %q", got)
	}
	if got := dispatchTraceparent(context.Background(), ""); got != "" {
		t.Fatalf("nothing to inherit must yield empty, got %q", got)
	}
}
