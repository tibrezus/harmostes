// Package v1alpha1 — Graph types for the graph-native workflow model (ADR-0001).
//
// These types define the directed graph structure used by spec.graph on the
// Workflow CRD. Nodes are typed units of work; edges connect them with
// optional conditions. The graph executor (internal/graph) walks this
// structure at runtime.
//
// Historically these lived in pipeline_types.go alongside the Pipeline CRD.
// The Pipeline CRD has been removed (superseded by Workflow.spec.graph);
// these graph types survive because the Workflow CRD embeds them directly.
package v1alpha1

import "encoding/json"

// GraphSpec is the directed graph: nodes + edges.
type GraphSpec struct {
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
}

// NodeSpec defines one node in the workflow graph.
type NodeSpec struct {
	// ID is the unique identifier for this node within the graph.
	// Used by edges to reference this node.
	ID string `json:"id"`

	// Type is the node type (determines the executor):
	// plugin | agent | gate | branch | dapr-state-get | dapr-state-set |
	// dapr-publish | vela-app | flux-reconcile | http-call | human-gate |
	// external
	//
	// "external" nodes are NEVER executed: they declare out-of-band systems
	// that participate in the workflow outside the kernel (async agents,
	// CI pipelines, the upstream itself). The map renders them (conceptual
	// topology); edges to/from them are display-only routing annotations.
	Type string `json:"type"`

	// Label is an optional human-readable display label for the map/canvas.
	// Falls back to ID when empty.
	//+optional
	Label string `json:"label,omitempty"`

	// Config is type-specific node configuration (raw JSON).
	// Validated by the node type's executor before execution.
	//+optional
	Config json.RawMessage `json:"config,omitempty"`

	// Outputs declares the output names this node produces.
	// Downstream nodes reference these via template expressions:
	//   {{ nodes.<id>.outputs.<name> }}
	//+optional
	Outputs []string `json:"outputs,omitempty"`

	// When is an optional condition that must evaluate truthy for this
	// node to execute. Uses template expressions (Jinja2-style).
	//+optional
	When string `json:"when,omitempty"`

	// Timeout is the maximum duration this node is allowed to run before being
	// killed (circuit breaker) — the node's INTERNAL budget, enforced inside
	// the run. It is not the Job's wall clock: the wall is the workflow's
	// reviewReady.runBound (default OneShotRunBound), and a node timeout
	// larger than the wall is an authoring error the wall enforces (#348).
	// Format is a Go duration string: "30s", "5m",
	// "1h". Empty means no timeout (inherits the pipeline's overall deadline).
	//+optional
	Timeout string `json:"timeout,omitempty"`

	// Requires declares the Surface Capabilities this node needs against
	// declared External System Bindings (ADR-0003). The kernel enforces, before
	// execution, that each named binding exists and grants the requested
	// capability. Binding presence != blanket access.
	//+optional
	Requires []CapabilityRequirement `json:"requires,omitempty"`

	// Retry is the transient-fault retry policy for this node (ADR-0012 §9).
	// A failed node is retried ONLY when its executor classified the failure
	// transient — for plugin nodes, exit code 75 (EX_TEMPFAIL) or 69
	// (EX_UNAVAILABLE). Node timeouts and executor errors are terminal, as
	// are all non-transient failures. Empty means no retry: the failure goes
	// straight into the graph's when:failed routing.
	//+optional
	Retry *RetryPolicy `json:"retry,omitempty"`
}

// RetryPolicy bounds the in-run retry of a transient node failure.
// Backoff doubles from InitialDelay up to MaxDelay. Budgets are
// seconds-capped on purpose: long waits belong to the cross-run re-arm
// (claims, schedules), not to a sleeping pod holding its claim slot.
type RetryPolicy struct {
	// MaxAttempts is the TOTAL number of execution attempts, including the
	// first. 1 (the default when absent) means no retry. Hard-capped at 5.
	//+kubebuilder:validation:Minimum=1
	//+kubebuilder:validation:Maximum=5
	MaxAttempts int `json:"maxAttempts,omitempty"`

	// InitialDelay is the backoff before the first retry. Go duration
	// string ("2s", "500ms"); default "5s".
	//+optional
	InitialDelay string `json:"initialDelay,omitempty"`

	// MaxDelay caps backoff growth. Go duration string; default "1m".
	//+optional
	MaxDelay string `json:"maxDelay,omitempty"`
}

// EdgeSpec defines a directed edge between two nodes.
type EdgeSpec struct {
	// From is the source node ID.
	From string `json:"from"`

	// To is the target node ID.
	To string `json:"to"`

	// When is the condition for traversing this edge:
	//   green     — previous gate/branch output was green/true
	//   failed    — previous gate/branch output was failed/false
	//   changed   — previous branch output was changed=true
	//   unchanged — previous branch output was changed=false
	//   {{ expr }} — template expression evaluating to truthy
	// Empty means always traverse (sequential).
	//+optional
	When string `json:"when,omitempty"`

	// MaxRetries limits how many times a loop-back edge can be traversed.
	// Used for gate feedback loops: agent → gate → [failed] → agent.
	// 0 means no limit (use with caution).
	//+optional
	MaxRetries int `json:"maxRetries,omitempty"`
}
