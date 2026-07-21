import { memo } from "react";
import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { RFNodeData, NodeExecState } from "../types";

// State → CSS class suffix + color.
const STATE_STYLES: Record<NodeExecState, { cls: string; color: string }> = {
  pending: { cls: "pending", color: "#6b7280" },
  running: { cls: "running", color: "#f59e0b" },
  green: { cls: "green", color: "#10b981" },
  failed: { cls: "failed", color: "#ef4444" },
};

// PipelineNode is the custom React Flow node that renders a harmostes pipeline
// node. Each node type gets a colored header and shows the node ID + type.
// In live execution mode, the node's border and a status dot reflect the
// execution state (pending → running → green/red), and gate feedback is
// shown inline on failed nodes.
export const PipelineNode = memo(function PipelineNode({ data, selected }: NodeProps) {
  const d = data as RFNodeData;
  if (!d?.typeMeta) return null;

  const exec = d.exec;
  const stateStyle = exec ? STATE_STYLES[exec.state] : null;
  const borderColor = stateStyle ? stateStyle.color : d.typeMeta.color;

  return (
    <div
      className={`pipeline-node ${selected ? "pipeline-node--selected" : ""} ${
        exec ? `pipeline-node--${stateStyle!.cls}` : ""
      }`}
      style={{ borderColor }}
    >
      <Handle type="target" position={Position.Top} className="pipeline-node-handle" />

      <div
        className="pipeline-node-header"
        style={{ backgroundColor: d.typeMeta.color }}
      >
        <span className="pipeline-node-type-label">{d.typeMeta.label}</span>
        {exec && (
          <span
            className={`pipeline-node-state-dot pipeline-node-state-dot--${stateStyle!.cls}`}
            style={{ backgroundColor: stateStyle!.color }}
          />
        )}
      </div>

      <div className="pipeline-node-body">
        <span className="pipeline-node-id">{d.spec.id}</span>
        {exec ? (
          <span
            className={`pipeline-node-status-badge pipeline-node-status-badge--${stateStyle!.cls}`}
            style={{ color: stateStyle!.color }}
          >
            {exec.state === "running" ? "●" : exec.state === "green" ? "✓" : exec.state === "failed" ? "✕" : "○"}
            {exec.durationMs !== undefined && exec.durationMs > 0 && (
              <span className="pipeline-node-duration">{formatDuration(exec.durationMs)}</span>
            )}
          </span>
        ) : (
          <span className="pipeline-node-cat">{d.typeMeta.category}</span>
        )}
      </div>

      {/* Agent metrics overlay (token counts, tool calls) */}
      {exec?.outputs && d.spec.type === "agent" && (
        <div className="pipeline-node-metrics">
          {exec.outputs.message_chars !== undefined && (
            <span title="Prompt chars">📝 {formatMetric(exec.outputs.message_chars)}</span>
          )}
          {exec.outputs.tool_calls !== undefined && (
            <span title="Tool calls">🔧 {formatMetric(exec.outputs.tool_calls)}</span>
          )}
          {exec.outputs.turns !== undefined && (
            <span title="Turns">🔄 {formatMetric(exec.outputs.turns)}</span>
          )}
        </div>
      )}

      {/* Gate feedback on failed nodes */}
      {exec?.feedback && exec.state === "failed" && (
        <div className="pipeline-node-feedback" title={exec.feedback}>
          {truncate(exec.feedback, 120)}
        </div>
      )}

      <Handle type="source" position={Position.Bottom} className="pipeline-node-handle" />
    </div>
  );
});

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m${Math.floor((ms % 60000) / 1000)}s`;
}

function formatMetric(v: unknown): string {
  if (typeof v === "number") {
    if (v >= 1000) return `${(v / 1000).toFixed(1)}k`;
    return String(v);
  }
  return String(v);
}

function truncate(s: string, n: number): string {
  if (s.length <= n) return s;
  return s.slice(0, n) + "…";
}
