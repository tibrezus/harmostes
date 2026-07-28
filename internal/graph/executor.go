// Package graph — graph executor: walks the pipeline graph, resolves node
// inputs, executes nodes via the registry, follows conditional edges (including
// loop-backs with maxRetries), checkpoints state to Dapr, and publishes
// lifecycle events.
package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/capability"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
)

// MaxIterations guards against infinite loops in cyclic graphs where maxRetries
// is 0 (unlimited). This is a safety valve, not a normal operating limit.
const MaxIterations = 1000

// DefaultStateStore is the Dapr state store component name for checkpoints.
const DefaultStateStore = "harmostes-state"

// DefaultPubSub is the Dapr pub/sub component name for lifecycle events.
const DefaultPubSub = "harmostes-pubsub"

// LifecycleTopic is the pub/sub topic for node lifecycle events.
const LifecycleTopic = "harmostes-events"

// DeadLetterTopic is the pub/sub topic for failed pipeline events. The UI
// subscribes to this topic to show a "failed pipelines" view with retry
// buttons.
const DeadLetterTopic = "harmostes-dead-letter"

// LifecycleEvent is the wire format for pipeline/node lifecycle events
// published to the Dapr pub/sub topic. The UI subscribes to these events to
// drive real-time canvas updates (G7).
type LifecycleEvent struct {
	Event      string      `json:"event"`                // pipeline.started, node.started, node.completed, node.failed, pipeline.completed, pipeline.failed
	Pipeline   string      `json:"pipeline"`             // pipeline CR name
	Node       string      `json:"node,omitempty"`       // node ID (empty for pipeline-level events)
	NodeType   string      `json:"nodeType,omitempty"`   // node type (agent, gate, plugin, etc.)
	Status     string      `json:"status,omitempty"`     // green | failed (empty for started events)
	Feedback   string      `json:"feedback,omitempty"`   // gate feedback or error message
	Outputs    NodeOutputs `json:"outputs,omitempty"`    // node outputs (agent metrics, deployment results)
	DurationMs int64       `json:"durationMs,omitempty"` // execution duration in milliseconds (completed/failed events)
	Timestamp  time.Time   `json:"timestamp"`            // event creation time (UTC)

	// Envelope carries the synthesized Node Result Envelope (ADR-0004) on
	// node.completed / node.failed events, giving the UI real-time visibility
	// into reference-backed claims, artifacts, and evidence. Nil on
	// pipeline-level and node.started events.
	Envelope *v1alpha1.NodeResultEnvelope `json:"envelope,omitempty"`

	// Provenance (G8): who/what triggered this pipeline run.
	TriggeredBy   string `json:"triggeredBy,omitempty"`   // username or "system"
	TriggerSource string `json:"triggerSource,omitempty"` // webhook | schedule | manual | controller
}

// DeadLetterEvent is published when a pipeline fails. It carries enough
// context for the UI to show a retry button: the pipeline name, the failing
// node, the error, and the trigger source (so the user knows whether to
// retry manually or wait for the next webhook).
type DeadLetterEvent struct {
	Pipeline      string                `json:"pipeline"`
	FailedNode    string                `json:"failedNode,omitempty"`
	Error         string                `json:"error"`
	NodeResults   map[string]NodeResult `json:"nodeResults,omitempty"`
	TriggeredBy   string                `json:"triggeredBy,omitempty"`
	TriggerSource string                `json:"triggerSource,omitempty"`
	Timestamp     time.Time             `json:"timestamp"`
}

// ExecutionResult is the outcome of a full graph execution.
type ExecutionResult struct {
	// Status is the pipeline-level outcome: green if no visited node failed.
	Status NodeStatus
	// NodeResults maps node ID → latest result (overwritten on re-execution).
	NodeResults map[string]NodeResult
	// NodeEnvelopes maps node ID → its synthesized Node Result Envelope
	// (ADR-0004). Populated for every finalized node (executed, denied, or
	// registry-error) so the caller (worker) can persist the canonical history
	// into an Attempt (ADR-0005, slice 3).
	NodeEnvelopes map[string]v1alpha1.NodeResultEnvelope
	// Message is a human-readable summary.
	Message string
}

// GraphExecutor walks a pipeline graph: resolves inputs → executes nodes →
// follows edges → checkpoints state → publishes events. It is the worker-side
// engine that turns a GraphSpec into an execution.
type GraphExecutor struct {
	registry   *Registry
	dapr       dapr.Client // optional: nil = no checkpointing/events
	stateStore string
	pubsub     string
	log        func(format string, args ...any)

	// Provenance (G8): stamped on all lifecycle events.
	triggeredBy   string
	triggerSource string

	// bindings is the Workflow's declared External System Bindings (ADR-0003),
	// used to enforce Capability Policy before node execution. Empty when no
	// Workflow bindings were provided (Pipeline CRs / legacy callers) — nodes
	// without Requires authorize trivially in that case.
	bindings []v1alpha1.ExternalSystemBinding

	// runID identifies the Workflow Run (Job) this execution belongs to. Stamped
	// on every synthesized Node Result Envelope so history (ADR-0005) can link
	// envelopes back to the run that produced them.
	runID string
}

// GraphExecutorOption configures a GraphExecutor.
type GraphExecutorOption func(*GraphExecutor)

// WithStateStore overrides the default state store component name.
func WithStateStore(name string) GraphExecutorOption {
	return func(e *GraphExecutor) { e.stateStore = name }
}

// WithPubSub overrides the default pub/sub component name.
func WithPubSub(name string) GraphExecutorOption {
	return func(e *GraphExecutor) { e.pubsub = name }
}

// WithLogger sets a structured logger for the executor.
func WithLogger(log func(format string, args ...any)) GraphExecutorOption {
	return func(e *GraphExecutor) { e.log = log }
}

// WithProvenance stamps the trigger source on all lifecycle events (G8).
// The worker reads these from env vars set by the controller.
func WithProvenance(triggeredBy, triggerSource string) GraphExecutorOption {
	return func(e *GraphExecutor) {
		e.triggeredBy = triggeredBy
		e.triggerSource = triggerSource
	}
}

// WithBindings sets the Workflow's declared External System Bindings (ADR-0003)
// so the kernel can enforce Capability Policy before executing each node. A
// node whose Requires are not satisfied by these bindings is refused and
// marked failed without being executed. Nodes without Requires authorize
// trivially, so omitting this option (or passing nil) preserves the current
// behavior for graphs whose nodes request nothing.
func WithBindings(bindings []v1alpha1.ExternalSystemBinding) GraphExecutorOption {
	return func(e *GraphExecutor) { e.bindings = bindings }
}

// WithRunID sets the Workflow Run (Job) identifier stamped on every Node Result
// Envelope (ADR-0004), so orchestration history (ADR-0005) can link envelopes
// to the run that produced them. Empty by default (envelopes are still
// recorded, just without run linkage).
func WithRunID(runID string) GraphExecutorOption {
	return func(e *GraphExecutor) { e.runID = runID }
}

// NewGraphExecutor creates a graph executor with the given registry and optional
// Dapr client. The Dapr client is used for state checkpointing and lifecycle
// event publishing. If nil, checkpointing/events are silently skipped (useful
// for testing).
func NewGraphExecutor(registry *Registry, client dapr.Client, opts ...GraphExecutorOption) *GraphExecutor {
	e := &GraphExecutor{
		registry:   registry,
		dapr:       client,
		stateStore: DefaultStateStore,
		pubsub:     DefaultPubSub,
		log:        func(string, ...any) {},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Execute walks the graph: resolve inputs → execute nodes → follow edges. The
// walk is a breadth-first traversal from entry nodes (nodes with no incoming
// edges). Conditional edges are evaluated after each node execution. Loop-back
// edges (back to a previously visited node) are limited by maxRetries.
//
// The whole run is one OTel trace: a root `graph.pipeline.run` span with a
// child span per node execution (the node executor creates its own span; this
// method creates a wrapper span for the graph walk).
func (e *GraphExecutor) Execute(ctx context.Context, graph v1alpha1.GraphSpec, pipelineName string) (ExecutionResult, error) {
	ctx, rootSpan := observability.Tracer().Start(ctx, "graph.pipeline.run",
		trace.WithAttributes(
			attribute.String("harmostes.pipeline", pipelineName),
			attribute.Int("harmostes.graph.nodes", len(graph.Nodes)),
			attribute.Int("harmostes.graph.edges", len(graph.Edges)),
		))
	defer rootSpan.End()

	result := ExecutionResult{
		Status:        StatusGreen,
		NodeResults:   make(map[string]NodeResult),
		NodeEnvelopes: make(map[string]v1alpha1.NodeResultEnvelope),
	}
	defer func() {
		rootSpan.SetAttributes(attribute.String("harmostes.pipeline.status", string(result.Status)))
		if result.Status == StatusFailed {
			rootSpan.SetStatus(codes.Error, result.Message)
		}
	}()

	// Build node lookup.
	nodeMap := make(map[string]v1alpha1.NodeSpec, len(graph.Nodes))
	for _, n := range graph.Nodes {
		nodeMap[n.ID] = n
	}

	// Build adjacency lists.
	outEdges := make(map[string][]v1alpha1.EdgeSpec)
	inDegree := make(map[string]int)
	for _, n := range graph.Nodes {
		inDegree[n.ID] = 0
	}
	for _, edge := range graph.Edges {
		outEdges[edge.From] = append(outEdges[edge.From], edge)
		// Edges with maxRetries > 0 are loop-backs (documented in the CRD).
		// They don't count towards inDegree: the target is an entry point that
		// is reached via a non-loop-back edge first, then re-reached via the
		// loop-back. Without this, a gate-feedback graph (agent→gate→agent)
		// would have no entry nodes.
		if edge.MaxRetries == 0 {
			inDegree[edge.To]++
		}
	}

	// Entry nodes: no incoming edges.
	var queue []string
	for _, n := range graph.Nodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	if len(queue) == 0 {
		result.Status = StatusFailed
		result.Message = "no entry nodes — graph is a pure cycle"
		return result, fmt.Errorf("graph has no entry nodes (all nodes have incoming edges)")
	}

	// Publish pipeline.started lifecycle event.
	e.publishLifecycle(ctx, LifecycleEvent{
		Event:    "pipeline.started",
		Pipeline: pipelineName,
	})

	edgeCount := make(map[string]int) // "from→to" → traversal count
	iterations := 0

	for len(queue) > 0 {
		if iterations >= MaxIterations {
			result.Status = StatusFailed
			result.Message = fmt.Sprintf("max iterations (%d) exceeded — possible infinite loop", MaxIterations)
			return result, fmt.Errorf("%s", result.Message)
		}
		iterations++

		nodeID := queue[0]
		queue = queue[1:]
		node, ok := nodeMap[nodeID]
		if !ok {
			result.Status = StatusFailed
			result.Message = fmt.Sprintf("edge references unknown node %q", nodeID)
			return result, fmt.Errorf("%s", result.Message)
		}

		// Resolve inputs: snapshot of all completed node outputs.
		env := NodeEnv{Inputs: snapshotOutputs(result.NodeResults)}

		// Capability Policy enforcement (ADR-0003, ADR-0001): the deterministic
		// kernel refuses to execute a node whose Surface Capability requirements
		// are not satisfied by the Workflow's declared bindings. The node is
		// marked failed without being executed, then routed through the normal
		// failure path (when:failed edges, else pipeline failure). Nodes without
		// Requires authorize trivially — backward compatible with existing graphs.
		if violations := capability.AuthorizeNode(e.bindings, node); len(violations) > 0 {
			feedback := capability.FormatViolations(violations)
			e.log("node %s: denied by capability policy — %s", nodeID, feedback)
			startTime := time.Now()
			denied := NodeResult{Status: StatusFailed, Feedback: feedback}
			result.NodeResults[nodeID] = denied
			deniedEnv := e.synthesizeEnvelope(nodeID, node.Type, denied)
			result.NodeEnvelopes[nodeID] = deniedEnv
			e.checkpoint(ctx, pipelineName, nodeID, denied)
			e.publishLifecycle(ctx, LifecycleEvent{
				Event:      "node.failed",
				Pipeline:   pipelineName,
				Node:       nodeID,
				NodeType:   node.Type,
				Status:     string(StatusFailed),
				Feedback:   feedback,
				DurationMs: time.Since(startTime).Milliseconds(),
				Envelope:   &deniedEnv,
			})
			// Reuse the standard failure routing: follow when:failed edges; if
			// none handle it, the pipeline fails.
			handled := false
			for _, edge := range outEdges[nodeID] {
				if e.shouldTraverse(edge, denied, result.NodeResults) {
					if e.enqueueEdge(&queue, edge, edgeCount) {
						handled = true
					}
				}
			}
			if !handled {
				result.Status = StatusFailed
				result.Message = fmt.Sprintf("node %s denied by capability policy: %s", nodeID, feedback)
				e.publishLifecycle(ctx, LifecycleEvent{
					Event:    "pipeline.failed",
					Pipeline: pipelineName,
					Status:   string(StatusFailed),
					Feedback: result.Message,
				})
				e.publishDeadLetter(ctx, pipelineName, nodeID, result.Message, result.NodeResults)
				return result, nil
			}
			continue
		}

		e.log("node %s: type=%s executing", nodeID, node.Type)
		startTime := time.Now()
		e.publishLifecycle(ctx, LifecycleEvent{
			Event:    "node.started",
			Pipeline: pipelineName,
			Node:     nodeID,
			NodeType: node.Type,
		})

		// Execute via registry.
		exec, err := e.registry.Get(node.Type)
		if err != nil {
			result.Status = StatusFailed
			result.Message = fmt.Sprintf("node %s: %v", nodeID, err)
			errResult := NodeResult{Status: StatusFailed, Feedback: err.Error()}
			result.NodeResults[nodeID] = errResult
			result.NodeEnvelopes[nodeID] = e.synthesizeEnvelope(nodeID, node.Type, errResult)
			errEnv := result.NodeEnvelopes[nodeID]
			e.publishLifecycle(ctx, LifecycleEvent{
				Event:      "node.failed",
				Pipeline:   pipelineName,
				Node:       nodeID,
				NodeType:   node.Type,
				Status:     string(StatusFailed),
				Feedback:   err.Error(),
				DurationMs: time.Since(startTime).Milliseconds(),
				Envelope:   &errEnv,
			})
			break
		}

		// Per-node timeout (G8 circuit breaker): if the node has a timeout,
		// wrap its execution in a context with deadline. On timeout, the node
		// is marked failed with "timed out after {duration}".
		execCtx := ctx
		var timeoutStr string
		if node.Timeout != "" {
			if d, err := time.ParseDuration(node.Timeout); err == nil && d > 0 {
				var cancel context.CancelFunc
				execCtx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
				timeoutStr = node.Timeout
			} else {
				e.log("warn: node %s: invalid timeout %q, ignoring", nodeID, node.Timeout)
			}
		}

		nodeResult, execErr := exec.Execute(execCtx, node, env)
		durationMs := time.Since(startTime).Milliseconds()
		if execErr != nil {
			nodeResult.Status = StatusFailed
			if nodeResult.Feedback == "" {
				// Check if the error was a timeout (G8 circuit breaker).
				if timeoutStr != "" && (execErr == context.DeadlineExceeded || strings.Contains(execErr.Error(), "deadline exceeded")) {
					nodeResult.Feedback = fmt.Sprintf("timed out after %s", timeoutStr)
				} else {
					nodeResult.Feedback = execErr.Error()
				}
			}
		}
		if nodeResult.Status == "" {
			nodeResult.Status = StatusGreen
		}

		result.NodeResults[nodeID] = nodeResult
		completedEnv := e.synthesizeEnvelope(nodeID, node.Type, nodeResult)
		result.NodeEnvelopes[nodeID] = completedEnv
		e.checkpoint(ctx, pipelineName, nodeID, nodeResult)
		completedEvent := LifecycleEvent{
			Event:      "node.completed",
			Pipeline:   pipelineName,
			Node:       nodeID,
			NodeType:   node.Type,
			Status:     string(nodeResult.Status),
			Feedback:   nodeResult.Feedback,
			DurationMs: durationMs,
			Envelope:   &completedEnv,
		}
		if nodeResult.Status == StatusFailed {
			completedEvent.Event = "node.failed"
		}
		e.publishLifecycle(ctx, completedEvent)
		e.log("node %s: status=%s duration=%dms", nodeID, nodeResult.Status, durationMs)

		if nodeResult.Status == StatusFailed {
			// Node failed: check if any outgoing edge handles failure (when: failed).
			// If no edge handles it, the pipeline fails.
			handled := false
			for _, edge := range outEdges[nodeID] {
				if e.shouldTraverse(edge, nodeResult, result.NodeResults) {
					if e.enqueueEdge(&queue, edge, edgeCount) {
						handled = true
					}
				}
			}
			if !handled {
				result.Status = StatusFailed
				result.Message = fmt.Sprintf("node %s failed: %s", nodeID, nodeResult.Feedback)
				e.publishLifecycle(ctx, LifecycleEvent{
					Event:    "pipeline.failed",
					Pipeline: pipelineName,
					Status:   string(StatusFailed),
					Feedback: result.Message,
				})
				// Dead-letter (G8): publish failure context for the retry UI.
				e.publishDeadLetter(ctx, pipelineName, nodeID, result.Message, result.NodeResults)
				return result, nil
			}
			continue
		}

		// Node succeeded: traverse outgoing edges.
		for _, edge := range outEdges[nodeID] {
			if e.shouldTraverse(edge, nodeResult, result.NodeResults) {
				e.enqueueEdge(&queue, edge, edgeCount)
			}
		}
	}

	// Pipeline succeeded: all reachable nodes completed.
	result.Message = "pipeline completed"
	e.publishLifecycle(ctx, LifecycleEvent{
		Event:    "pipeline.completed",
		Pipeline: pipelineName,
		Status:   string(StatusGreen),
	})
	return result, nil
}

// enqueueEdge adds the edge's target to the queue, enforcing maxRetries. Returns
// false (and sets the pipeline to failed) if maxRetries is exceeded.
func (e *GraphExecutor) enqueueEdge(queue *[]string, edge v1alpha1.EdgeSpec, edgeCount map[string]int) bool {
	key := edge.From + "→" + edge.To
	edgeCount[key]++

	if edge.MaxRetries > 0 && edgeCount[key] > edge.MaxRetries {
		// maxRetries exceeded — caller handles failure.
		return false
	}
	*queue = append(*queue, edge.To)
	return true
}

// shouldTraverse evaluates an edge condition against the source node's result.
// Conditions: empty = always, green/failed = node status, changed/unchanged =
// branch output, or a Go text/template expression.
func (e *GraphExecutor) shouldTraverse(edge v1alpha1.EdgeSpec, sourceResult NodeResult, allResults map[string]NodeResult) bool {
	switch edge.When {
	case "", "always":
		return true
	case "green":
		return sourceResult.Status == StatusGreen
	case "failed":
		return sourceResult.Status == StatusFailed
	case "changed":
		return getBoolOutput(sourceResult.Outputs, "changed")
	case "unchanged":
		return !getBoolOutput(sourceResult.Outputs, "changed")
	default:
		// Template expression — evaluate against all node outputs.
		return evaluateCondition(edge.When, snapshotOutputs(allResults))
	}
}

// checkpoint saves the node result to the Dapr state store for resume/audit.
// Key format: pipeline/<pipelineName>/nodes/<nodeID>. Best-effort: errors are
// logged but do not fail the pipeline.
func (e *GraphExecutor) checkpoint(ctx context.Context, pipelineName, nodeID string, result NodeResult) {
	if e.dapr == nil {
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		e.log("warn: checkpoint marshal %s: %v", nodeID, err)
		return
	}
	key := fmt.Sprintf("pipeline/%s/nodes/%s", pipelineName, nodeID)
	if err := e.dapr.SaveState(ctx, e.stateStore, key, string(data)); err != nil {
		e.log("warn: checkpoint %s: %v", nodeID, err)
	}
}

// publishLifecycle publishes a lifecycle event to the Dapr pub/sub topic.
// Best-effort: errors are logged but do not fail the pipeline.
func (e *GraphExecutor) publishLifecycle(ctx context.Context, ev LifecycleEvent) {
	if e.dapr == nil {
		return
	}
	ev.Timestamp = time.Now().UTC()
	ev.TriggeredBy = e.triggeredBy
	ev.TriggerSource = e.triggerSource
	b, err := json.Marshal(ev)
	if err != nil {
		e.log("warn: publish %s: marshal: %v", ev.Event, err)
		return
	}
	if err := e.dapr.Publish(ctx, e.pubsub, LifecycleTopic, string(b)); err != nil {
		e.log("warn: publish %s: %v", ev.Event, err)
	}
}

// publishDeadLetter publishes a dead-letter event when a pipeline fails.
// The UI subscribes to this topic to show a "failed pipelines" view with
// retry buttons. Best-effort: errors are logged but not fatal.
func (e *GraphExecutor) publishDeadLetter(ctx context.Context, pipelineName, failedNode, errMsg string, nodeResults map[string]NodeResult) {
	if e.dapr == nil {
		return
	}
	ev := DeadLetterEvent{
		Pipeline:      pipelineName,
		FailedNode:    failedNode,
		Error:         errMsg,
		NodeResults:   nodeResults,
		TriggeredBy:   e.triggeredBy,
		TriggerSource: e.triggerSource,
		Timestamp:     time.Now().UTC(),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		e.log("warn: dead-letter marshal: %v", err)
		return
	}
	if err := e.dapr.Publish(ctx, e.pubsub, DeadLetterTopic, string(b)); err != nil {
		e.log("warn: dead-letter publish: %v", err)
	}
}

// snapshotOutputs builds a map of nodeID → outputs from the latest results.
func snapshotOutputs(results map[string]NodeResult) map[string]NodeOutputs {
	out := make(map[string]NodeOutputs, len(results))
	for id, r := range results {
		out[id] = r.Outputs
	}
	return out
}

// getBoolOutput reads a boolean output from a node's outputs map, handling both
// bool and string ("true"/"false") representations.
func getBoolOutput(outputs NodeOutputs, key string) bool {
	switch v := outputs[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

// envelopeStatus maps the internal NodeStatus to the canonical Node Result
// status string (ADR-0004). The kernel owns the final outcome — an exec error,
// timeout, or capability denial adjusts NodeResult.Status before this runs — so
// the envelope's status always reflects the true result.
func envelopeStatus(s NodeStatus) string {
	switch s {
	case StatusGreen:
		return v1alpha1.NodeResultStatusOK
	case StatusSkipped:
		return v1alpha1.NodeResultStatusSkipped
	default:
		return v1alpha1.NodeResultStatusFailed
	}
}

// synthesizeEnvelope builds the universal Node Result Envelope (ADR-0004) for a
// finalized node. Kernel-authoritative fields (NodeID, RunID, Status,
// Provenance, ProducedAt) are always stamped; executor-provided enrichment
// (Summary, Claims, Artifacts, References, Payload) is merged on top when the
// executor returned a non-nil NodeResult.Envelope. When the executor provided
// no Summary, Feedback is used so every envelope carries a human note.
//
// This is called for every finalized node — executed, capability-denied, or
// registry-error — so the caller always receives a complete, uniform record per
// node for orchestration history (ADR-0005).
func (e *GraphExecutor) synthesizeEnvelope(nodeID, nodeType string, nr NodeResult) v1alpha1.NodeResultEnvelope {
	now := metav1.Now()
	env := v1alpha1.NodeResultEnvelope{
		NodeID: nodeID,
		RunID:  e.runID,
		Status: envelopeStatus(nr.Status),
		Provenance: v1alpha1.Provenance{
			TriggeredBy:   e.triggeredBy,
			TriggerSource: e.triggerSource,
			ProducedAt:    now,
		},
		ProducedAt: now,
	}
	if nr.Envelope != nil {
		// Executor-provided enrichment wins for these fields.
		env.Summary = nr.Envelope.Summary
		env.Artifacts = nr.Envelope.Artifacts
		env.Claims = nr.Envelope.Claims
		env.Payload = nr.Envelope.Payload
		env.References = nr.Envelope.References
	}
	if env.Summary == "" {
		env.Summary = nr.Feedback
	}
	return env
}

// ExecuteGraph is a convenience function: create a default-registry executor and
// run the graph in one call. Used by the worker for Pipeline CRs.
func ExecuteGraph(ctx context.Context, graph v1alpha1.GraphSpec, pipelineName string, deps Dependencies, opts ...GraphExecutorOption) (ExecutionResult, error) {
	registry := NewDefaultRegistry(deps)
	exec := NewGraphExecutor(registry, deps.DaprClient, opts...)
	return exec.Execute(ctx, graph, pipelineName)
}
