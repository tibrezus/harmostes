import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  useNodesState,
  useEdgesState,
  type Edge,
  type Node,
  type NodeTypes,
  BackgroundVariant,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import type { GraphSpec, RFNodeData } from "../types";
import { autoLayout } from "../graph-utils";
import { PipelineNode } from "../components/PipelineNode";
import { ConfigPanel } from "../components/ConfigPanel";

const nodeTypes: NodeTypes = {
  harmostes: PipelineNode,
};

// WorkflowResponse from GET /api/workflows/{name}/graph
interface WorkflowGraphResponse {
  workflow: string;
  disabled: boolean;
  source: { kind: string; repo?: string; branch?: string };
  trigger: string;
  graph: GraphSpec;
}

interface WorkflowCanvasProps {
  name: string;
}

// WorkflowCanvas renders a READ-ONLY React Flow canvas for an existing
// Workflow CR. The graph is compiled server-side from the Workflow spec
// (prepare → agent → deploy) via graph.CompileWorkflow().
//
// This bridges the two previously-disconnected systems: every Workflow now
// has a canvas view, while every Pipeline CR (graph-native) continues to use
// the editable PipelineEditor. In step 2, the two will converge into one
// unified model.
export function WorkflowCanvas({ name }: WorkflowCanvasProps) {
  const [nodes, setNodes, onNodesChange] = useNodesState<Node<RFNodeData>>([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([]);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [meta, setMeta] = useState<WorkflowGraphResponse | null>(null);

  // Load the compiled graph from the API.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoading(true);
      try {
        const resp = await fetch(`/api/workflows/${encodeURIComponent(name)}/graph`);
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const data: WorkflowGraphResponse = await resp.json();
        if (cancelled) return;
        setMeta(data);
        const { nodes: rfNodes, edges: rfEdges } = autoLayout(data.graph);
        setNodes(rfNodes);
        setEdges(rfEdges);
      } catch (e) {
        if (!cancelled) setError(String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [name, setNodes, setEdges]);

  const onNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    setSelectedNodeId(node.id);
  }, []);

  const onPaneClick = useCallback(() => {
    setSelectedNodeId(null);
  }, []);

  const selectedNode = useMemo(
    () => nodes.find((n) => n.id === selectedNodeId) || null,
    [nodes, selectedNodeId],
  );

  if (loading) {
    return <div className="editor-loading">Loading workflow canvas…</div>;
  }

  if (error) {
    return (
      <div className="editor-error">
        <p className="error">{error}</p>
        <a href={`/workflows/${encodeURIComponent(name)}`} className="btn btn-primary">← Back to workflow</a>
      </div>
    );
  }

  return (
    <div className="pipeline-editor">
      {/* Top bar — read-only mode */}
      <div className="editor-topbar">
        <a href={`/workflows/${encodeURIComponent(name)}`} className="link-muted">← Workflow</a>
        <span className="pipeline-name-input" style={{ fontWeight: 600 }}>
          {name}
        </span>
        {meta && (
          <>
            <span className="badge badge-blue">{meta.trigger}</span>
            {meta.disabled && <span className="badge badge-muted">Disabled</span>}
          </>
        )}
        <span className="save-ok" style={{ fontStyle: "italic" }}>Read-only canvas (compiled from Workflow spec)</span>
      </div>

      <div className="editor-body">
        {/* Left: empty (no palette in read-only mode) */}
        <div style={{ width: "200px" }} />

        {/* Center: Canvas */}
        <div className="canvas-container">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onNodeClick={onNodeClick}
            onPaneClick={onPaneClick}
            nodeTypes={nodeTypes}
            fitView
            nodesDraggable={false}
            nodesConnectable={false}
            elementsSelectable={true}
          >
            <Background variant={BackgroundVariant.Dots} gap={16} size={1} />
            <Controls showInteractive={false} />
            <MiniMap
              nodeColor={(n) => (n.data as RFNodeData)?.typeMeta?.color || "#6366f1"}
              maskColor="rgba(0,0,0,0.3)"
            />
          </ReactFlow>
        </div>

        {/* Right: Config panel (read-only inspection) */}
        <ConfigPanel
          node={selectedNode}
          edge={null}
          onUpdateConfig={() => {}}
          onUpdateNodeId={() => {}}
          onUpdateNodeTimeout={() => {}}
          onUpdateEdge={() => {}}
          onDeleteNode={() => {}}
        />
      </div>
    </div>
  );
}
