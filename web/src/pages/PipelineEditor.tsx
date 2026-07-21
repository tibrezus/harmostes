import { useCallback, useEffect, useMemo, useRef, useState } from "react";
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

import type { EdgeSpec, GraphSpec, NodeSpec, NodeType, RFNodeData, LifecycleEvent, NodeExecMeta } from "../types";
import { NODE_TYPES } from "../nodeTypes";
import { PipelineNode } from "../components/PipelineNode";
import { NodePalette } from "../components/NodePalette";
import { ConfigPanel } from "../components/ConfigPanel";
import { RunTimeline } from "../components/RunTimeline";
import { usePipelineEvents } from "../hooks/usePipelineEvents";

// Register custom node types for React Flow.
const nodeTypes: NodeTypes = {
  harmostes: PipelineNode,
};

let nodeCounter = 0;

function generateNodeId(): string {
  nodeCounter++;
  return `node-${Date.now().toString(36)}-${nodeCounter}`;
}

// Auto-layout: simple vertical cascade. Nodes without incoming edges go at y=0.
// Each subsequent layer goes 150px lower.
function autoLayout(graph: GraphSpec): { nodes: Node<RFNodeData>[]; edges: Edge[] } {
  const inDegree: Record<string, number> = {};
  const adj: Record<string, string[]> = {};
  for (const n of graph.nodes) {
    inDegree[n.id] = 0;
    adj[n.id] = [];
  }
  for (const e of graph.edges) {
    if (e.maxRetries === 0 || e.maxRetries === undefined) {
      inDegree[e.to] = (inDegree[e.to] || 0) + 1;
    }
    adj[e.from] = adj[e.from] || [];
    adj[e.from].push(e.to);
  }

  // BFS layers.
  const layers: Record<string, number> = {};
  const queue: string[] = [];
  for (const n of graph.nodes) {
    if ((inDegree[n.id] || 0) === 0) {
      layers[n.id] = 0;
      queue.push(n.id);
    }
  }
  while (queue.length > 0) {
    const id = queue.shift()!;
    for (const next of adj[id] || []) {
      const newLayer = (layers[id] || 0) + 1;
      if (layers[next] === undefined || layers[next] < newLayer) {
        layers[next] = newLayer;
        queue.push(next);
      }
    }
  }
  // Any remaining nodes (in cycles) get max layer + 1.
  const maxLayer = Math.max(0, ...Object.values(layers));
  for (const n of graph.nodes) {
    if (layers[n.id] === undefined) {
      layers[n.id] = maxLayer + 1;
    }
  }

  // Position nodes in each layer.
  const layerCounts: Record<number, number> = {};
  const rfNodes: Node<RFNodeData>[] = graph.nodes.map((n) => {
    const layer = layers[n.id] || 0;
    const idx = layerCounts[layer] || 0;
    layerCounts[layer] = idx + 1;
    const typeMeta = NODE_TYPES[n.type as NodeType] || NODE_TYPES.plugin;
    return {
      id: n.id,
      type: "harmostes",
      position: { x: 100 + idx * 260, y: layer * 160 },
      data: { spec: n, label: typeMeta.label, typeMeta } as RFNodeData,
    };
  });

  const rfEdges: Edge[] = graph.edges.map((e, i) => ({
    id: `e-${e.from}-${e.to}-${i}`,
    source: e.from,
    target: e.to,
    label: edgeLabel(e),
    animated: e.when === "failed",
    type: e.maxRetries ? "step" : "smoothstep",
    markerEnd: { type: MarkerType.ArrowClosed },
    data: { when: e.when, maxRetries: e.maxRetries },
  }));

  return { nodes: rfNodes, edges: rfEdges };
}

function edgeLabel(e: EdgeSpec): string | undefined {
  if (!e.when) return undefined;
  if (e.maxRetries) return `${e.when} (×${e.maxRetries})`;
  return e.when;
}

// Serialize React Flow state back to Pipeline CR graph spec.
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

interface PipelineEditorProps {
  name?: string;
}

export function PipelineEditor({ name }: PipelineEditorProps) {
  const isNew = !name;
  const [pipelineName, setPipelineName] = useState(name || "");
  const [triggerType, setTriggerType] = useState("webhook");
  const [nodes, setNodes, onNodesChange] = useNodesState<Node<RFNodeData>>([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([]);
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null);
  const [loading, setLoading] = useState(!isNew);
  const [error, setError] = useState("");
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveMsg, setSaveMsg] = useState("");
  const [timelineOpen, setTimelineOpen] = useState(false);
  const [liveMode, setLiveMode] = useState(false);

  // SSE lifecycle events for this pipeline (G7 live execution).
  // Only connect when viewing an existing (saved) pipeline.
  const ssePipelineName = !isNew && pipelineName ? pipelineName : undefined;
  const { events: pipelineEvents, connected: sseConnected } = usePipelineEvents(ssePipelineName);

  // Auto-enable live mode when the first event arrives.
  const lastEventCount = useRef(0);
  useEffect(() => {
    if (pipelineEvents.length > 0 && pipelineEvents.length !== lastEventCount.current) {
      lastEventCount.current = pipelineEvents.length;
      setLiveMode(true);
      if (!timelineOpen) setTimelineOpen(true);
    }
  }, [pipelineEvents, timelineOpen]);

  // Load existing pipeline.
  useEffect(() => {
    if (isNew) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    (async () => {
      setLoading(true);
      try {
        const resp = await fetch(`/api/pipelines/${encodeURIComponent(name!)}`);
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const pipe = await resp.json();
        if (cancelled) return;
        setPipelineName(pipe.metadata.name);
        setTriggerType(pipe.spec.trigger?.type || "webhook");
        const { nodes: rfNodes, edges: rfEdges } = autoLayout(pipe.spec.graph);
        setNodes(rfNodes);
        setEdges(rfEdges);
      } catch (e) {
        if (!cancelled) setError(String(e));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  // Track dirty state.
  useEffect(() => {
    if (!loading) setDirty(true);
  }, [nodes, edges, triggerType, loading]);

  // Sync execution states to nodes when events arrive (G7 live execution).
  // This overlays execution metadata (state, feedback, metrics) on the
  // existing canvas nodes without changing their positions or structure.
  useEffect(() => {
    if (pipelineEvents.length === 0 || nodes.length === 0) return;
    const execMap = deriveExecStates(pipelineEvents);
    setNodes((nds) =>
      nds.map((n) => {
        const exec = execMap[n.id];
        // Only update if the exec state changed (avoid unnecessary re-renders).
        if (exec || n.data.exec) {
          return { ...n, data: { ...n.data, exec } };
        }
        return n;
      })
    );
  }, [pipelineEvents, setNodes]); // intentionally exclude nodes — we use setNodes callback

  // Animate edges to show data flow direction during execution.
  useEffect(() => {
    if (!liveMode || pipelineEvents.length === 0) return;
    const execMap = deriveExecStates(pipelineEvents);
    setEdges((eds) =>
      eds.map((e) => {
        const sourceExec = execMap[e.source];
        const targetExec = execMap[e.target];
        // Animate: source completed (green) and target running.
        const animate =
          (sourceExec?.state === "green" && targetExec?.state === "running") ||
          targetExec?.state === "running";
        return { ...e, animated: animate || e.data?.when === "failed" };
      })
    );
  }, [pipelineEvents, liveMode, setEdges]);

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
    [setEdges]
  );

  const onNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    setSelectedNodeId(node.id);
  }, []);

  const onPaneClick = useCallback(() => {
    setSelectedNodeId(null);
  }, []);

  // Drag from palette → create node at drop position.
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
        data: {
          spec: { id, type: nodeType, config: {} },
          label: typeMeta.label,
          typeMeta,
        },
      };
      setNodes((nds) => nds.concat(newNode));
      setSelectedNodeId(id);
    },
    [setNodes]
  );

  // Delete selected node + its edges.
  const deleteSelectedNode = useCallback(() => {
    if (!selectedNodeId) return;
    setNodes((nds) => nds.filter((n) => n.id !== selectedNodeId));
    setEdges((eds) => eds.filter((e) => e.source !== selectedNodeId && e.target !== selectedNodeId));
    setSelectedNodeId(null);
  }, [selectedNodeId, setNodes, setEdges]);

  // Update selected node's config (or top-level when condition).
  const updateNodeConfig = useCallback(
    (key: string, value: string) => {
      if (!selectedNodeId) return;
      setNodes((nds) =>
        nds.map((n) => {
          if (n.id !== selectedNodeId) return n;
          if (key === "__when__") {
            return {
              ...n,
              data: { ...n.data, spec: { ...n.data.spec, when: value } },
            };
          }
          const config = { ...(n.data.spec.config || {}) };
          setNestedValue(config, key, value);
          return {
            ...n,
            data: { ...n.data, spec: { ...n.data.spec, config } },
          };
        })
      );
    },
    [selectedNodeId, setNodes]
  );

  // Update selected node's ID.
  const updateNodeId = useCallback(
    (newId: string) => {
      if (!selectedNodeId || !newId) return;
      // Check for duplicate.
      if (nodes.some((n) => n.id === newId)) return;
      setNodes((nds) =>
        nds.map((n) =>
          n.id === selectedNodeId
            ? { ...n, id: newId, data: { ...n.data, spec: { ...n.data.spec, id: newId } } }
            : n
        )
      );
      // Update edges that reference the old ID.
      setEdges((eds) =>
        eds.map((e) => ({
          ...e,
          source: e.source === selectedNodeId ? newId : e.source,
          target: e.target === selectedNodeId ? newId : e.target,
        }))
      );
      setSelectedNodeId(newId);
    },
    [selectedNodeId, nodes, setNodes, setEdges]
  );

  // Update edge condition.
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
            : e
        )
      );
    },
    [setEdges]
  );

  // Save pipeline.
  const handleSave = async () => {
    const finalName = pipelineName.trim();
    if (!finalName) {
      alert("Pipeline name is required");
      return;
    }
    setSaving(true);
    setSaveMsg("");
    try {
      const graph = rfToGraph(nodes, edges);
      const resp = await fetch(`/api/pipelines/${encodeURIComponent(finalName)}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          spec: {
            trigger: { type: triggerType },
            graph,
          },
        }),
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({ error: resp.statusText }));
        throw new Error(body.error || `HTTP ${resp.status}`);
      }
      setSaveMsg("Saved ✓");
      setDirty(false);
      if (isNew) {
        // Navigate to the editor URL.
        window.history.replaceState({}, "", `/pipelines/${encodeURIComponent(finalName)}`);
      }
    } catch (e) {
      setSaveMsg(`Error: ${e}`);
    } finally {
      setSaving(false);
    }
  };

  // Export graph as YAML/JSON.
  const graphYaml = useMemo(() => {
    const graph = rfToGraph(nodes, edges);
    return JSON.stringify({ trigger: { type: triggerType }, graph }, null, 2);
  }, [nodes, edges, triggerType]);

  const selectedNode = useMemo(
    () => nodes.find((n) => n.id === selectedNodeId) || null,
    [nodes, selectedNodeId]
  );

  const selectedEdge = useMemo(
    () => edges.find((e) => e.id === selectedNodeId) || null,
    [edges, selectedNodeId]
  );

  // YAML apply: parse JSON text, rebuild canvas.
  const [yamlText, setYamlText] = useState("");
  const [yamlOpen, setYamlOpen] = useState(false);
  useEffect(() => {
    setYamlText(graphYaml);
  }, [graphYaml]);

  const applyYaml = () => {
    try {
      const parsed = JSON.parse(yamlText);
      if (!parsed.graph) throw new Error("missing 'graph' key");
      if (parsed.trigger?.type) setTriggerType(parsed.trigger.type);
      const { nodes: rfNodes, edges: rfEdges } = autoLayout(parsed.graph as GraphSpec);
      setNodes(rfNodes);
      setEdges(rfEdges);
      setSelectedNodeId(null);
    } catch (e) {
      alert(`Invalid YAML/JSON: ${e}`);
    }
  };

  if (loading) {
    return <div className="editor-loading">Loading pipeline…</div>;
  }
  if (error) {
    return (
      <div className="editor-error">
        <p className="error">{error}</p>
        <a href="/pipelines" className="btn btn-primary">← Back to list</a>
      </div>
    );
  }

  return (
    <div className="pipeline-editor">
      {/* Top bar */}
      <div className="editor-topbar">
        <a href="/pipelines" className="link-muted">← Pipelines</a>
        <input
          className="pipeline-name-input"
          value={pipelineName}
          onChange={(e) => setPipelineName(e.target.value)}
          placeholder="pipeline-name"
          disabled={!isNew && !!name}
        />
        <select
          className="trigger-select"
          value={triggerType}
          onChange={(e) => setTriggerType(e.target.value)}
        >
          <option value="webhook">webhook</option>
          <option value="schedule">schedule</option>
          <option value="manual">manual</option>
        </select>
        <button
          className={`btn ${dirty ? "btn-primary" : "btn-secondary"}`}
          onClick={handleSave}
          disabled={saving}
        >
          {saving ? "Saving…" : "Save"}
        </button>
        {saveMsg && <span className={saveMsg.startsWith("Error") ? "error" : "save-ok"}>{saveMsg}</span>}
        {liveMode && (
          <span className={`live-indicator ${sseConnected ? "live-indicator--connected" : "live-indicator--disconnected"}`}>
            <span className="live-indicator-dot" />
            {sseConnected ? "Live" : "Reconnecting…"}
          </span>
        )}
        <button className="btn btn-secondary btn-sm" onClick={() => setTimelineOpen(!timelineOpen)}>
          {timelineOpen ? "Hide Timeline" : "Timeline"}
          {pipelineEvents.length > 0 && <span className="timeline-badge">{pipelineEvents.length}</span>}
        </button>
        <button className="btn btn-secondary btn-sm" onClick={() => setYamlOpen(!yamlOpen)}>
          {yamlOpen ? "Hide YAML" : "Show YAML"}
        </button>
      </div>

      <div className="editor-body">
        {/* Left: Node palette */}
        <NodePalette />

        {/* Center: Canvas */}
        <div className="canvas-container" onDrop={onDrop} onDragOver={onDragOver}>
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onNodeClick={onNodeClick}
            onEdgeClick={(_, edge) => setSelectedNodeId(edge.id)}
            onPaneClick={onPaneClick}
            nodeTypes={nodeTypes}
            fitView
            deleteKeyCode={["Backspace", "Delete"]}
          >
            <Background variant={BackgroundVariant.Dots} gap={16} size={1} />
            <Controls />
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
          onUpdateConfig={updateNodeConfig}
          onUpdateNodeId={updateNodeId}
          onUpdateEdge={updateEdgeCondition}
          onDeleteNode={deleteSelectedNode}
        />
      </div>

      {/* Bottom: Timeline panel (collapsible, G7 live execution) */}
      {timelineOpen && (
        <div className="timeline-panel">
          <RunTimeline events={pipelineEvents} />
        </div>
      )}

      {/* Bottom: YAML panel (collapsible) */}
      {yamlOpen && (
        <div className="yaml-panel">
          <div className="yaml-panel-header">
            <span>Graph YAML (JSON format)</span>
            <button className="btn btn-secondary btn-sm" onClick={applyYaml}>
              Apply to canvas
            </button>
          </div>
          <textarea
            className="yaml-textarea"
            value={yamlText}
            onChange={(e) => setYamlText(e.target.value)}
            spellCheck={false}
          />
        </div>
      )}
    </div>
  );
}

// setNestedValue sets a.b.c = value in an object.
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
    // Try to parse as number/bool.
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

// deriveExecStates processes a stream of lifecycle events and builds a map of
// node ID → execution metadata. The last event for each node wins (e.g.
// node.started → running, then node.completed → green).
function deriveExecStates(events: LifecycleEvent[]): Record<string, NodeExecMeta | undefined> {
  const map: Record<string, NodeExecMeta> = {};
  for (const ev of events) {
    if (!ev.node) continue;
    switch (ev.event) {
      case "node.started":
        map[ev.node] = {
          state: "running",
          startedAt: ev.timestamp,
        };
        break;
      case "node.completed":
        map[ev.node] = {
          state: ev.status === "failed" ? "failed" : "green",
          feedback: ev.feedback,
          outputs: ev.outputs,
          durationMs: ev.durationMs,
          startedAt: map[ev.node]?.startedAt,
          completedAt: ev.timestamp,
        };
        break;
      case "node.failed":
        map[ev.node] = {
          state: "failed",
          feedback: ev.feedback,
          durationMs: ev.durationMs,
          completedAt: ev.timestamp,
        };
        break;
    }
  }
  return map;
}
