import type { NodeType, NodeTypeMeta } from "./types";

// NODE_TYPES is the canonical catalog of pipeline node types.
// Each entry defines: label, category (deterministic/non-deterministic),
// display color, description, and config field schema for the auto-generated
// config panel.
//
// This mirrors:
// - CRD enum (chart/crds/pipelines.harmostes.dev.yaml)
// - Executor registry (internal/graph/node.go NewDefaultRegistry)
// - Graph-native platform docs (wiki/concepts/graph-native-platform.md)
export const NODE_TYPES: Record<NodeType, NodeTypeMeta> = {
  plugin: {
    type: "plugin",
    label: "Plugin",
    category: "deterministic",
    color: "#6366f1",
    description: "Shell script (built-in / ConfigMap / image). Windmill-style script block.",
    configFields: [
      { key: "plugin.name", label: "Plugin Name", type: "text", placeholder: "rig-emit", required: true },
      { key: "plugin.configMap", label: "ConfigMap", type: "text", placeholder: "plugin-config" },
      { key: "plugin.key", label: "Key", type: "text", placeholder: "script.sh" },
    ],
  },
  agent: {
    type: "agent",
    label: "Agent",
    category: "non-deterministic",
    color: "#f59e0b",
    description: "pi.dev RPC session (LLM + skills + tools + gate feedback loop).",
    configFields: [
      { key: "model", label: "Model", type: "select", options: ["zai/glm-5.2", "zai/glm-4.7", "zai/glm-5.1"], default: "zai/glm-5.2" },
      { key: "skill", label: "Skill", type: "text", placeholder: "llm-wiki" },
      { key: "task", label: "Task", type: "text", placeholder: "tasks/wiki-update" },
      { key: "maxFixes", label: "Max Fixes", type: "number", default: "3" },
    ],
  },
  gate: {
    type: "gate",
    label: "Gate",
    category: "deterministic",
    color: "#ec4899",
    description: "Validation script (exit 0 = green, stderr = feedback).",
    configFields: [
      { key: "plugin.name", label: "Gate Plugin", type: "text", placeholder: "wiki-lint", required: true },
      { key: "plugin.configMap", label: "ConfigMap", type: "text" },
      { key: "plugin.key", label: "Key", type: "text" },
    ],
  },
  branch: {
    type: "branch",
    label: "Branch",
    category: "deterministic",
    color: "#14b8a6",
    description: "Template expression evaluation. Zero tokens. Outputs {changed: bool}.",
    configFields: [
      { key: "condition", label: "Condition", type: "textarea", placeholder: '{{ gt (index (index .Nodes "prepare") "changed") "false" }}', required: true },
    ],
  },
  "dapr-state-get": {
    type: "dapr-state-get",
    label: "State Get",
    category: "deterministic",
    color: "#8b5cf6",
    description: "Read a key from a Dapr state store.",
    configFields: [
      { key: "store", label: "Store Name", type: "text", placeholder: "harmostes-state", required: true },
      { key: "key", label: "Key", type: "text", placeholder: "pipeline/{{ .Pipeline }}/last-hash", required: true },
    ],
  },
  "dapr-state-set": {
    type: "dapr-state-set",
    label: "State Set",
    category: "deterministic",
    color: "#8b5cf6",
    description: "Write a key to a Dapr state store.",
    configFields: [
      { key: "store", label: "Store Name", type: "text", placeholder: "harmostes-state", required: true },
      { key: "key", label: "Key", type: "text", placeholder: "pipeline/{{ .Pipeline }}/last-hash", required: true },
      { key: "value", label: "Value", type: "textarea", placeholder: '{{ index (index .Nodes "agent") "commit" }}', required: true },
    ],
  },
  "dapr-publish": {
    type: "dapr-publish",
    label: "Publish",
    category: "deterministic",
    color: "#8b5cf6",
    description: "Publish a message to a Dapr pub/sub topic.",
    configFields: [
      { key: "pubsub", label: "PubSub Name", type: "text", placeholder: "harmostes-pubsub", required: true },
      { key: "topic", label: "Topic", type: "text", placeholder: "harmostes-events", required: true },
      { key: "payload", label: "Payload (JSON)", type: "textarea", placeholder: '{"event":"custom"}' },
    ],
  },
  "vela-app": {
    type: "vela-app",
    label: "Vela App",
    category: "deterministic",
    color: "#3b82f6",
    description: "Create/update/delete/wait a KubeVela Application CR.",
    configFields: [
      { key: "action", label: "Action", type: "select", options: ["apply", "delete", "wait"], default: "apply" },
      { key: "name", label: "Application Name", type: "text", required: true },
    ],
  },
  "flux-reconcile": {
    type: "flux-reconcile",
    label: "Flux Reconcile",
    category: "deterministic",
    color: "#3b82f6",
    description: "Trigger Flux reconciliation + wait for Ready.",
    configFields: [
      { key: "resource", label: "Resource", type: "text", placeholder: "helmrelease/my-service", required: true },
      { key: "namespace", label: "Namespace", type: "text", placeholder: "production" },
      { key: "wait", label: "Wait", type: "select", options: ["true", "false"], default: "true" },
    ],
  },
  "http-call": {
    type: "http-call",
    label: "HTTP Call",
    category: "deterministic",
    color: "#64748b",
    description: "HTTP request (GET/POST/...).",
    configFields: [
      { key: "method", label: "Method", type: "select", options: ["GET", "POST", "PUT", "DELETE", "PATCH"], default: "GET" },
      { key: "url", label: "URL", type: "text", placeholder: "https://api.example.com/health", required: true },
    ],
  },
  "human-gate": {
    type: "human-gate",
    label: "Human Gate",
    category: "non-deterministic",
    color: "#ef4444",
    description: "Pause for human approval (Dapr state + pub/sub).",
    configFields: [
      { key: "message", label: "Approval Message", type: "textarea", placeholder: "Approve deployment to production?", required: true },
      { key: "timeout", label: "Timeout (seconds)", type: "number", default: "3600" },
    ],
  },
};

export const PALETTE_NODE_TYPES = Object.values(NODE_TYPES);
