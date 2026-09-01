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
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/capability"
	"github.com/tibrezus/harmostes/internal/claim"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/observability"
	"github.com/tibrezus/harmostes/internal/timeline"
	"github.com/tibrezus/harmostes/internal/worker"
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
	Event    string `json:"event"`    // pipeline.started, node.started, node.completed, node.failed, pipeline.completed, pipeline.failed
	Pipeline string `json:"pipeline"` // pipeline CR name

	// Attempt is the Attempt CR name this execution belongs to (ADR-0007).
	// Stamped by the worker (which knows it from HARMOSTES_ATTEMPT) so the UI
	// can scope per-attempt subscriptions without guessing from envelopes —
	// node.started events carry no envelope, but always carry this. Empty on
	// events from pre-attribution workers (UI wakes conservatively on it).
	Attempt string `json:"attempt,omitempty"`

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

	// timeline appends node-boundary evidence per run (optional, nil-safe).
	timeline timeline.Writer

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

	// attemptName is the Attempt CR name this execution belongs to (ADR-0007).
	// Stamped on every lifecycle event so the UI can attribute events to the
	// attempt without envelopes (node.started has none).
	attemptName string

	// onNodeResult is invoked with each node's envelope as the node completes
	// (after claim enforcement, so it matches what the outcome records).
	// Optional: nil on Pipeline CRs and tests without recording.
	onNodeResult func(context.Context, v1alpha1.NodeResultEnvelope)

	// wfCtx carries the Workflow context (source URL, namespace, workdir, etc.)
	// into every node's NodeEnv. Without this, graph-native plugin nodes don't
	// receive HARMOSTES_SOURCE_URL and other env vars that the declarative
	// worker injects automatically. Set via WithWorkflowContext.
	wfCtx WorkflowContext
}

// WorkflowContext carries Workflow-level context into graph node execution.
// It populates the NodeEnv fields that graph-native plugin/agent nodes need to
// resolve HARMOSTES_SOURCE_URL, HARMOSTES_WORKDIR, etc. — the same env vars
// that the declarative worker.Run() injects automatically.
type WorkflowContext struct {
	Name           string   // workflow / pipeline name
	Namespace      string   // k8s namespace
	Workdir        string   // shared working directory
	Source         string   // resolved source ref/revision
	SourceURL      string   // upstream source repo URL
	SourceBranch   string   // upstream source branch
	SourceLanguage string   // source language hint (go, zig, …) for prepare plugins
	WorkspaceDir   string   // fetched workspace repo path (== Workdir when a workspaceRepo is set)
	Shadow         string   // push target branch (parallel/dry-run)
	State          string   // Dapr state key prefix for this workflow
	ExtraEnv       []string // extra env vars propagated to all plugin nodes
}

// GraphExecutorOption configures a GraphExecutor.
type GraphExecutorOption func(*GraphExecutor)

// WithTimeline injects the evidence-layer writer for node boundary events.
func WithTimeline(w timeline.Writer) GraphExecutorOption {
	return func(e *GraphExecutor) { e.timeline = w }
}

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

// WithAttemptName sets the Attempt CR name (ADR-0007) stamped on every
// lifecycle event, letting the UI scope per-attempt event subscriptions.
// The worker reads it from HARMOSTES_ATTEMPT. Empty by default (events are
// still published, just unattributed — the UI treats those conservatively).
func WithAttemptName(name string) GraphExecutorOption {
	return func(e *GraphExecutor) { e.attemptName = name }
}

// WithOnNodeResult sets a callback invoked with each node's envelope as the
// node completes (post claim-enforcement — identical to what the run outcome
// records). The worker uses it to land envelopes on the Attempt CR
// incrementally; nil by default (Pipeline CRs, tests).
func WithOnNodeResult(fn func(context.Context, v1alpha1.NodeResultEnvelope)) GraphExecutorOption {
	return func(e *GraphExecutor) { e.onNodeResult = fn }
}

// WithWorkflowContext carries the Workflow-level context (source URL,
// namespace, workdir, etc.) into every node's NodeEnv. Graph-native plugin
// nodes need this to resolve HARMOSTES_SOURCE_URL and related env vars that
// the declarative worker.Run() injects automatically.
func WithWorkflowContext(ctx WorkflowContext) GraphExecutorOption {
	return func(e *GraphExecutor) { e.wfCtx = ctx }
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
		// Edges FROM an external node are display-only (external nodes never
		// execute, so these edges never traverse). They must not suppress the
		// target's entry-node status — otherwise a real node whose only incoming
		// edge comes from an external node (e.g. mirror→merge) would never run.
		if edge.MaxRetries == 0 && !isExternalNode(nodeMap, edge.From) {
			inDegree[edge.To]++
		}
	}

	// Entry nodes: no incoming edges. External nodes are never entries — they
	// are display-only topology (the map renders them; the executor ignores them).
	var queue []string
	for _, n := range graph.Nodes {
		if inDegree[n.ID] == 0 && n.Type != NodeTypeExternal {
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
		env := NodeEnv{
			Inputs:         snapshotOutputs(result.NodeResults),
			Workflow:       e.wfCtx.Name,
			RunID:          e.runID,
			Namespace:      e.wfCtx.Namespace,
			Workdir:        e.wfCtx.Workdir,
			Source:         e.wfCtx.Source,
			SourceURL:      e.wfCtx.SourceURL,
			SourceBranch:   e.wfCtx.SourceBranch,
			SourceLanguage: e.wfCtx.SourceLanguage,
			WorkspaceDir:   e.wfCtx.WorkspaceDir,
			Shadow:         e.wfCtx.Shadow,
			State:          e.wfCtx.State,
			ExtraEnv:       e.wfCtx.ExtraEnv,
		}

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
			deniedEnv := e.synthesizeEnvelope(nodeID, node.Type, denied, time.Since(startTime).Milliseconds())
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
					if e.enqueueEdge(&queue, edge, edgeCount, nodeMap) {
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
			result.NodeEnvelopes[nodeID] = e.synthesizeEnvelope(nodeID, node.Type, errResult, time.Since(startTime).Milliseconds())
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

		if e.timeline != nil {
			if err := e.timeline.Emit(ctx, timeline.KindNodeStarted, nodeID, map[string]any{
				"type": node.Type,
			}); err != nil {
				e.log("warn: timeline emit node.started %s: %v", nodeID, err)
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
		if e.timeline != nil {
			payload := map[string]any{
				"type":       node.Type,
				"status":     string(nodeResult.Status),
				"durationMs": durationMs,
			}
			if fb := nodeResult.Feedback; fb != "" {
				// Same #115-class redaction as the plugin tail: feedback is
				// plugin combined output bound for a durable sink.
				payload["feedback"] = truncateForTimeline(worker.Redact(fb), 200)
			}
			if err := e.timeline.Emit(ctx, timeline.KindNodeCompleted, nodeID, payload); err != nil {
				e.log("warn: timeline emit node.completed %s: %v", nodeID, err)
			}
		}
		completedEnv := e.synthesizeEnvelope(nodeID, node.Type, nodeResult, durationMs)
		// Trust enforcement (ADR-0004): a non-deterministic node cannot
		// self-validate its own claims — demote any self-asserted validated
		// claims to observed before the envelope is recorded.
		if demoted := claim.Enforce(exec.Deterministic(), &completedEnv); demoted > 0 {
			e.log("node %s: demoted %d self-validated claim(s) (non-deterministic node)", nodeID, demoted)
		}
		result.NodeEnvelopes[nodeID] = completedEnv
		// Incremental recording: hand the envelope to the owner (the worker
		// persists it to the Attempt CR) the moment the node completes, so the
		// UI's live position advances node-by-node instead of everything
		// landing in a batch at outcome. Nil-safe (Pipeline CRs / tests).
		if e.onNodeResult != nil {
			e.onNodeResult(ctx, completedEnv)
		}
		e.checkpoint(ctx, pipelineName, nodeID, nodeResult)
		completedEvent := LifecycleEvent{
			Event:      "node.completed",
			Pipeline:   pipelineName,
			Node:       nodeID,
			NodeType:   node.Type,
			Status:     string(nodeResult.Status),
			Feedback:   nodeResult.Feedback,
			Outputs:    nodeResult.Outputs,
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
					if e.enqueueEdge(&queue, edge, edgeCount, nodeMap) {
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

		// Node succeeded: a deterministic node (gate) that declares a validation
		// scope promotes matching observed claims from the run to validated
		// (ADR-0004 promotion). No-op when the node declares no scope.
		if nodeResult.Status == StatusGreen && exec.Deterministic() {
			e.applyPromotions(ctx, pipelineName, nodeID, node, &result)
		}

		// Node succeeded: traverse outgoing edges.
		for _, edge := range outEdges[nodeID] {
			if e.shouldTraverse(edge, nodeResult, result.NodeResults) {
				e.enqueueEdge(&queue, edge, edgeCount, nodeMap)
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
//
// Edges targeting an EXTERNAL node are display-only: the target is never
// enqueued, but the traversal counts as handled — a failed node routed to an
// external system (e.g. merge conflict → async conflict-resolver) is a
// legitimate terminal state, delegated out-of-band, not a pipeline failure.
func (e *GraphExecutor) enqueueEdge(queue *[]string, edge v1alpha1.EdgeSpec, edgeCount map[string]int, nodeMap map[string]v1alpha1.NodeSpec) bool {
	if isExternalNode(nodeMap, edge.To) {
		e.log("edge %s→%s: external target — display-only, not executed", edge.From, edge.To)
		return true
	}
	key := edge.From + "→" + edge.To
	edgeCount[key]++

	if edge.MaxRetries > 0 && edgeCount[key] > edge.MaxRetries {
		// maxRetries exceeded — caller handles failure.
		return false
	}
	*queue = append(*queue, edge.To)
	return true
}

// NodeTypeExternal is the display-only node type for out-of-band systems.
// External nodes never execute; the map renders them as conceptual topology.
const NodeTypeExternal = "external"

// isExternalNode reports whether the node referenced by id is an external
// (display-only) node. Unknown ids are not external.
func isExternalNode(nodeMap map[string]v1alpha1.NodeSpec, id string) bool {
	n, ok := nodeMap[id]
	return ok && n.Type == NodeTypeExternal
}

// shouldTraverse evaluates an edge condition against the source node's result.
// Conditions: empty = always, green/failed = node status, changed/unchanged =
// branch output, or a Go text/template expression.
func (e *GraphExecutor) shouldTraverse(edge v1alpha1.EdgeSpec, sourceResult NodeResult, allResults map[string]NodeResult) bool {
	switch edge.When {
	case "always":
		return true
	case "", "green":
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
	ev.Attempt = e.attemptName
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
func (e *GraphExecutor) synthesizeEnvelope(nodeID, nodeType string, nr NodeResult, durationMs int64) v1alpha1.NodeResultEnvelope {
	now := metav1.Now()
	env := v1alpha1.NodeResultEnvelope{
		NodeID:     nodeID,
		RunID:      e.runID,
		Status:     envelopeStatus(nr.Status),
		DurationMs: durationMs,
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

// applyPromotions runs ADR-0004 claim promotion for a deterministic node that
// just succeeded. The node's ValidationScope(s) (parsed from its config)
// declare which claim types/bindings it deterministically confirmed; matching
// observed claims across the run's accumulated envelopes are promoted to
// validated, stamped with ValidatedBy. Promoted envelopes are re-published as
// lifecycle events so the UI sees the trust-class change in real time. No-op
// when the node declares no scope (backward compatible).
func (e *GraphExecutor) applyPromotions(ctx context.Context, pipelineName, nodeID string, node v1alpha1.NodeSpec, result *ExecutionResult) {
	scopes := nodeValidationScopes(node)
	if len(scopes) == 0 {
		return
	}
	for _, scope := range scopes {
		promotions := claim.Promote(result.NodeEnvelopes, nodeID, scope)
		for _, p := range promotions {
			e.log("node %s: promoted claim %s.%s (%s) → validated by %s",
				nodeID, p.FromNodeID, p.Claim.Type, p.Claim.Binding, nodeID)
			// Re-publish the promoted envelope so the UI reflects the new trust class.
			if env, ok := result.NodeEnvelopes[p.FromNodeID]; ok {
				e.publishLifecycle(ctx, LifecycleEvent{
					Event:    "claim.validated",
					Pipeline: pipelineName,
					Node:     p.FromNodeID,
					Envelope: &env,
					Feedback: "validated by " + nodeID,
				})
			}
		}
	}
}

// nodeValidationScopes extracts the ValidationScope(s) a node declares, for the
// node types that can act as deterministic validators. Today only gate nodes
// declare a scope (GateNodeConfig.Validates). Returns nil for any other node
// type or a gate with no scope.
func nodeValidationScopes(node v1alpha1.NodeSpec) []claim.ValidationScope {
	if node.Type != "gate" {
		return nil
	}
	cfg, err := parseConfig[GateNodeConfig](node.Config)
	if err != nil {
		return nil
	}
	return cfg.Validates
}

// ExecuteGraph is a convenience function: create a default-registry executor and
// run the graph in one call. Used by the worker for Pipeline CRs.
func ExecuteGraph(ctx context.Context, graph v1alpha1.GraphSpec, pipelineName string, deps Dependencies, opts ...GraphExecutorOption) (ExecutionResult, error) {
	registry := NewDefaultRegistry(deps)
	exec := NewGraphExecutor(registry, deps.DaprClient, opts...)
	return exec.Execute(ctx, graph, pipelineName)
}

// truncateForTimeline keeps payloads small: evidence rows, not log dumps.
func truncateForTimeline(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) { // don't split a rune
		cut--
	}
	return s[:cut] + "…"
}
