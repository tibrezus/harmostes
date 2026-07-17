// Package dapr is a tiny client for the Dapr sidecar (state store + pub/sub) at
// localhost:3500. It is the deterministic event-system application layer that
// surrounds harmostes: state for skip/dedup, pub/sub for choreography +
// observability. Dapr abstracts the backend (Valkey today; swap by changing the
// Component CR) so this client never talks to Valkey directly.
package dapr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/tibrezus/harmostes/internal/observability"
)

// Client is the Dapr surface harmostes uses.
type Client interface {
	// GetState returns the stored value ("" if absent). A missing key is not an
	// error.
	GetState(ctx context.Context, store, key string) (string, error)
	// SaveState writes a single key.
	SaveState(ctx context.Context, store, key, value string) error
	// DeleteState removes a key (idempotent).
	DeleteState(ctx context.Context, store, key string) error
	// Publish sends a JSON payload on a pub/sub topic (best-effort; returns nil on
	// 200/204).
	Publish(ctx context.Context, pubsub, topic, jsonPayload string) error
}

// HTTPClient talks to a Dapr sidecar over HTTP.
type HTTPClient struct {
	BaseURL string // e.g. http://localhost:3500
	HTTP    *http.Client
}

// New returns a client for the sidecar at baseURL (default http://localhost:3500).
func New(baseURL string) *HTTPClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:3500" // not localhost: Go may resolve it to IPv6 ::1, which daprd doesn't bind
	}
	return &HTTPClient{BaseURL: baseURL, HTTP: &http.Client{}}
}

// inject stamps the active W3C trace context (traceparent + tracestate) onto the
// outbound request so the Dapr sidecar creates its state/pubsub span as a child
// of the harmostes span that triggered the call — the trace-join (Phase 2). No-op
// when no span is active in ctx or telemetry is disabled (the global propagator
// is no-op until observability.Init configures W3C).
func inject(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

func (c *HTTPClient) GetState(ctx context.Context, store, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1.0/state/%s/%s", c.BaseURL, store, key), nil)
	if err != nil {
		return "", err
	}
	inject(ctx, req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dapr get-state: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Dapr returns the value JSON-encoded (a quoted string for a string value).
	var v string
	if json.Unmarshal(bytes.TrimSpace(b), &v) == nil {
		return v, nil
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *HTTPClient) SaveState(ctx context.Context, store, key, value string) error {
	body, err := json.Marshal([]map[string]any{{"key": key, "value": value}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/state/%s", c.BaseURL, store), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	inject(ctx, req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dapr save-state: %s", resp.Status)
	}
	return nil
}

func (c *HTTPClient) DeleteState(ctx context.Context, store, key string) error {
	// Dapr's bulk-delete: POST an array with operation=delete (the form the
	// existing bash scripts use, version-portable).
	body, err := json.Marshal([]map[string]any{{"key": key, "operation": "delete"}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/state/%s", c.BaseURL, store), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	inject(ctx, req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200/204 = deleted; 404 (already gone) is also success.
	if resp.StatusCode > http.StatusNoContent {
		return nil
	}
	return nil
}

func (c *HTTPClient) Publish(ctx context.Context, pubsub, topic, jsonPayload string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1.0/publish/%s/%s", c.BaseURL, pubsub, topic),
		strings.NewReader(jsonPayload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	inject(ctx, req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dapr publish %s/%s: %s", pubsub, topic, resp.Status)
	}
	return nil
}

// Tracing returns a Client decorator that emits a client span for every
// building-block call, so harmostes's side of the Dapr boundary is observable
// and interlocks with daprd's now-native OTel. daprd emits the matching runtime
// span as a CHILD of this span: the inner HTTP client injects this span's W3C
// traceparent, yielding a continuous
//
//	harmostes.phase → dapr.<op> → daprd.<op> trace.
//
// Span-only by design: daprd already counts + times these calls in its own
// dapr_* metrics (scraped), so harmostes emits only the one thing daprd
// structurally cannot — the initiator/client span (which phase caused it, which
// store/topic, ok/error status). Volume, latency, and error rate for the Dapr
// dependency are read from daprd's dapr_* metrics, not re-counted here.
//
// A nil client returns nil (a no-op wire-up). With telemetry disabled (no Init)
// the tracer is no-op, so this is zero-overhead and behaviour-preserving.
func Tracing(c Client) Client {
	if c == nil {
		return nil
	}
	return &tracingClient{inner: c}
}

// tracingClient implements Client by delegating to inner, wrapping each call in
// a dapr.<op> client span with semantic attributes (rpc.system=dapr so the
// backend's service-map / dependency views group Dapr calls) + error/status.
type tracingClient struct{ inner Client }

func (t *tracingClient) GetState(ctx context.Context, store, key string) (string, error) {
	var v string
	err := t.run(ctx, "state.get", func(ctx context.Context) error {
		var e error
		v, e = t.inner.GetState(ctx, store, key)
		return e
	},
		attribute.String("rpc.system", "dapr"),
		attribute.String("rpc.method", "state.get"),
		attribute.String("dapr.store", store),
		attribute.String("dapr.key", key),
	)
	return v, err
}

func (t *tracingClient) SaveState(ctx context.Context, store, key, value string) error {
	return t.run(ctx, "state.save", func(ctx context.Context) error {
		return t.inner.SaveState(ctx, store, key, value)
	},
		attribute.String("rpc.system", "dapr"),
		attribute.String("rpc.method", "state.save"),
		attribute.String("dapr.store", store),
		attribute.String("dapr.key", key),
	)
}

func (t *tracingClient) DeleteState(ctx context.Context, store, key string) error {
	return t.run(ctx, "state.delete", func(ctx context.Context) error {
		return t.inner.DeleteState(ctx, store, key)
	},
		attribute.String("rpc.system", "dapr"),
		attribute.String("rpc.method", "state.delete"),
		attribute.String("dapr.store", store),
		attribute.String("dapr.key", key),
	)
}

func (t *tracingClient) Publish(ctx context.Context, pubsub, topic, jsonPayload string) error {
	return t.run(ctx, "publish", func(ctx context.Context) error {
		return t.inner.Publish(ctx, pubsub, topic, jsonPayload)
	},
		attribute.String("rpc.system", "dapr"),
		attribute.String("rpc.method", "publish"),
		attribute.String("messaging.system", "dapr"),
		attribute.String("messaging.destination.name", topic),
		attribute.String("dapr.pubsub", pubsub),
		attribute.String("dapr.topic", topic),
	)
}

// run executes fn under a dapr.<op> client span, recording the error/status on
// it. The span context flows into fn, so the inner client's W3C injection nests
// daprd's runtime span under it (the trace-join). Latency + outcome counts are
// owned by daprd's dapr_* metrics, not re-counted here.
func (t *tracingClient) run(ctx context.Context, op string, fn func(context.Context) error, attrs ...attribute.KeyValue) error {
	ctx, span := observability.Tracer().Start(ctx, "dapr."+op, trace.WithAttributes(attrs...))
	defer span.End()
	if err := fn(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}
