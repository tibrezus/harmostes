import { PALETTE_NODE_TYPES } from "../nodeTypes";
import type { NodeType } from "../types";

// NodePalette is the left sidebar showing all available node types.
// Users drag a node type onto the canvas to create a new node.
export function NodePalette() {
  const onDragStart = (e: React.DragEvent, nodeType: NodeType) => {
    e.dataTransfer.setData("application/harmostes-node-type", nodeType);
    e.dataTransfer.effectAllowed = "copy";
  };

  const deterministic = PALETTE_NODE_TYPES.filter((t) => t.category === "deterministic");
  const nonDeterministic = PALETTE_NODE_TYPES.filter((t) => t.category === "non-deterministic");

  return (
    <div className="node-palette">
      <div className="palette-section">
        <h3 className="palette-section-title">Deterministic</h3>
        <p className="palette-section-desc">Zero LLM tokens</p>
        {deterministic.map((t) => (
          <div
            key={t.type}
            className="palette-item"
            draggable
            onDragStart={(e) => onDragStart(e, t.type)}
            title={t.description}
          >
            <span className="palette-item-dot" style={{ backgroundColor: t.color }} />
            <span className="palette-item-label">{t.label}</span>
          </div>
        ))}
      </div>

      <div className="palette-section">
        <h3 className="palette-section-title">Non-deterministic</h3>
        <p className="palette-section-desc">Consumes LLM tokens</p>
        {nonDeterministic.map((t) => (
          <div
            key={t.type}
            className="palette-item"
            draggable
            onDragStart={(e) => onDragStart(e, t.type)}
            title={t.description}
          >
            <span className="palette-item-dot" style={{ backgroundColor: t.color }} />
            <span className="palette-item-label">{t.label}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
