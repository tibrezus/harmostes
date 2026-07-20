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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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

// ExecutionResult is the outcome of a full graph execution.
type ExecutionResult struct {
	// Status is the pipeline-level outcome: green if no visited node failed.
	Status NodeStatus
	// NodeResults maps node ID → latest result (overwritten on re-execution).
	NodeResults map[string]NodeResult
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
		Status:      StatusGreen,
		NodeResults: make(map[string]NodeResult),
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
	e.publishLifecycle(ctx, "pipeline.started", pipelineName, "", NodeResult{})

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

		e.log("node %s: type=%s executing", nodeID, node.Type)
		e.publishLifecycle(ctx, "node.started", pipelineName, nodeID, NodeResult{})

		// Execute via registry.
		exec, err := e.registry.Get(node.Type)
		if err != nil {
			result.Status = StatusFailed
			result.Message = fmt.Sprintf("node %s: %v", nodeID, err)
			result.NodeResults[nodeID] = NodeResult{Status: StatusFailed, Feedback: err.Error()}
			e.publishLifecycle(ctx, "node.failed", pipelineName, nodeID, result.NodeResults[nodeID])
			break
		}

		nodeResult, execErr := exec.Execute(ctx, node, env)
		if execErr != nil {
			nodeResult.Status = StatusFailed
			if nodeResult.Feedback == "" {
				nodeResult.Feedback = execErr.Error()
			}
		}
		if nodeResult.Status == "" {
			nodeResult.Status = StatusGreen
		}

		result.NodeResults[nodeID] = nodeResult
		e.checkpoint(ctx, pipelineName, nodeID, nodeResult)
		e.publishLifecycle(ctx, "node.completed", pipelineName, nodeID, nodeResult)
		e.log("node %s: status=%s", nodeID, nodeResult.Status)

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
				e.publishLifecycle(ctx, "pipeline.failed", pipelineName, "", result.NodeResults[nodeID])
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
	e.publishLifecycle(ctx, "pipeline.completed", pipelineName, "", NodeResult{})
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

// publishLifecycle publishes a node lifecycle event to the Dapr pub/sub.
// Best-effort: errors are logged but do not fail the pipeline.
func (e *GraphExecutor) publishLifecycle(ctx context.Context, event, pipelineName, nodeID string, result NodeResult) {
	if e.dapr == nil {
		return
	}
	payload := map[string]any{
		"event":    event,
		"pipeline": pipelineName,
	}
	if nodeID != "" {
		payload["node"] = nodeID
	}
	if result.Status != "" {
		payload["status"] = string(result.Status)
	}
	if result.Feedback != "" {
		payload["feedback"] = result.Feedback
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := e.dapr.Publish(ctx, e.pubsub, LifecycleTopic, string(b)); err != nil {
		e.log("warn: publish %s: %v", event, err)
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

// ExecuteGraph is a convenience function: create a default-registry executor and
// run the graph in one call. Used by the worker for Pipeline CRs.
func ExecuteGraph(ctx context.Context, graph v1alpha1.GraphSpec, pipelineName string, deps Dependencies, opts ...GraphExecutorOption) (ExecutionResult, error) {
	registry := NewDefaultRegistry(deps)
	exec := NewGraphExecutor(registry, deps.DaprClient, opts...)
	return exec.Execute(ctx, graph, pipelineName)
}
