// Package graph implements the graph-native pipeline model: a directed graph
// of nodes (deterministic + non-deterministic) connected by edges. Each node
// type has a registered executor that runs it.
//
// The node executor registry is the bridge between the Pipeline CRD's graph
// model and the existing worker/agent infrastructure. A "plugin" node wraps
// worker.RunPlugin; a "gate" node wraps worker.GatePlugin; an "agent" node
// wraps agent.Task; a "branch" node evaluates a template condition.
//
// New node types (dapr-state, vela-app, flux-reconcile, http-call,
// human-gate) are added by implementing NodeExecutor and calling
// Registry.Register.
package graph

import (
	"context"
	"fmt"
	"sort"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/worker"
)

// NodeStatus is the outcome of executing a node.
type NodeStatus string

const (
	// StatusGreen means the node succeeded (gate passed, plugin exited 0,
	// agent gate green, branch evaluated truthy).
	StatusGreen NodeStatus = "green"
	// StatusFailed means the node failed (gate rejected, plugin non-zero exit,
	// agent gate red after maxFixes).
	StatusFailed NodeStatus = "failed"
	// StatusSkipped means the node was not executed (condition was false).
	StatusSkipped NodeStatus = "skipped"
)

// NodeEnv is the execution context passed to every node executor. It carries
// enough state for executors to do their work without a direct k8s dependency.
type NodeEnv struct {
	Workflow     string                 // pipeline/workflow name
	Namespace    string                 // k8s namespace
	Workdir      string                 // shared working directory
	Source       string                 // resolved source ref/revision
	SourceURL    string                 // upstream source URL
	SourceBranch string                 // upstream source branch
	State        string                 // Dapr state key prefix
	Inputs       map[string]NodeOutputs // upstream node outputs: nodeID → outputs
}

// NodeOutputs holds the outputs of a single node, available to downstream nodes
// via template expressions: {{ nodes.<id>.outputs.<name> }}.
type NodeOutputs map[string]any

// NodeResult is what a NodeExecutor returns after running a node.
type NodeResult struct {
	Status   NodeStatus  // green | failed | skipped
	Outputs  NodeOutputs // this node's outputs (available to downstream nodes)
	Feedback string      // human-readable feedback (gate stderr, error msg, etc.)
}

// NodeExecutor executes a single node in the pipeline graph. Each node type
// (plugin, agent, gate, branch, ...) has exactly one registered executor.
type NodeExecutor interface {
	// Execute runs the node and returns its result.
	Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error)

	// Type returns the node type this executor handles (e.g. "plugin").
	Type() string

	// Deterministic returns true if the node never calls an LLM. Used by the
	// graph executor for optimization (deterministic subgraphs can be run in
	// deterministic-only mode).
	Deterministic() bool
}

// Registry maps node type strings to NodeExecutor implementations. It is the
// central catalog the graph executor consults to dispatch each node.
type Registry struct {
	executors map[string]NodeExecutor
}

// NewRegistry returns an empty node executor registry.
func NewRegistry() *Registry {
	return &Registry{executors: make(map[string]NodeExecutor)}
}

// Register adds (or replaces) the executor for a node type. The executor's
// Type() method must match the nodeType argument.
func (r *Registry) Register(exec NodeExecutor) {
	r.executors[exec.Type()] = exec
}

// Get returns the executor for the given node type, or an error if unregistered.
func (r *Registry) Get(nodeType string) (NodeExecutor, error) {
	exec, ok := r.executors[nodeType]
	if !ok {
		return nil, fmt.Errorf("graph: no executor registered for node type %q (registered: %s)", nodeType, r.Types())
	}
	return exec, nil
}

// Types returns all registered node types, sorted alphabetically.
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.executors))
	for t := range r.executors {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// Has reports whether a node type is registered.
func (r *Registry) Has(nodeType string) bool {
	_, ok := r.executors[nodeType]
	return ok
}

// Dependencies bundles the external infrastructure that default executors need.
// This is the single injection point — callers construct one Dependencies value
// and pass it to NewDefaultRegistry.
type Dependencies struct {
	PluginResolver worker.PluginResolver // resolves plugin refs to commands
	AgentRunner    AgentRunner           // runs the agent task→gate loop
	TaskResolver   TaskResolver          // resolves task templates (ConfigMap keys, etc.)
	DaprClient     dapr.Client           // Dapr sidecar client (state + pub/sub). Optional — nil-safe.
}

// AgentRunner runs the agent task→gate→feedback loop. This mirrors
// worker.AgentRunner so the graph package can wrap the existing RPC
// implementation without importing it directly (avoids a worker→graph cycle
// when the worker later compiles graphs).
type AgentRunner interface {
	Run(ctx context.Context, task string, gate agent.Gate, maxFixes int, log agent.Logger) (agent.Result, error)
}

// TaskResolver yields the agent's task text from a template reference.
type TaskResolver interface {
	Get(ctx context.Context, ref string) (string, error)
}

// NewDefaultRegistry returns a registry pre-loaded with the four core node
// executors: plugin, gate, agent, branch. Additional node types (dapr-state,
// vela-app, flux-reconcile, http-call, human-gate) are added incrementally
// in subsequent phases by calling Register on the returned registry.
func NewDefaultRegistry(deps Dependencies) *Registry {
	r := NewRegistry()
	r.Register(NewPluginExecutor(deps.PluginResolver))
	r.Register(NewGateExecutor(deps.PluginResolver))
	r.Register(NewAgentExecutor(deps.AgentRunner, deps.TaskResolver, deps.PluginResolver))
	r.Register(NewBranchExecutor())
	// Dapr node types (G3) — nil-safe: return error if executed without a client.
	r.Register(NewStateGetExecutor(deps.DaprClient))
	r.Register(NewStateSetExecutor(deps.DaprClient))
	r.Register(NewPublishExecutor(deps.DaprClient))
	return r
}
