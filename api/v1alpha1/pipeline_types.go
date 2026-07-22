// Package v1alpha1 — Pipeline CRD types for the graph-native platform (v0.8.0).
//
// A Pipeline is a directed graph of nodes connected by edges. Unlike the
// fixed-shape Workflow CRD (prepare → agent → gate → deploy), a Pipeline's
// graph is user-defined: any arrangement of deterministic and non-deterministic
// nodes, connected by conditional edges, including loop-backs for gate
// feedback.
//
// The graph model mirrors what canvas UIs (React Flow, Dify, Langflow) use:
//   { nodes: [...], edges: [{source, target}] }
//
// Backward compatibility: existing Workflow CRs continue to work. The worker
// compiles a Workflow's fixed pipeline into an equivalent graph internally.

package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Pipeline is a graph-native automation pipeline: a directed graph of nodes
// (deterministic + non-deterministic) connected by edges (sequential,
// conditional, loop-back). Defined as YAML, rendered on a canvas, executed
// by the worker's graph executor.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pipe
// +kubebuilder:printcolumn:name=Trigger,type=string,JSONPath=.spec.trigger.type
// +kubebuilder:printcolumn:name=Nodes,type=integer,JSONPath=.status.nodeCount
// +kubebuilder:printcolumn:name=Status,type=string,JSONPath=.status.phase
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp
type Pipeline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PipelineSpec   `json:"spec,omitempty"`
	Status PipelineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PipelineList is a list of Pipelines.
type PipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pipeline `json:"items"`
}

// PipelineSpec declares a graph-native pipeline.
type PipelineSpec struct {
	// Trigger defines what starts this pipeline.
	Trigger TriggerSpec `json:"trigger"`

	// Graph is the directed graph of nodes and edges.
	Graph GraphSpec `json:"graph"`

	// Dapr configures the Dapr sidecar for this pipeline.
	//+optional
	Dapr *DaprSpec `json:"dapr,omitempty"`

	// Disabled prevents the pipeline from being scheduled.
	//+optional
	Disabled bool `json:"disabled,omitempty"`
}

// TriggerSpec defines what starts a pipeline.
type TriggerSpec struct {
	// Type is the trigger mechanism: webhook | schedule | event | manual.
	Type string `json:"type"`

	// Config is type-specific trigger configuration (raw JSON).
	//+optional
	Config json.RawMessage `json:"config,omitempty"`
}

// GraphSpec is the directed graph: nodes + edges.
type GraphSpec struct {
	Nodes []NodeSpec `json:"nodes"`
	Edges []EdgeSpec `json:"edges,omitempty"`
}

// NodeSpec defines one node in the pipeline graph.
type NodeSpec struct {
	// ID is the unique identifier for this node within the graph.
	// Used by edges to reference this node.
	ID string `json:"id"`

	// Type is the node type (determines the executor):
	// plugin | agent | gate | branch | dapr-state-get | dapr-state-set |
	// dapr-publish | vela-app | flux-reconcile | http-call | human-gate
	Type string `json:"type"`

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
	// killed (circuit breaker). Format is a Go duration string: "30s", "5m",
	// "1h". Empty means no timeout (inherits the pipeline's overall deadline).
	// When the timeout fires, the node is marked failed with feedback
	// "timed out after {duration}".
	//+optional
	Timeout string `json:"timeout,omitempty"`
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

// DaprSpec configures the Dapr sidecar for a pipeline.
type DaprSpec struct {
	// AppID is the Dapr application ID.
	AppID string `json:"appID"`

	// Components names the Dapr components (state stores, pub/sub, etc.)
	// to enable for this pipeline. Referenced by dapr-* node types.
	//+optional
	Components []string `json:"components,omitempty"`
}

// PipelineStatus is reported by the controller after pipeline execution.
type PipelineStatus struct {
	// ObservedGeneration is the generation observed by the controller.
	//+optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the current pipeline phase: idle | running | succeeded | failed.
	//+optional
	Phase string `json:"phase,omitempty"`

	// NodeCount is the number of nodes in the graph (for the CRD column).
	//+optional
	NodeCount int `json:"nodeCount,omitempty"`

	// LastRunAt is when the pipeline was last executed.
	//+optional
	LastRunAt metav1.Time `json:"lastRunAt,omitempty"`

	// Message is a human-readable status message.
	//+optional
	Message string `json:"message,omitempty"`

	// Conditions are standard k8s conditions.
	//+optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy
// ---------------------------------------------------------------------------

func (in *Pipeline) DeepCopyInto(out *Pipeline) { deepCopy(in, out) }
func (in *Pipeline) DeepCopy() *Pipeline {
	if in == nil {
		return nil
	}
	out := new(Pipeline)
	deepCopy(in, out)
	return out
}
func (in *Pipeline) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PipelineList) DeepCopyInto(out *PipelineList) { deepCopy(in, out) }
func (in *PipelineList) DeepCopy() *PipelineList {
	if in == nil {
		return nil
	}
	out := new(PipelineList)
	deepCopy(in, out)
	return out
}
func (in *PipelineList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*Pipeline)(nil)
	_ runtime.Object = (*PipelineList)(nil)
)

// Resource returns the GroupResource for Pipeline-related resources.
// This allows callers to use v1alpha1.Resource("pipelines") for client operations.
func PipelineResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource("pipelines").GroupResource()
}

// init registers Pipeline types in the scheme.
func init() {
	// Pipeline types are registered alongside Workflow types via addKnownTypes.
}
