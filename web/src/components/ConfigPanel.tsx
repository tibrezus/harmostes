import type { Edge, Node } from "@xyflow/react";
import type { ConfigField, RFNodeData } from "../types";
import { NODE_TYPES } from "../nodeTypes";

interface ConfigPanelProps {
  node: Node<RFNodeData> | null;
  edge: Edge | null;
  onUpdateConfig: (key: string, value: string) => void;
  onUpdateNodeId: (newId: string) => void;
  onUpdateNodeTimeout: (timeout: string) => void;
  onUpdateEdge: (edgeId: string, when: string, maxRetries: number) => void;
  onDeleteNode: () => void;
}

export function ConfigPanel({
  node,
  edge,
  onUpdateConfig,
  onUpdateNodeId,
  onUpdateNodeTimeout,
  onUpdateEdge,
  onDeleteNode,
}: ConfigPanelProps) {
  if (edge) {
    return <EdgeConfigPanel edge={edge} onUpdate={onUpdateEdge} />;
  }

  if (!node) {
    return (
      <div className="config-panel config-panel--empty">
        <p className="muted">
          Select a node or edge to edit its configuration.
          <br />
          <br />
          Drag node types from the left palette onto the canvas.
          <br />
          <br />
          Drag from a node's bottom handle to another node's top handle to create an edge.
        </p>
      </div>
    );
  }

  const data = node.data as RFNodeData;
  const typeMeta = data.typeMeta;
  const config = data.spec.config || {};

  return (
    <div className="config-panel">
      <div className="config-panel-header" style={{ borderColor: typeMeta.color }}>
        <span className="config-panel-type" style={{ color: typeMeta.color }}>
          {typeMeta.label}
        </span>
        <span className="config-panel-cat">{typeMeta.category}</span>
      </div>

      <div className="config-panel-section">
        <label className="config-label">Node ID</label>
        <input
          className="config-input"
          value={data.spec.id}
          onChange={(e) => onUpdateNodeId(e.target.value)}
        />
      </div>

      <div className="config-panel-section">
        <p className="config-field-desc">{typeMeta.description}</p>
      </div>

      {typeMeta.configFields.map((field) => (
        <ConfigFieldInput
          key={field.key}
          field={field}
          value={getNestedValue(config, field.key)}
          onChange={(v) => onUpdateConfig(field.key, v)}
        />
      ))}

      <div className="config-panel-section">
        <label className="config-label">When (condition)</label>
        <input
          className="config-input"
          placeholder="{{ nodes.check.outputs.changed }}"
          value={data.spec.when || ""}
          onChange={(e) => onUpdateConfig("__when__", e.target.value)}
        />
        <p className="config-field-hint">Optional: template expression for node execution condition.</p>
      </div>

      <div className="config-panel-section">
        <label className="config-label">Timeout (circuit breaker)</label>
        <input
          className="config-input"
          placeholder="30s, 5m, 1h"
          value={data.spec.timeout || ""}
          onChange={(e) => onUpdateNodeTimeout(e.target.value)}
        />
        <p className="config-field-hint">Max execution time before the node is killed. Empty = no limit.</p>
      </div>

      <div className="config-panel-actions">
        <button className="btn btn-danger btn-sm" onClick={onDeleteNode}>
          Delete Node
        </button>
      </div>
    </div>
  );
}

function ConfigFieldInput({
  field,
  value,
  onChange,
}: {
  field: ConfigField;
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <div className="config-panel-section">
      <label className="config-label">
        {field.label}
        {field.required && <span className="config-required">*</span>}
      </label>
      {field.type === "textarea" ? (
        <textarea
          className="config-textarea"
          placeholder={field.placeholder}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          rows={3}
        />
      ) : field.type === "select" ? (
        <select
          className="config-select"
          value={value || field.default || ""}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">— select —</option>
          {field.options?.map((opt) => (
            <option key={opt} value={opt}>
              {opt}
            </option>
          ))}
        </select>
      ) : field.type === "number" ? (
        <input
          type="number"
          className="config-input"
          placeholder={field.placeholder || field.default}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : (
        <input
          type="text"
          className="config-input"
          placeholder={field.placeholder || field.default}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {field.placeholder && field.type !== "select" && (
        <p className="config-field-hint">{field.placeholder}</p>
      )}
    </div>
  );
}

function EdgeConfigPanel({
  edge,
  onUpdate,
}: {
  edge: Edge;
  onUpdate: (edgeId: string, when: string, maxRetries: number) => void;
}) {
  const data = (edge.data || {}) as { when?: string; maxRetries?: number };
  const conditions = ["", "green", "failed", "changed", "unchanged"];

  return (
    <div className="config-panel">
      <div className="config-panel-header">
        <span className="config-panel-type">Edge</span>
      </div>

      <div className="config-panel-section">
        <label className="config-label">From → To</label>
        <p className="config-field-value">
          {edge.source} → {edge.target}
        </p>
      </div>

      <div className="config-panel-section">
        <label className="config-label">Condition (when)</label>
        <select
          className="config-select"
          value={data.when || ""}
          onChange={(e) => onUpdate(edge.id, e.target.value, data.maxRetries || 0)}
        >
          {conditions.map((c) => (
            <option key={c || "always"} value={c}>
              {c || "always"}
            </option>
          ))}
          <option value="__custom__">custom template</option>
        </select>
      </div>

      {data.when && !conditions.includes(data.when) && (
        <div className="config-panel-section">
          <label className="config-label">Custom condition</label>
          <input
            className="config-input"
            value={data.when}
            onChange={(e) => onUpdate(edge.id, e.target.value, data.maxRetries || 0)}
          />
        </div>
      )}

      <div className="config-panel-section">
        <label className="config-label">Max Retries (loop-back)</label>
        <input
          type="number"
          className="config-input"
          min={0}
          value={data.maxRetries || 0}
          onChange={(e) => onUpdate(edge.id, data.when || "", parseInt(e.target.value) || 0)}
        />
        <p className="config-field-hint">
          Set &gt; 0 for gate feedback loops (agent → gate → agent on failure).
        </p>
      </div>
    </div>
  );
}

// getNestedValue reads a.b.c from an object.
function getNestedValue(obj: Record<string, unknown>, path: string): string {
  const keys = path.split(".");
  let cur: unknown = obj;
  for (const k of keys) {
    if (cur === null || cur === undefined || typeof cur !== "object") return "";
    cur = (cur as Record<string, unknown>)[k];
  }
  if (cur === null || cur === undefined) return "";
  if (typeof cur === "boolean") return cur ? "true" : "false";
  return String(cur);
}
