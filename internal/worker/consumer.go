package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"

	k8s "github.com/tibrezus/harmostes/internal/k8s"

	"strconv"
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
type RunFunc func(ctx context.Context, req RunRequest) error

// RunRequest is the one-shot run request the consumer hands to the worker
// binary (itself, exec'd). Named fields — not positional strings.
type RunRequest struct {
	Workflow    string
	Namespace   string
	Source      string
	Attempt     string
	Traceparent string
	Pr          string
	Action      string
	Revision    string
	PrTitle     string
}

// Consumer is the pub/sub-triggered workflow executor.
type Consumer struct {
	cfg    ConsumerConfig
	server *http.Server
	// (The run-scoped single-flight mutex died with ADR-0007 phase 3:
	// graphs run in Job pods; the Dispatcher's createMu dedupes creates.)
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

	// No run-scoped single-flight: dispatch is milliseconds and graphs run
	// in Job pods (ADR-0007). The gate + capacity check + create-section
	// dedupe in the Dispatcher make redelivery idempotent; the Job's
	// activeDeadlineSeconds (OneShotRunBound) carries the wall-clock bound
	// that used to live here.
	runCtx := r.Context()

	if err := c.cfg.RunFunc(runCtx, RunRequest{
		Workflow:    trigger.Workflow,
		Namespace:   trigger.Namespace,
		Source:      trigger.Source,
		Attempt:     trigger.AttemptName,
		Traceparent: trigger.Traceparent,
		Pr:          trigger.Pr,
		Action:      trigger.Action,
		Revision:    trigger.Revision,
		PrTitle:     trigger.PrTitle,
	}); err != nil {
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
	Pr          string `json:"pr,omitempty"`
	PrTitle     string `json:"prTitle,omitempty"`
	Action      string `json:"action,omitempty"`
}

// RunConsumer is the entry point for consumer mode. Called from main's
// "consumer" subcommand. The pool pod is the DISPATCHER (ADR-0007 phase 3):
// each trigger is validated by the Review-Ready Gate in-process and accepted
// runs dispatch as an Attempt + Job (milliseconds); the graphs run in the
// Job pods, never here.
func RunConsumer(ctx context.Context) error {
	logger := slog.Default().With("component", "harmostes-consumer")

	dispatcher, err := DispatcherFromEnv(pluginConfigMapsFromEnv(), extraConfigMapMountsFromEnv(logger.Info), func(format string, args ...any) {
		logger.Info(fmt.Sprintf(format, args...))
	})
	if err != nil {
		return fmt.Errorf("dispatcher: %w", err)
	}

	consumer := NewConsumer(ConsumerConfig{
		HTTPPort:   envOr("HARMOSTES_CONSUMER_PORT", "8084"),
		PubsubName: envOr("HARMOSTES_PUBSUB_NAME", "pubsub"),
		Topic:      envOr("HARMOSTES_TRIGGER_TOPIC", "harmostes-triggers"),
		RunFunc:    dispatcher.Dispatch,
		Logger:     logger,
	})

	return consumer.Start(ctx)
}

// pluginConfigMapsFromEnv reads the comma-separated plugin ConfigMap list
// the chart renders (HARMOSTES_PLUGIN_CONFIGMAPS) — the same mounts the pool
// pod carries must reach the per-Attempt Jobs.

// extraConfigMapMountsFromEnv parses HARMOSTES_EXTRA_CONFIGMAP_MOUNTS —
// comma-separated ConfigMap=path[=mode] entries the chart renders for the
// named mounts the pool carries but per-Attempt Jobs must ALSO carry for
// pool-only plugins to run (#311). Mode is octal (0755 when omitted).
// Malformed entries are skipped with a log line.
func extraConfigMapMountsFromEnv(logf func(string, ...any)) []k8s.ConfigMapMount {
	raw := os.Getenv("HARMOSTES_EXTRA_CONFIGMAP_MOUNTS")
	if raw == "" {
		return nil
	}
	var out []k8s.ConfigMapMount
	for _, pair := range strings.Split(raw, ",") {
		parts := strings.Split(pair, "=")
		if len(parts) < 2 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
			logf("warn: HARMOSTES_EXTRA_CONFIGMAP_MOUNTS: skipping malformed entry %q", pair)
			continue
		}
		m := k8s.ConfigMapMount{Name: parts[0], MountPath: parts[1]}
		if len(parts) >= 3 {
			mode, err := strconv.ParseInt(parts[2], 8, 32)
			if err != nil {
				logf("warn: HARMOSTES_EXTRA_CONFIGMAP_MOUNTS: bad mode in %q: %v", pair, err)
				continue
			}
			mode32 := int32(mode)
			m.Mode = &mode32
		}
		out = append(out, m)
	}
	return out
}

func pluginConfigMapsFromEnv() []string {
	raw := os.Getenv("HARMOSTES_PLUGIN_CONFIGMAPS")
	if raw == "" {
		return nil
	}
	var cms []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			cms = append(cms, name)
		}
	}
	return cms
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
