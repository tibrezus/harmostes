import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  type Connection,
  type Edge,
  type Node,
  type NodeTypes,
  MarkerType,
  BackgroundVariant,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";

import type { EdgeSpec, GraphSpec, NodeType, RFNodeData } from "../types";
import { NODE_TYPES } from "../nodeTypes";
import { autoLayout, edgeLabel } from "../graph-utils";
import { PipelineNode } from "../components/PipelineNode";
import { NodePalette } from "../components/NodePalette";
import { ConfigPanel } from "../components/ConfigPanel";

const nodeTypes: NodeTypes = {
  harmostes: PipelineNode,
};

let nodeCounter = 0;
function generateNodeId(): string {
  nodeCounter++;
  return `node-${Date.now().toString(36)}-${nodeCounter}`;
}

// WorkflowResponse from GET /api/workflows/{name}/graph
interface WorkflowGraphResponse {
  workflow: string;
  disabled: boolean;
  graphNative: boolean;
  source: { kind: string; repo?: string; branch?: string };
  trigger: string;
  graph: GraphSpec;
}

interface WorkflowCanvasProps {
  name: string;
}

// rfToGraph serializes React Flow state back to a GraphSpec.
function rfToGraph(nodes: Node<RFNodeData>[], edges: Edge[]): GraphSpec {
  return {
    nodes: nodes.map((n) => n.data.spec),
    edges: edges.map((e) => ({
      from: e.source,
      to: e.target,
      when: (e.data as { when?: string })?.when || "",
      maxRetries: (e.data as { maxRetries?: number })?.maxRetries || 0,
    })),
  };
}

// WorkflowCanvas is the unified canvas for Workflow CRs. It handles two modes:
//
// 1. **Read-only** (declarative workflows — no spec.graph): renders the compiled
//    graph (prepare → agent → deploy). Shows a "Convert to Graph" button that
//    promotes the workflow to graph-native mode.
//
// 2. **Editable** (graph-native workflows — spec.graph present): full React Flow
//    editor with palette, save, config panel. Edits persist to spec.graph via
//    PUT /api/workflows/{name}/graph. This is the "canvas → code" direction.
export function WorkflowCanvas({ name }: WorkflowCanvasProps) {
  const [nodes, setNodes, onNodesChange] = useNodesState<Node<RFNodeData>>([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([]);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [graphNative, setGraphNative] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveMsg, setSaveMsg] = useState("");
  const [converting, setConverting] = useState(false);

  // Load the graph from the API.
  const loadGraph = useCallback(async () => {
    setLoading(true);
    try {
      const resp = await fetch(`/api/workflows/${encodeURIComponent(name)}/graph`);
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const data: WorkflowGraphResponse = await resp.json();
      setGraphNative(data.graphNative);
      const { nodes: rfNodes, edges: rfEdges } = autoLayout(data.graph);
      setNodes(rfNodes);
      setEdges(rfEdges);
      setDirty(false);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, [name, setNodes, setEdges]);

  useEffect(() => {
    loadGraph();
  }, [loadGraph]);

  // Track dirty state (only in editable mode).
  useEffect(() => {
    if (!loading && graphNative) setDirty(true);
  }, [nodes, edges, loading, graphNative]);

  const onConnect = useCallback(
    (conn: Connection) => {
      const newEdge: Edge = {
        ...conn,
        id: `e-${conn.source}-${conn.target}-${Date.now()}`,
        type: "smoothstep",
        markerEnd: { type: MarkerType.ArrowClosed },
        data: { when: "", maxRetries: 0 },
      };
      setEdges((eds) => addEdge(newEdge, eds));
    },
    [setEdges],
  );

  const onNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    setSelectedNodeId(node.id);
  }, []);

  const onPaneClick = useCallback(() => {
    setSelectedNodeId(null);
  }, []);

  const onDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  }, []);

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault();
      const nodeType = e.dataTransfer.getData("application/harmostes-node-type") as NodeType;
      if (!nodeType || !NODE_TYPES[nodeType]) return;

      const bounds = (e.currentTarget as HTMLElement).getBoundingClientRect();
      const position = {
        x: e.clientX - bounds.left - 100,
        y: e.clientY - bounds.top - 40,
      };

      const typeMeta = NODE_TYPES[nodeType];
      const id = generateNodeId();
      const newNode: Node<RFNodeData> = {
        id,
        type: "harmostes",
        position,
        data: { spec: { id, type: nodeType, config: {} }, label: typeMeta.label, typeMeta },
      };
      setNodes((nds) => nds.concat(newNode));
      setSelectedNodeId(id);
    },
    [setNodes],
  );

  const deleteSelectedNode = useCallback(() => {
    if (!selectedNodeId) return;
    setNodes((nds) => nds.filter((n) => n.id !== selectedNodeId));
    setEdges((eds) => eds.filter((e) => e.source !== selectedNodeId && e.target !== selectedNodeId));
    setSelectedNodeId(null);
  }, [selectedNodeId, setNodes, setEdges]);

  const updateNodeConfig = useCallback(
    (key: string, value: string) => {
      if (!selectedNodeId) return;
      setNodes((nds) =>
        nds.map((n) => {
          if (n.id !== selectedNodeId) return n;
          if (key === "__when__") {
            return { ...n, data: { ...n.data, spec: { ...n.data.spec, when: value } } };
          }
          const config = { ...(n.data.spec.config || {}) };
          setNestedValue(config, key, value);
          return { ...n, data: { ...n.data, spec: { ...n.data.spec, config } } };
        }),
      );
    },
    [selectedNodeId, setNodes],
  );

  const updateNodeId = useCallback(
    (newId: string) => {
      if (!selectedNodeId || !newId) return;
      if (nodes.some((n) => n.id === newId)) return;
      setNodes((nds) =>
        nds.map((n) =>
          n.id === selectedNodeId
            ? { ...n, id: newId, data: { ...n.data, spec: { ...n.data.spec, id: newId } } }
            : n,
        ),
      );
      setEdges((eds) =>
        eds.map((e) => ({
          ...e,
          source: e.source === selectedNodeId ? newId : e.source,
          target: e.target === selectedNodeId ? newId : e.target,
        })),
      );
      setSelectedNodeId(newId);
    },
    [selectedNodeId, nodes, setNodes, setEdges],
  );

  const updateNodeTimeout = useCallback(
    (timeout: string) => {
      if (!selectedNodeId) return;
      setNodes((nds) =>
        nds.map((n) =>
          n.id === selectedNodeId
            ? { ...n, data: { ...n.data, spec: { ...n.data.spec, timeout } } }
            : n,
        ),
      );
    },
    [selectedNodeId, setNodes],
  );

  const updateEdgeCondition = useCallback(
    (edgeId: string, when: string, maxRetries: number) => {
      setEdges((eds) =>
        eds.map((e) =>
          e.id === edgeId
            ? {
                ...e,
                label: edgeLabel({ from: e.source, to: e.target, when, maxRetries }),
                animated: when === "failed",
                type: maxRetries ? "step" : "smoothstep",
                data: { when, maxRetries },
              }
            : e,
        ),
      );
    },
    [setEdges],
  );

  // Save graph to spec.graph (canvas → code).
  const handleSave = async () => {
    setSaving(true);
    setSaveMsg("");
    try {
      const graph = rfToGraph(nodes, edges);
      const resp = await fetch(`/api/workflows/${encodeURIComponent(name)}/graph`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ graph }),
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body.error || `HTTP ${resp.status}`);
      }
      setSaveMsg("Saved ✓");
      setDirty(false);
    } catch (e) {
      setSaveMsg(`Error: ${e}`);
    } finally {
      setSaving(false);
    }
  };

  // Convert declarative workflow → graph-native (promotes to editable canvas).
  const handleConvert = async () => {
    setConverting(true);
    try {
      const resp = await fetch(`/api/workflows/${encodeURIComponent(name)}/convert`, {
        method: "POST",
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body.error || `HTTP ${resp.status}`);
      }
      // Reload as graph-native.
      await loadGraph();
    } catch (e) {
      setSaveMsg(`Error: ${e}`);
    } finally {
      setConverting(false);
    }
  };

  const selectedNode = useMemo(
    () => nodes.find((n) => n.id === selectedNodeId) || null,
    [nodes, selectedNodeId],
  );

  const selectedEdge = useMemo(
    () => edges.find((e) => e.id === selectedNodeId) || null,
    [edges, selectedNodeId],
  );

  if (loading) {
    return <div className="editor-loading">Loading workflow canvas…</div>;
  }

  if (error) {
    return (
      <div className="editor-error">
        <p className="error">{error}</p>
        <a href={`/workflows/${encodeURIComponent(name)}`} className="btn btn-primary">
          ← Back to workflow
        </a>
      </div>
    );
  }

  return (
    <div className="pipeline-editor">
      {/* Top bar */}
      <div className="editor-topbar">
        <a href={`/workflows/${encodeURIComponent(name)}`} className="link-muted">
          ← Workflow
        </a>
        <span className="pipeline-name-input" style={{ fontWeight: 600 }}>
          {name}
        </span>
        {graphNative ? (
          <>
            <span className="badge badge-green">Graph-native</span>
            <button
              className={`btn ${dirty ? "btn-primary" : "btn-secondary"}`}
              onClick={handleSave}
              disabled={saving}
            >
              {saving ? "Saving…" : "Save"}
            </button>
            {saveMsg && (
              <span className={saveMsg.startsWith("Error") ? "error" : "save-ok"}>{saveMsg}</span>
            )}
          </>
        ) : (
          <>
            <span className="badge badge-muted">Declarative</span>
            <button
              className="btn btn-secondary"
              onClick={handleConvert}
              disabled={converting}
              title="Compile this workflow's prepare → agent → deploy into spec.graph, making it editable on the canvas"
            >
              {converting ? "Converting…" : "Convert to Graph"}
            </button>
            {saveMsg && <span className="error">{saveMsg}</span>}
          </>
        )}
      </div>

      <div className="editor-body">
        {/* Left: palette (only in editable mode) */}
        {graphNative ? <NodePalette /> : <div style={{ width: "200px" }} />}

        {/* Center: Canvas */}
        <div className="canvas-container" onDrop={graphNative ? onDrop : undefined} onDragOver={graphNative ? onDragOver : undefined}>
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={graphNative ? onConnect : undefined}
            onNodeClick={onNodeClick}
            onEdgeClick={graphNative ? (_, edge) => setSelectedNodeId(edge.id) : undefined}
            onPaneClick={onPaneClick}
            nodeTypes={nodeTypes}
            fitView
            nodesDraggable={graphNative}
            nodesConnectable={graphNative}
            elementsSelectable={true}
            deleteKeyCode={graphNative ? ["Backspace", "Delete"] : []}
          >
            <Background variant={BackgroundVariant.Dots} gap={16} size={1} />
            <Controls showInteractive={false} />
            <MiniMap
              nodeColor={(n) => (n.data as RFNodeData)?.typeMeta?.color || "#6366f1"}
              maskColor="rgba(0,0,0,0.3)"
            />
          </ReactFlow>
        </div>

        {/* Right: Config panel */}
        <ConfigPanel
          node={selectedNode}
          edge={selectedEdge}
          onUpdateConfig={graphNative ? updateNodeConfig : () => {}}
          onUpdateNodeId={graphNative ? updateNodeId : () => {}}
          onUpdateNodeTimeout={graphNative ? updateNodeTimeout : () => {}}
          onUpdateEdge={graphNative ? updateEdgeCondition : () => {}}
          onDeleteNode={graphNative ? deleteSelectedNode : () => {}}
        />
      </div>
    </div>
  );
}

function setNestedValue(obj: Record<string, unknown>, path: string, value: string) {
  const keys = path.split(".");
  let cur = obj;
  for (let i = 0; i < keys.length - 1; i++) {
    const k = keys[i];
    if (cur[k] === undefined || typeof cur[k] !== "object") {
      cur[k] = {};
    }
    cur = cur[k] as Record<string, unknown>;
  }
  const lastKey = keys[keys.length - 1];
  if (value === "") {
    delete cur[lastKey];
  } else {
    const num = Number(value);
    if (!isNaN(num) && value.trim() !== "") {
      cur[lastKey] = num;
    } else if (value === "true" || value === "false") {
      cur[lastKey] = value === "true";
    } else {
      cur[lastKey] = value;
    }
  }
}
