package dapr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// withW3C installs the W3C traceparent propagator + an always-sampling tracer
// provider for the duration of a test, so the active span context is valid and
// injectable. Restored on cleanup so it cannot leak into other tests.
func withW3C(t *testing.T) {
	t.Helper()
	prevProp := otel.GetTextMapPropagator()
	prevTP := otel.GetTracerProvider()
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevProp)
		otel.SetTracerProvider(prevTP)
	})
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample())))
}

// validTraceparent is a loose shape check: "00-<32hex>-<16hex>-<2hex>".
func validTraceparent(s string) bool {
	return strings.HasPrefix(s, "00-") && strings.Count(s, "-") == 3 && len(s) == 55
}

// TestInjectsTraceparent stamps a parent span on the context and asserts every
// Dapr method carries the traceparent header on the outbound request — the
// trace-join that makes daprd's state/pubsub spans children of the caller.
func TestInjectsTraceparent(t *testing.T) {
	withW3C(t)
	tracer := otel.Tracer("test")

	cases := []struct {
		name string
		call func(c *HTTPClient, ctx context.Context) error
	}{
		{"GetState", func(c *HTTPClient, ctx context.Context) error { _, err := c.GetState(ctx, "store", "key"); return err }},
		{"SaveState", func(c *HTTPClient, ctx context.Context) error { return c.SaveState(ctx, "store", "key", "v") }},
		{"DeleteState", func(c *HTTPClient, ctx context.Context) error { return c.DeleteState(ctx, "store", "key") }},
		{"Publish", func(c *HTTPClient, ctx context.Context) error { return c.Publish(ctx, "pubsub", "topic", "{}") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("traceparent")
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`""`)) // JSON-encoded empty string
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()

			ctx, span := tracer.Start(context.Background(), "parent")
			defer span.End()

			if err := tc.call(New(srv.URL), ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !validTraceparent(got) {
				t.Fatalf("%s: traceparent not injected / malformed: %q", tc.name, got)
			}
		})
	}
}

// TestNoSpanNoTraceparent asserts that a bare context (no active span) injects
// nothing — telemetry disabled or an out-of-band call does not fabricate a span.
func TestNoSpanNoTraceparent(t *testing.T) {
	withW3C(t)

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New(srv.URL).Publish(context.Background(), "pubsub", "topic", "{}"); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected no traceparent with no active span, got %q", got)
	}
}

// inMemoryTelemetry installs a synchronous in-memory trace exporter so a test
// can assert on emitted spans. The global tracer provider is restored on cleanup.
func inMemoryTelemetry(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	te := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(te),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})
	return te
}

// spanByName returns the emitted span stub with the given name, or nil.
func spanByName(te *tracetest.InMemoryExporter, name string) *tracetest.SpanStub {
	spans := te.GetSpans()
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// TestTracingEmitsClientSpan asserts the decorator creates a dapr.publish client
// span with semantic attributes + propagates its span context into the inner
// client so daprd's runtime span would nest under it (the trace-join). Volume,
// latency, and error counts are owned by daprd's dapr_* metrics, not asserted
// here.
func TestTracingEmitsClientSpan(t *testing.T) {
	te := inMemoryTelemetry(t)

	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := Tracing(New(srv.URL)).Publish(context.Background(), "pubsub", "wiki.docs", "{}"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	span := spanByName(te, "dapr.publish")
	if span == nil {
		t.Fatalf("no dapr.publish span emitted; got %d spans", len(te.GetSpans()))
	}
	attrVal := func(k string) string {
		for _, a := range span.Attributes {
			if string(a.Key) == k {
				return a.Value.AsString()
			}
		}
		return ""
	}
	if got := attrVal("rpc.system"); got != "dapr" {
		t.Errorf("rpc.system = %q, want dapr", got)
	}
	if got := attrVal("messaging.destination.name"); got != "wiki.docs" {
		t.Errorf("messaging.destination.name = %q, want wiki.docs", got)
	}
	if span.Status.Code != codes.Unset {
		t.Errorf("status = %v, want Unset (ok)", span.Status.Code)
	}
	// The decorator's span context propagated into the inner client → a W3C
	// traceparent reached the server; daprd's runtime span would nest under it.
	if !validTraceparent(gotTraceparent) {
		t.Errorf("no valid traceparent propagated to server: %q", gotTraceparent)
	}
}

// TestTracingErrorOutcome asserts a failing call records the error + marks the
// client span errored.
func TestTracingErrorOutcome(t *testing.T) {
	te := inMemoryTelemetry(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := Tracing(New(srv.URL)).Publish(context.Background(), "pubsub", "topic", "{}"); err == nil {
		t.Fatal("expected error from HTTP 500")
	}

	span := spanByName(te, "dapr.publish")
	if span == nil {
		t.Fatalf("no dapr.publish span emitted")
	}
	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
}

// TestTracingDisabledNoop asserts that with no provider configured (the local
// dev / unit-test default) the decorator never panics and is behaviour-neutral.
func TestTracingDisabledNoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GetState expects 200 (+ a JSON body); the other ops accept 204.
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`""`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := Tracing(New(srv.URL))
	if err := c.Publish(context.Background(), "pubsub", "t", "{}"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := c.GetState(context.Background(), "store", "key"); err != nil {
		t.Fatalf("getstate: %v", err)
	}
}
