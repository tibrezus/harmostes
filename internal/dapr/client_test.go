package dapr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
