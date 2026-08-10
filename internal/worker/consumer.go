package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"

	"github.com/tibrezus/harmostes/internal/observability"
)

// ---------------------------------------------------------------------------
// Consumer — pub/sub-triggered workflow execution.
//
// The consumer runs as a long-lived HTTP server inside a worker pod. The pod's
// daprd sidecar discovers the subscription via GET /dapr/subscribe, then
// delivers trigger events as POST /triggers.
//
// This replaces the batchv1.Job-per-run model:
//
//   OLD: controller → batchv1.Job → one-shot pod → dead pod accumulates
//   NEW: controller → pub/sub → consumer pod → runs workflow → ready for next
//
// Single-flight: each pod processes ONE trigger at a time. Horizontal scaling
// (more pods) provides concurrency. The pod is idle between events — no dead
// pods, no accumulation.
// ---------------------------------------------------------------------------

// daprSubscription is the response to GET /dapr/subscribe. It tells daprd
// which pub/sub component and topic to deliver, and which route to call.
type daprSubscription struct {
	PubsubName string `json:"pubsubname"`
	Topic      string `json:"topic"`
	Route      string `json:"route"`
}

// ConsumerConfig configures the pub/sub consumer.
type ConsumerConfig struct {
	HTTPPort   string  // port for the HTTP server (daprd calls this)
	PubsubName string  // Dapr pub/sub component name (default "pubsub")
	Topic      string  // topic to subscribe to (default "harmostes-triggers")
	RunFunc    RunFunc // the function that executes a workflow
	Logger     *slog.Logger
}

// RunFunc executes a single workflow run. The consumer shells out to itself
// (/proc/self/exe) in one-shot Job mode — this gives process isolation (fresh
// process per run, no state leaks) without pod bloat. The consumer pod is the
// supervisor; it spawns worker processes and monitors them. When idle, the
// pod just waits (minimal resource usage).
type RunFunc func(ctx context.Context, workflow, namespace, source, attemptName, traceparent string) error

// Consumer is the pub/sub-triggered workflow executor.
type Consumer struct {
	cfg    ConsumerConfig
	server *http.Server
	mu     sync.Mutex // single-flight: one concurrent run per pod
}

// NewConsumer creates a pub/sub consumer.
func NewConsumer(cfg ConsumerConfig) *Consumer {
	if cfg.PubsubName == "" {
		cfg.PubsubName = "pubsub"
	}
	if cfg.Topic == "" {
		cfg.Topic = "harmostes-triggers"
	}
	if cfg.HTTPPort == "" {
		cfg.HTTPPort = "8084"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Consumer{cfg: cfg}
}

// Start launches the HTTP server (blocking). It handles:
//   - GET  /dapr/subscribe — tells daprd which topic to deliver
//   - POST /triggers       — processes a trigger event
//   - GET  /healthz        — liveness/readiness probe
func (c *Consumer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/dapr/subscribe", c.handleSubscribe)
	mux.HandleFunc("/triggers", c.handleTrigger)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c.server = &http.Server{
		Addr:    ":" + c.cfg.HTTPPort,
		Handler: mux,
	}

	c.cfg.Logger.Info("consumer listening",
		"port", c.cfg.HTTPPort,
		"topic", c.cfg.Topic,
		"pubsub", c.cfg.PubsubName,
	)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.server.Shutdown(shutdownCtx)
	}()

	return c.server.ListenAndServe()
}

// handleSubscribe returns the Dapr pub/sub subscription configuration.
// daprd calls this on startup to discover which topics to deliver.
func (c *Consumer) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	subs := []daprSubscription{{
		PubsubName: c.cfg.PubsubName,
		Topic:      c.cfg.Topic,
		Route:      "/triggers",
	}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(subs)
}

// handleTrigger processes a trigger event delivered by daprd.
//
// The event is a CloudEvent wrapping a TriggerEvent payload. The handler:
//  1. Parses the CloudEvent + TriggerEvent
//  2. Fetches the Workflow CR by name
//  3. Runs the workflow (single-flight — blocks if another run is active)
//  4. Returns 200 (ACK) on success, 500 (NACK) on failure
//
// NACK causes daprd to re-deliver the message (at-least-once semantics via
// Redis Streams). The consumer must be idempotent to handle re-delivery.
func (c *Consumer) handleTrigger(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0)
	// Read the body
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}

	// Parse the CloudEvent
	var event cloudevents.Event
	if err := json.Unmarshal(body, &event); err != nil {
		c.cfg.Logger.Error("failed to parse cloud event", "error", err, "body_len", len(body))
		http.Error(w, "invalid cloud event", http.StatusBadRequest)
		return
	}

	// Parse the TriggerEvent from the data field
	var trigger TriggerEvent
	if err := event.DataAs(&trigger); err != nil {
		c.cfg.Logger.Error("failed to parse trigger event", "error", err)
		http.Error(w, "invalid trigger data", http.StatusBadRequest)
		return
	}

	c.cfg.Logger.Info("trigger received",
		"workflow", trigger.Workflow,
		"trigger_type", trigger.TriggerType,
		"revision", trigger.Revision,
		"event_id", event.ID(),
	)

	// Single-flight: acquire the lock. If another run is active, return 503
	// so daprd re-delivers to another pod (or retries later).
	if !c.mu.TryLock() {
		c.cfg.Logger.Warn("concurrent run rejected (single-flight)", "workflow", trigger.Workflow)
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer c.mu.Unlock()

	// Run the workflow (shells out to /proc/self/exe in one-shot mode)
	runCtx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	if err := c.cfg.RunFunc(runCtx, trigger.Workflow, trigger.Namespace, trigger.Source, trigger.AttemptName, trigger.Traceparent); err != nil {
		c.cfg.Logger.Error("workflow run failed", "workflow", trigger.Workflow, "error", err)
		http.Error(w, fmt.Sprintf("run failed: %v", err), http.StatusInternalServerError)
		return
	}

	c.cfg.Logger.Info("trigger processed", "workflow", trigger.Workflow, "trigger_type", trigger.TriggerType)
	w.WriteHeader(http.StatusOK)
}

// TriggerEvent is the payload published by the controller when a workflow is
// due. It mirrors the controller's TriggerEvent struct (internal/controller/
// trigger.go) — kept as a local copy to avoid a circular import.
type TriggerEvent struct {
	Workflow    string `json:"workflow"`
	Namespace   string `json:"namespace"`
	Revision    string `json:"revision,omitempty"`
	Source      string `json:"source,omitempty"`
	TriggerType string `json:"triggerType"`
	Traceparent string `json:"traceparent,omitempty"`
	AttemptName string `json:"attemptName,omitempty"`
}

// buildChildEnv constructs the environment for the exec'd one-shot worker.
// It scrubs HARMOSTES_CONSUMER_MODE so the child runs in one-shot mode
// (not consumer mode), then appends the workflow-specific vars.
func buildChildEnv(parentEnv []string, workflow, namespace, source, attemptName, traceparent string) []string {
	childEnv := make([]string, 0, len(parentEnv)+5)
	for _, e := range parentEnv {
		if !strings.HasPrefix(e, "HARMOSTES_CONSUMER_MODE=") {
			childEnv = append(childEnv, e)
		}
	}
	childEnv = append(childEnv,
		"HARMOSTES_WORKFLOW="+workflow,
		"HARMOSTES_NAMESPACE="+namespace,
		// Prevent the exec'd worker from shutting down the shared daprd
		// sidecar (which would kill the consumer process).
		"HARMOSTES_NO_DAPR_SHUTDOWN=true",
	)
	if source != "" {
		childEnv = append(childEnv, "HARMOSTES_SOURCE="+source)
	}
	if attemptName != "" {
		childEnv = append(childEnv, "HARMOSTES_ATTEMPT="+attemptName)
	}
	if traceparent != "" {
		childEnv = append(childEnv, observability.TraceparentCarrierKey+"="+traceparent)
	}
	return childEnv
}

// RunConsumer is the entry point for consumer mode. Called from main when
// HARMOSTES_CONSUMER_MODE is set. The RunFunc shells out to /proc/self/exe
// in one-shot Job mode for each trigger event.
func RunConsumer(ctx context.Context) error {
	logger := slog.Default().With("component", "harmostes-consumer")

	// The RunFunc execs ourselves in one-shot mode.
	runFunc := func(runCtx context.Context, workflow, namespace, source, attemptName, traceparent string) error {
		cmd := exec.CommandContext(runCtx, "/proc/self/exe")
		cmd.Env = buildChildEnv(os.Environ(), workflow, namespace, source, attemptName, traceparent)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	consumer := NewConsumer(ConsumerConfig{
		HTTPPort:   envOr("HARMOSTES_CONSUMER_PORT", "8084"),
		PubsubName: envOr("HARMOSTES_PUBSUB_NAME", "pubsub"),
		Topic:      envOr("HARMOSTES_TRIGGER_TOPIC", "harmostes-triggers"),
		RunFunc:    runFunc,
		Logger:     logger,
	})

	return consumer.Start(ctx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
