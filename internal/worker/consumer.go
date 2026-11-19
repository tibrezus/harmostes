package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

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
	Namespace  string // the namespace the workflows live in (fast-poll runs)

	// FastPoll is the BASE cadence of the kernel's reconciliation floor
	// (owner requirement, #556): every pass, ArmedWaitingWorkflows names
	// the review-ready workflows holding an ARMED, NEVER-DISPATCHED claim,
	// and each gets a sweep through RunFunc. The INSTANT dispatch path is
	// the CI wake (host-native ci_completed events → a run cycle within
	// seconds of the last check landing); this loop is the at-most-once
	// webhook recovery floor, so its cadence backs off exponentially
	// (base ×2 per stalled pass, 30m cap) and resets to base whenever the
	// armed set shrinks or grows. It fires ONLY while armed claims exist
	// and publishes no trigger events. 0 = off.
	FastPoll time.Duration
	// ArmedWaitingWorkflows returns the review-ready workflow NAMES with
	// armed-not-dispatched claims. Nil disables the fast poll.
	ArmedWaitingWorkflows func(ctx context.Context) []string
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
	// Repo carries the host-native CI wake's repository (#556): CI payloads
	// have (repo, sha) but no PR number, so the gate re-derives the PR from
	// its armed claims. Empty on PR-shaped wakes (the pointer carries it).
	Repo string
}

// Consumer is the pub/sub-triggered workflow executor.
type Consumer struct {
	cfg    ConsumerConfig
	server *http.Server
	// muxOpts mount extra routes (the PRLineage actor host) on the
	// consumer's ServeMux — one process, one app-port.
	muxOpts []func(*http.ServeMux)
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
	// Extra routes (the PRLineage actor host mounts /actors/ + /dapr/config
	// on this mux — one process, one app-port; r21 P4.3).
	for _, opt := range c.muxOpts {
		opt(mux)
	}
	mux.HandleFunc("/dapr/subscribe", c.handleSubscribe)
	mux.HandleFunc("/triggers", c.handleTrigger)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c.server = &http.Server{
		Addr:    ":" + c.cfg.HTTPPort,
		Handler: mux,
	}

	if c.cfg.FastPoll > 0 && c.cfg.ArmedWaitingWorkflows != nil {
		go c.armedBackoffLoop(ctx)
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

// armedBackoffLoop is the kernel's reconciliation floor (#556): while any
// review-ready workflow holds an armed-not-dispatched claim, re-sweep at an
// exponentially backing-off cadence — FastPoll at base, ×2 per stalled
// pass (the armed set unchanged), capped at 30m; any change in the armed
// set (a dispatch landed, or a fresh arm arrived) resets to base, because
// either event is exactly when the next few seconds matter. The sweep is
// idempotent and capacity-safe (the dispatcher's createMu + the gate's own
// dedupe), so overlap degrades to a no-op; a busy flag keeps one pass per
// tick even when a pass overruns. An idle fleet runs ZERO passes: the loop
// publishes nothing and touches nothing when ArmedWaitingWorkflows is
// empty (the CI wake owns latency; this loop only bounds webhook loss).
func (c *Consumer) armedBackoffLoop(ctx context.Context) {
	const backoffCap = 30 * time.Minute
	delay := c.cfg.FastPoll
	lastLen := -1
	var busy atomic.Bool
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !busy.CompareAndSwap(false, true) {
			// Overrun: retry shortly without treating it as a stalled pass.
			timer.Reset(time.Second)
			continue
		}
		list := c.cfg.ArmedWaitingWorkflows(ctx)
		// The reconciliation floor is a pass, not a delivery: without a root
		// span its sweeps are invisible in the trace (#928). Every sweep's
		// dispatch nests under it, so a stalled armed fleet reads as one
		// trace per pass instead of orphaned runs.
		sweepCtx, sweepSpan := observability.Tracer().Start(ctx, "worker.fast-poll.sweep",
			trace.WithAttributes(attribute.Int("harmostes.armed_workflows", len(list))))
		for _, wf := range list {
			runCtx, cancel := context.WithTimeout(sweepCtx, 2*time.Minute)
			runCtx, span := dispatchSpan(runCtx, wf, "fast-poll", "fast-poll")
			if err := c.cfg.RunFunc(runCtx, RunRequest{
				Workflow:    wf,
				Namespace:   c.cfg.Namespace,
				Source:      "fast-poll",
				Traceparent: dispatchTraceparent(runCtx, ""),
			}); err != nil {
				span.SetStatus(codes.Error, err.Error())
				c.cfg.Logger.Info("backoff sweep failed (retried next pass)", "workflow", wf, "error", err)
			}
			span.End()
			cancel()
		}
		sweepSpan.End()
		busy.Store(false)
		// Cadence: any movement in the armed set resets to base — a shrink
		// means a dispatch just landed (the next one should not wait out a
		// backoff), a growth means a fresh arm (same). Only a stalled set
		// (same size, sweeps converging to no-ops) earns the doubling.
		switch {
		case lastLen < 0 || len(list) != lastLen:
			delay = c.cfg.FastPoll
		case delay < backoffCap:
			delay = min(delay*2, backoffCap)
		}
		lastLen = len(list)
		timer.Reset(delay)
	}
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
//
// The trace hop (#928): daprd delivers the message carrying the W3C
// traceparent it propagated from the publisher, so the dispatch decision —
// the gate sweep, capacity check, Attempt mint, Job create — gets a span of
// its own instead of vanishing into the delivery. The span's traceparent is
// handed to the dispatched run, making the chain
// controller.reconcile → daprd.publish → daprd.delivery → worker.trigger.dispatch
// → graph.pipeline.run one trace.
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
	// activeDeadlineSeconds carries the wall-clock bound (reviewReady.runBound,
	// #348/#333, default OneShotRunBound)
	// that used to live here.
	//
	// Parent selection, in authority order:
	//   1. the DELIVERY header — daprd's own propagated context for this
	//      message (the publisher's span as daprd saw it, plus daprd's own
	//      delivery span in the chain);
	//   2. the payload stamp — the controller's reconcile span, published in
	//      the event body for exactly this handoff. Used when the delivery
	//      carries no header (published outside Dapr, or an older sidecar),
	//      so the link the controller established is not lost;
	//   3. neither — the dispatch is its own root. Nothing is fabricated.
	parent := observability.ContextWithTraceparent(r.Context(), r.Header.Get("traceparent"))
	// The propagator ignores malformed values, so validity is checked by the
	// SDK rather than re-parsed here: a garbage header must not outrank a
	// valid payload stamp.
	if !trace.SpanContextFromContext(parent).IsValid() {
		parent = observability.ContextWithTraceparent(r.Context(), trigger.Traceparent)
	}
	runCtx, span := dispatchSpan(parent, trigger.Workflow, trigger.TriggerType, trigger.Source)
	defer span.End()

	if err := c.cfg.RunFunc(runCtx, RunRequest{
		Workflow:  trigger.Workflow,
		Namespace: trigger.Namespace,
		Source:    trigger.Source,
		Attempt:   trigger.AttemptName,
		// The DISPATCH span's traceparent, not the payload's: the run nests
		// under the hop that decided it. TraceparentFromContext is "" when
		// telemetry is disabled, in which case the delivery/payload context is
		// passed through unchanged rather than dropped (#928).
		Traceparent: dispatchTraceparent(runCtx, trigger.Traceparent),
		Pr:          trigger.Pr,
		Action:      trigger.Action,
		Revision:    trigger.Revision,
		PrTitle:     trigger.PrTitle,
		Repo:        trigger.Repo,
	}); err != nil {
		span.SetStatus(codes.Error, err.Error())
		c.cfg.Logger.Error("workflow run failed", "workflow", trigger.Workflow, "error", err)
		http.Error(w, fmt.Sprintf("run failed: %v", err), http.StatusInternalServerError)
		return
	}

	c.cfg.Logger.Info("trigger processed", "workflow", trigger.Workflow, "trigger_type", trigger.TriggerType)
	w.WriteHeader(http.StatusOK)
}

// dispatchSpan opens the hop's span: one construction for both wake paths
// (delivered event and fast-poll sweep), so the span name and its identity
// attributes cannot drift between them.
func dispatchSpan(ctx context.Context, workflow, triggerType, source string) (context.Context, trace.Span) {
	return observability.Tracer().Start(ctx, "worker.trigger.dispatch",
		trace.WithAttributes(
			attribute.String("harmostes.workflow", workflow),
			attribute.String("harmostes.trigger.type", triggerType),
			attribute.String("harmostes.trigger.source", source),
		))
}

// dispatchTraceparent is the traceparent handed to a dispatched run. The
// dispatch span's own context wins — that nesting is the point of #928. When
// this pod's telemetry is disabled the span records nothing, so the context the
// delivery carried is passed through unchanged rather than dropped: a run must
// not lose a link the controller already established just because the middle
// pod is un-instrumented. An empty result means there was no context to inherit
// — nothing is invented.
func dispatchTraceparent(ctx context.Context, inherited string) string {
	if tp := observability.TraceparentFromContext(ctx); tp != "" {
		return tp
	}
	return inherited
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
	Repo        string `json:"repo,omitempty"`
}

// RunConsumer is the entry point for consumer mode. Called from main's
// "consumer" subcommand. The pool pod is the DISPATCHER (ADR-0007 phase 3):
// each trigger is validated by the Review-Ready Gate in-process and accepted
// runs dispatch as an Attempt + Job (milliseconds); the graphs run in the
// Job pods, never here.
func RunConsumer(ctx context.Context, muxOpts ...func(*http.ServeMux)) error {
	logger := slog.Default().With("component", "harmostes-consumer")

	dispatcher, err := DispatcherFromEnv(func(format string, args ...any) {
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
		Namespace:  dispatcher.Namespace(),
		FastPoll:   fastPollFromEnv(logger),
		ArmedWaitingWorkflows: func(ctx context.Context) []string {
			return dispatcher.ArmedWaitingWorkflows(ctx)
		},
	})
	consumer.muxOpts = muxOpts

	return consumer.Start(ctx)
}

// The chart-env parsers (pluginConfigMapsFromEnv, extraConfigMapMountsFromEnv)
// live in dispatch.go beside the DispatchConfig they feed — one module owns
// env→config, so a parsed fact cannot be dropped between a parser and a
// struct field (the #314 class).

// fastPollFromEnv reads HARMOSTES_GATE_FASTPOLL (Go duration; the chart
// default is 30s). "0"/unparsable = off — the cron-only backstop.
func fastPollFromEnv(logger *slog.Logger) time.Duration {
	raw := os.Getenv("HARMOSTES_GATE_FASTPOLL")
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		logger.Warn("HARMOSTES_GATE_FASTPOLL unparsable — fast poll disabled", "value", raw)
		return 0
	}
	return d
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
