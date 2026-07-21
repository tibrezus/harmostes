package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// EventHub tests
// ---------------------------------------------------------------------------

func TestEventHubSubscribePublish(t *testing.T) {
	hub := NewEventHub()
	sub, cancel := hub.Subscribe("my-pipeline")
	defer cancel()

	if hub.SubscriberCount("my-pipeline") != 1 {
		t.Errorf("subscriber count = %d, want 1", hub.SubscriberCount("my-pipeline"))
	}

	ev := Event{
		Event:    "node.started",
		Pipeline: "my-pipeline",
		Node:     "deploy",
		NodeType: "vela-app",
	}
	hub.Publish(ev)

	select {
	case got := <-sub.ch:
		if got.Event != "node.started" {
			t.Errorf("event = %q, want node.started", got.Event)
		}
		if got.Node != "deploy" {
			t.Errorf("node = %q, want deploy", got.Node)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestEventHubUnsubscribe(t *testing.T) {
	hub := NewEventHub()
	_, cancel := hub.Subscribe("my-pipeline")

	if hub.SubscriberCount("my-pipeline") != 1 {
		t.Fatalf("subscriber count = %d, want 1", hub.SubscriberCount("my-pipeline"))
	}

	cancel()

	if hub.SubscriberCount("my-pipeline") != 0 {
		t.Errorf("subscriber count after cancel = %d, want 0", hub.SubscriberCount("my-pipeline"))
	}

	// Publishing after cancel should not panic.
	hub.Publish(Event{Event: "node.started", Pipeline: "my-pipeline"})
}

func TestEventHubMultipleSubscribers(t *testing.T) {
	hub := NewEventHub()
	sub1, cancel1 := hub.Subscribe("my-pipeline")
	defer cancel1()
	sub2, cancel2 := hub.Subscribe("my-pipeline")
	defer cancel2()

	hub.Publish(Event{Event: "node.completed", Pipeline: "my-pipeline", Node: "agent", Status: "green"})

	for i, sub := range []<-chan Event{sub1.ch, sub2.ch} {
		select {
		case got := <-sub:
			if got.Status != "green" {
				t.Errorf("subscriber %d: status = %q, want green", i, got.Status)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d: timed out", i)
		}
	}
}

func TestEventHubIsolationByPipeline(t *testing.T) {
	hub := NewEventHub()
	subA, cancelA := hub.Subscribe("pipeline-a")
	defer cancelA()
	subB, cancelB := hub.Subscribe("pipeline-b")
	defer cancelB()

	hub.Publish(Event{Event: "node.started", Pipeline: "pipeline-a", Node: "node-1"})

	// Sub A should receive.
	select {
	case <-subA.ch:
		// ok
	case <-time.After(time.Second):
		t.Fatal("subA: timed out")
	}

	// Sub B should NOT receive (wrong pipeline).
	select {
	case ev := <-subB.ch:
		t.Errorf("subB received event for wrong pipeline: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// ok — expected timeout
	}
}

func TestEventHubDropsOnFullChannel(t *testing.T) {
	hub := NewEventHub()
	// Create subscriber with tiny buffer (simulated by not reading).
	sub, cancel := hub.Subscribe("my-pipeline")
	defer cancel()

	// Publish more than the buffer (64). The overflow events should be
	// silently dropped, not block.
	for i := 0; i < 100; i++ {
		hub.Publish(Event{Event: "node.started", Pipeline: "my-pipeline"})
	}

	// Should be able to read at least the first 64 (buffered).
	count := 0
loop:
	for {
		select {
		case <-sub.ch:
			count++
		default:
			break loop
		}
	}
	if count > 64 {
		t.Errorf("received %d events, expected at most 64 (buffer size)", count)
	}
}

// ---------------------------------------------------------------------------
// Dapr subscription endpoint tests
// ---------------------------------------------------------------------------

func TestHandleDaprSubscribe(t *testing.T) {
	srv := newTestServerWithHub(t)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dapr/subscribe", nil)
	srv.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}

	var subs []daprSubscription
	if err := json.Unmarshal(resp.Body.Bytes(), &subs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(subs) != 1 {
		t.Fatalf("subscriptions = %d, want 1", len(subs))
	}
	if subs[0].PubSubName != "harmostes-pubsub" {
		t.Errorf("pubsubname = %q, want harmostes-pubsub", subs[0].PubSubName)
	}
	if subs[0].Topic != "harmostes-events" {
		t.Errorf("topic = %q, want harmostes-events", subs[0].Topic)
	}
	if subs[0].Route != "/dapr/events" {
		t.Errorf("route = %q, want /dapr/events", subs[0].Route)
	}
}

func TestHandleDaprEvent(t *testing.T) {
	srv := newTestServerWithHub(t)

	// Subscribe to the pipeline so we can verify the event was published.
	sub, cancel := srv.hub.Subscribe("test-pipeline")
	defer cancel()

	cloudEvent := `{"data":{"event":"node.completed","pipeline":"test-pipeline","node":"deploy","status":"green"}}`
	resp := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/dapr/events", strings.NewReader(cloudEvent))
	srv.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}

	select {
	case got := <-sub.ch:
		if got.Event != "node.completed" {
			t.Errorf("event = %q, want node.completed", got.Event)
		}
		if got.Node != "deploy" {
			t.Errorf("node = %q, want deploy", got.Node)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event in hub")
	}
}

func TestHandleDaprEventNoPipeline(t *testing.T) {
	srv := newTestServerWithHub(t)

	// Event without a pipeline field — should be silently ignored.
	cloudEvent := `{"data":{"event":"some.other.event"}}`
	resp := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/dapr/events", strings.NewReader(cloudEvent))
	srv.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
}

func TestHandleDaprEventBadJSON(t *testing.T) {
	srv := newTestServerWithHub(t)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/dapr/events", strings.NewReader("not json"))
	srv.Routes().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

// ---------------------------------------------------------------------------
// SSE endpoint tests
// ---------------------------------------------------------------------------

func TestHandlePipelineSSE(t *testing.T) {
	srv := newTestServerWithHub(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// SSE is a long-lived connection. We connect via HTTP client, verify the
	// initial comment arrives, then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/pipelines/my-pipe/events", nil)
	req.Header.Set("X-Harmostes-Dev-User", "alice")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect SSE: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	// Wait for subscriber registration.
	waitFor(t, func() bool {
		return srv.hub.SubscriberCount("my-pipe") == 1
	})

	// Read the initial connection comment.
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if !strings.Contains(body, ": connected to pipeline my-pipe") {
		t.Errorf("initial response does not contain connection confirmation: %q", body)
	}
}

func TestHandlePipelineSSEDeliversEvents(t *testing.T) {
	srv := newTestServerWithHub(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/pipelines/test-pipe/events", nil)
	req.Header.Set("X-Harmostes-Dev-User", "alice")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect SSE: %v", err)
	}
	defer resp.Body.Close()

	// Wait for subscriber registration.
	waitFor(t, func() bool {
		return srv.hub.SubscriberCount("test-pipe") == 1
	})

	// Publish an event.
	srv.hub.Publish(Event{
		Event:    "node.completed",
		Pipeline: "test-pipe",
		Node:     "agent",
		Status:   "green",
	})

	// Read from the stream until we see the event.
	buf := make([]byte, 1024)
	deadline := time.After(2 * time.Second)
	var body strings.Builder
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for event in SSE stream; got: %s", body.String())
		default:
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body.Write(buf[:n])
			if strings.Contains(body.String(), `"event":"node.completed"`) {
				if !strings.Contains(body.String(), `"status":"green"`) {
					t.Errorf("event missing status: %s", body.String())
				}
				return
			}
		}
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandlePipelineSSENoName(t *testing.T) {
	srv := newTestServerWithHub(t)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// An empty name won't match the route pattern; it'll 404 through the mux.
	resp, err := http.Get(ts.URL + "/api/pipelines//events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	// Without auth headers, we get 401 before reaching the handler.
	// This verifies the SSE endpoint is behind the auth middleware.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (auth required)", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestServerWithHub(t *testing.T) *Server {
	t.Helper()
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	return &Server{
		namespace: "default",
		logger:    slog.Default(),
		templates: tmpl,
		hub:       NewEventHub(),
	}
}

// withTestIdentity injects an identity into the request context (same pattern
// as the auth middleware).
func withTestIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, identityKey, &Identity{Username: "alice"})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
