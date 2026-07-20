// Types mirroring the harmostes Pipeline CRD (api/v1alpha1/pipeline_types.go).
// These are the wire format for the /api/pipelines endpoints.

export interface NodeSpec {
  id: string;
  type: NodeType;
  config?: Record<string, unknown>;
  outputs?: string[];
  when?: string;
}

export interface EdgeSpec {
  from: string;
  to: string;
  when?: string;
  maxRetries?: number;
}

export interface GraphSpec {
  nodes: NodeSpec[];
  edges: EdgeSpec[];
}

export interface TriggerSpec {
  type: string;
  config?: Record<string, unknown>;
}

export interface PipelineSpec {
  trigger: TriggerSpec;
  graph: GraphSpec;
  dapr?: { appID: string; components?: string[] };
  disabled?: boolean;
}

export interface Pipeline {
  apiVersion?: string;
  kind?: string;
  metadata: {
    name: string;
    namespace?: string;
    labels?: Record<string, string>;
    creationTimestamp?: string;
  };
  spec: PipelineSpec;
  status?: {
    phase?: string;
    nodeCount?: number;
    lastRunAt?: string;
    message?: string;
  };
}

export interface PipelineSummary {
  name: string;
  nodes: number;
  trigger: string;
  phase: string;
  updatedAt: string;
}

// Node type catalog — mirrors the CRD enum (chart/crds/pipelines.harmostes.dev.yaml)
// and the executor registry (internal/graph/node.go).
export type NodeType =
  | "plugin"
  | "agent"
  | "gate"
  | "branch"
  | "dapr-state-get"
  | "dapr-state-set"
  | "dapr-publish"
  | "vela-app"
  | "flux-reconcile"
  | "http-call"
  | "human-gate";

// Node type metadata for the palette + config panel.
export interface NodeTypeMeta {
  type: NodeType;
  label: string;
  category: "deterministic" | "non-deterministic";
  color: string;
  description: string;
  // Config field definitions for the auto-generated config panel.
  configFields: ConfigField[];
}

export interface ConfigField {
  key: string;
  label: string;
  type: "text" | "textarea" | "number" | "select";
  placeholder?: string;
  options?: string[];
  required?: boolean;
  default?: string;
}

// React Flow node data — wraps the NodeSpec with display metadata.
export interface RFNodeData {
  spec: NodeSpec;
  label: string;
  typeMeta: NodeTypeMeta;
  [key: string]: unknown;
}

export interface RFEdgeData {
  when?: string;
  maxRetries?: number;
  [key: string]: unknown;
}
