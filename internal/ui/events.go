package ui

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// EventHub — in-memory fan-out bus for pipeline lifecycle events
// ---------------------------------------------------------------------------

// Event is a lifecycle event delivered to SSE clients. It mirrors the wire
// format published by the graph executor to the Dapr pub/sub topic.
type Event struct {
	Event      string         `json:"event"`                // pipeline.started, node.started, node.completed, etc.
	Pipeline   string         `json:"pipeline"`             // pipeline CR name
	Node       string         `json:"node,omitempty"`       // node ID (empty for pipeline-level)
	NodeType   string         `json:"nodeType,omitempty"`   // node type (agent, gate, etc.)
	Status     string         `json:"status,omitempty"`     // green | failed
	Feedback   string         `json:"feedback,omitempty"`   // gate feedback or error message
	Outputs    map[string]any `json:"outputs,omitempty"`    // node outputs (agent metrics, etc.)
	DurationMs int64          `json:"durationMs,omitempty"` // execution duration in ms
	Timestamp  time.Time      `json:"timestamp"`            // event creation time (UTC)
}

// subscriber is a single SSE client listening for events on a pipeline.
type subscriber struct {
	ch     chan Event
	closed chan struct{}
	once   sync.Once
}

func (s *subscriber) close() {
	s.once.Do(func() {
		close(s.ch)
		close(s.closed)
	})
}

// EventHub is an in-memory fan-out bus. It receives events from the Dapr
// subscription endpoint (POST /dapr/events) and delivers them to SSE clients
// connected via GET /api/pipelines/{name}/events.
//
// The hub is per-process (per UI pod). Each UI pod's daprd sidecar subscribes
// to the pub/sub topic independently; since the backing store is Redis
// pub/sub (broadcast), every pod receives every event. SSE clients connected
// to any pod therefore receive all events for their pipeline.
type EventHub struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{} // pipeline name → subscribers
}

// NewEventHub creates an empty EventHub.
func NewEventHub() *EventHub {
	return &EventHub{
		subs: make(map[string]map[*subscriber]struct{}),
	}
}

// Subscribe registers a new SSE subscriber for the given pipeline. Returns the
// subscriber channel and a cancel function to deregister. The channel has a
// buffer of 64 events; if the client is slow, events are dropped (best-effort
// delivery — SSE is a live stream, not a durable queue).
func (h *EventHub) Subscribe(pipeline string) (*subscriber, func()) {
	sub := &subscriber{
		ch:     make(chan Event, 64),
		closed: make(chan struct{}),
	}

	h.mu.Lock()
	if h.subs[pipeline] == nil {
		h.subs[pipeline] = make(map[*subscriber]struct{})
	}
	h.subs[pipeline][sub] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		if subs, ok := h.subs[pipeline]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.subs, pipeline)
			}
		}
		h.mu.Unlock()
		sub.close()
	}

	return sub, cancel
}

// Publish fans out an event to all subscribers for the event's pipeline.
// Non-blocking: if a subscriber's channel is full, the event is dropped for
// that subscriber (logged but not fatal).
func (h *EventHub) Publish(ev Event) {
	// Copy subscriber pointers under the lock, then iterate the copy without
	// the lock. This avoids a race between Publish (iterating the map) and
	// cancel (deleting from the map).
	h.mu.RLock()
	subs := make([]*subscriber, 0, len(h.subs[ev.Pipeline]))
	for sub := range h.subs[ev.Pipeline] {
		subs = append(subs, sub)
	}
	h.mu.RUnlock()

	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		default:
			// Channel full — drop event for this slow subscriber.
		}
	}
}

// SubscriberCount returns the number of active subscribers for a pipeline.
// Used in tests and diagnostics.
func (h *EventHub) SubscriberCount(pipeline string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[pipeline])
}

// ---------------------------------------------------------------------------
// Dapr subscription endpoints
// ---------------------------------------------------------------------------

// daprSubscription is the declarative subscription format returned by
// GET /dapr/subscribe. Dapr reads this to know which topics to deliver to
// this app and at which route.
type daprSubscription struct {
	PubSubName string `json:"pubsubname"`
	Topic      string `json:"topic"`
	Route      string `json:"route"`
}

// handleDaprSubscribe returns the Dapr subscription declaration. This endpoint
// is called by the daprd sidecar at startup to discover which pub/sub topics
// the app wants to receive. It must be unauthenticated (daprd doesn't send
// Authentik headers).
func (s *Server) handleDaprSubscribe(w http.ResponseWriter, r *http.Request) {
	subs := []daprSubscription{
		{
			PubSubName: "harmostes-pubsub",
			Topic:      "harmostes-events",
			Route:      "/dapr/events",
		},
	}
	s.writeJSON(w, http.StatusOK, subs)
}

// daprCloudEvent is the CloudEvent envelope that Dapr wraps published messages
// in before delivering to subscribers.
type daprCloudEvent struct {
	Data Event `json:"data"`
}

// handleDaprEvent receives a lifecycle event from the daprd sidecar (delivered
// as a CloudEvent). It unwraps the data and publishes to the event hub. Must be
// unauthenticated — daprd is a trusted in-pod sidecar.
func (s *Server) handleDaprEvent(w http.ResponseWriter, r *http.Request) {
	var ce daprCloudEvent
	if err := json.NewDecoder(r.Body).Decode(&ce); err != nil {
		s.logger.Warn("dapr event: decode cloud event", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if ce.Data.Pipeline == "" {
		// Not a lifecycle event — ignore silently.
		w.WriteHeader(http.StatusOK)
		return
	}

	if s.logger.Enabled(r.Context(), slog.LevelDebug) {
		s.logger.Debug("dapr event received",
			"event", ce.Data.Event,
			"pipeline", ce.Data.Pipeline,
			"node", ce.Data.Node,
			"status", ce.Data.Status,
		)
	}

	s.hub.Publish(ce.Data)
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// SSE endpoint
// ---------------------------------------------------------------------------

// handlePipelineSSE streams lifecycle events for a specific pipeline as
// Server-Sent Events. The client connects via EventSource (browser API) and
// receives events in real-time as the pipeline executes.
//
// SSE format: "data: <json>\n\n" per event, with periodic heartbeat comments
// (": keepalive\n\n") to keep the connection alive through proxies.
func (s *Server) handlePipelineSSE(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.writeAPIError(w, http.StatusBadRequest, "pipeline name required")
		return
	}

	// Set SSE headers.
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	// Subscribe to events for this pipeline.
	sub, cancel := s.hub.Subscribe(name)
	defer cancel()

	// Send initial connection confirmation.
	fmt.Fprintf(w, ": connected to pipeline %s\n\n", name)
	flusher.Flush()

	// Heartbeat ticker — keeps the connection alive through proxies/load balancers.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected.
			return

		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				s.logger.Error("sse marshal event", "err", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()

		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
