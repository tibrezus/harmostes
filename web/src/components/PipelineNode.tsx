import { memo } from "react";
import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { RFNodeData } from "../types";

// PipelineNode is the custom React Flow node that renders a harmostes pipeline
// node. Each node type gets a colored header and shows the node ID + type.
export const PipelineNode = memo(function PipelineNode({ data, selected }: NodeProps) {
  const d = data as RFNodeData;
  if (!d?.typeMeta) return null;

  return (
    <div
      className={`pipeline-node ${selected ? "pipeline-node--selected" : ""}`}
      style={{ borderColor: d.typeMeta.color }}
    >
      <Handle type="target" position={Position.Top} className="pipeline-node-handle" />

      <div
        className="pipeline-node-header"
        style={{ backgroundColor: d.typeMeta.color }}
      >
        <span className="pipeline-node-type-label">{d.typeMeta.label}</span>
      </div>

      <div className="pipeline-node-body">
        <span className="pipeline-node-id">{d.spec.id}</span>
        <span className="pipeline-node-cat pipeline-node-cat--{d.typeMeta.category}">
          {d.typeMeta.category}
        </span>
      </div>

      <Handle type="source" position={Position.Bottom} className="pipeline-node-handle" />
    </div>
  );
});
