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

import type { EdgeSpec, GraphSpec, NodeSpec, NodeType, RFNodeData, NodeExecMeta } from "../types";
import { NODE_TYPES } from "../nodeTypes";
import { PipelineNode } from "../components/PipelineNode";
import { NodePalette } from "../components/NodePalette";
import { ConfigPanel } from "../components/ConfigPanel";
import { RunTimeline } from "../components/RunTimeline";
import { usePipelineEvents } from "../hooks/usePipelineEvents";
import {
  autoLayout,
  edgeLabel,
  rfToGraph,
  generateNodeId,
  setNestedValue,
  deriveExecStates,
} from "../graph-utils";

const nodeTypes: NodeTypes = {
  harmostes: PipelineNode,
};

interface WorkflowEditorProps {
  name?: string; // undefined = new workflow (creation mode)
}

export function WorkflowEditor({ name }: WorkflowEditorProps) {
  const isNew = !name;
  const [workflowName, setWorkflowName] = useState(name || "");
  const [repoUrl, setRepoUrl] = useState("");
  const [branch, setBranch] = useState("main");
  const [language, setLanguage] = useState("go");
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

  // SSE lifecycle events for live execution overlay.
  const sseName = !isNew && workflowName ? workflowName : undefined;
  const { events: pipelineEvents, connected: sseConnected } = usePipelineEvents(sseName);

  const lastEventCount = useRef(0);
  useEffect(() => {
    if (pipelineEvents.length > 0 && pipelineEvents.length !== lastEventCount.current) {
      lastEventCount.current = pipelineEvents.length;
      setLiveMode(true);
      if (!timelineOpen) setTimelineOpen(true);
    }
  }, [pipelineEvents, timelineOpen]);

  // Load existing workflow graph.
  useEffect(() => {
    if (isNew) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    (async () => {
      setLoading(true);
      try {
        const resp = await fetch(`/api/workflows/${encodeURIComponent(name!)}/graph`);
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const data = await resp.json();
        if (cancelled) return;
        setWorkflowName(data.workflow);
        if (data.source) {
          setRepoUrl(data.source.repo || "");
          setBranch(data.source.branch || "main");
          setLanguage(data.source.language || "go");
        }
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  useEffect(() => {
    if (!loading) setDirty(true);
  }, [nodes, edges, repoUrl, branch, language, loading]);

  // Live execution overlay.
  useEffect(() => {
    if (pipelineEvents.length === 0 || nodes.length === 0) return;
    const execMap = deriveExecStates(pipelineEvents);
    setNodes((nds) =>
      nds.map((n) => {
        const exec = execMap[n.id] as NodeExecMeta | undefined;
        if (exec || n.data.exec) {
          return { ...n, data: { ...n.data, exec } };
        }
        return n;
      })
    );
  }, [pipelineEvents, setNodes]);

  useEffect(() => {
    if (!liveMode || pipelineEvents.length === 0) return;
    const execMap = deriveExecStates(pipelineEvents);
    setEdges((eds) =>
      eds.map((e) => {
        const sourceExec = execMap[e.source];
        const targetExec = execMap[e.target];
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

  const onPaneClick = useCallback(() => setSelectedNodeId(null), []);

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
      const position = { x: e.clientX - bounds.left - 100, y: e.clientY - bounds.top - 40 };
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
    [setNodes]
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
        })
      );
    },
    [selectedNodeId, setNodes]
  );

  const updateNodeId = useCallback(
    (newId: string) => {
      if (!selectedNodeId || !newId) return;
      if (nodes.some((n) => n.id === newId)) return;
      setNodes((nds) =>
        nds.map((n) =>
          n.id === selectedNodeId
            ? { ...n, id: newId, data: { ...n.data, spec: { ...n.data.spec, id: newId } } }
            : n
        )
      );
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

  const updateNodeTimeout = useCallback(
    (timeout: string) => {
      if (!selectedNodeId) return;
      setNodes((nds) =>
        nds.map((n) =>
          n.id === selectedNodeId
            ? { ...n, data: { ...n.data, spec: { ...n.data.spec, timeout } } }
            : n
        )
      );
    },
    [selectedNodeId, setNodes]
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
            : e
        )
      );
    },
    [setEdges]
  );

  // Save: POST /api/workflows (new) or PUT /api/workflows/{name}/graph (existing).
  const handleSave = async () => {
    const finalName = workflowName.trim();
    if (!finalName) {
      alert("Workflow name is required");
      return;
    }
    if (isNew && !repoUrl.trim()) {
      alert("Source repository URL is required for new workflows");
      return;
    }
    if (nodes.length === 0) {
      alert("Add at least one node to the graph");
      return;
    }
    setSaving(true);
    setSaveMsg("");
    try {
      const graph = rfToGraph(nodes, edges);
      const source = { kind: "git", repo: repoUrl.trim(), branch: branch.trim() || "main", language: language.trim() };

      if (isNew) {
        // Create new graph-native workflow.
        const resp = await fetch("/api/workflows", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: finalName, source, graph }),
        });
        if (!resp.ok) {
          const body = await resp.json().catch(() => ({ error: resp.statusText }));
          if (resp.status === 403 && body.violations) {
            const msgs = (body.violations as { nodeType: string; nodeId: string; requires: string[] }[])
              .map((v) => `${v.nodeType} (${v.nodeId}): requires ${v.requires.join(" | ")}`)
              .join("\n");
            throw new Error(`RBAC violation — your groups lack permission:\n${msgs}`);
          }
          throw new Error(body.error || `HTTP ${resp.status}`);
        }
        // Switch to edit mode (update URL).
        window.history.replaceState({}, "", `/workflows/${encodeURIComponent(finalName)}/canvas`);
      } else {
        // Update existing workflow graph.
        const resp = await fetch(`/api/workflows/${encodeURIComponent(finalName)}/graph`, {
          method: "PUT",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ graph }),
        });
        if (!resp.ok) {
          const body = await resp.json().catch(() => ({ error: resp.statusText }));
          throw new Error(body.error || `HTTP ${resp.status}`);
        }
      }
      setSaveMsg("Saved ✓");
      setDirty(false);
    } catch (e) {
      setSaveMsg(`Error: ${e}`);
    } finally {
      setSaving(false);
    }
  };

  const graphYaml = useMemo(() => {
    const graph = rfToGraph(nodes, edges);
    return JSON.stringify(
      { source: { kind: "git", repo: repoUrl, branch, language }, graph },
      null,
      2
    );
  }, [nodes, edges, repoUrl, branch, language]);

  const selectedNode = useMemo(() => nodes.find((n) => n.id === selectedNodeId) || null, [nodes, selectedNodeId]);
  const selectedEdge = useMemo(() => edges.find((e) => e.id === selectedNodeId) || null, [edges, selectedNodeId]);

  const [yamlText, setYamlText] = useState("");
  const [yamlOpen, setYamlOpen] = useState(false);
  useEffect(() => { setYamlText(graphYaml); }, [graphYaml]);

  const applyYaml = () => {
    try {
      const parsed = JSON.parse(yamlText);
      if (!parsed.graph) throw new Error("missing 'graph' key");
      if (parsed.source) {
        setRepoUrl(parsed.source.repo || "");
        setBranch(parsed.source.branch || "main");
        setLanguage(parsed.source.language || "go");
      }
      const { nodes: rfNodes, edges: rfEdges } = autoLayout(parsed.graph as GraphSpec);
      setNodes(rfNodes);
      setEdges(rfEdges);
      setSelectedNodeId(null);
    } catch (e) {
      alert(`Invalid JSON: ${e}`);
    }
  };

  if (loading) {
    return <div className="editor-loading">Loading workflow…</div>;
  }
  if (error) {
    return (
      <div className="editor-error">
        <p className="error">{error}</p>
        <a href="/workflows" className="btn btn-primary">← Back to workflows</a>
      </div>
    );
  }

  return (
    <div className="pipeline-editor">
      <div className="editor-topbar">
        <a href="/workflows" className="link-muted">← Workflows</a>
        <input
          className="pipeline-name-input"
          value={workflowName}
          onChange={(e) => setWorkflowName(e.target.value)}
          placeholder="workflow-name"
          disabled={!isNew}
        />
        {isNew && (
          <>
            <input
              className="pipeline-name-input"
              style={{ minWidth: "200px" }}
              value={repoUrl}
              onChange={(e) => setRepoUrl(e.target.value)}
              placeholder="https://github.com/org/repo.git"
              title="Source repository URL"
            />
            <input
              className="pipeline-name-input"
              style={{ width: "80px" }}
              value={branch}
              onChange={(e) => setBranch(e.target.value)}
              placeholder="main"
              title="Branch"
            />
            <select value={language} onChange={(e) => setLanguage(e.target.value)} className="trigger-select">
              <option value="go">go</option>
              <option value="zig">zig</option>
              <option value="cargo">cargo</option>
              <option value="python">python</option>
              <option value="auto">auto</option>
            </select>
          </>
        )}
        <button className={`btn ${dirty ? "btn-primary" : "btn-secondary"}`} onClick={handleSave} disabled={saving}>
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
        <NodePalette />
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
        <ConfigPanel
          node={selectedNode}
          edge={selectedEdge}
          onUpdateConfig={updateNodeConfig}
          onUpdateNodeId={updateNodeId}
          onUpdateNodeTimeout={updateNodeTimeout}
          onUpdateEdge={updateEdgeCondition}
          onDeleteNode={deleteSelectedNode}
        />
      </div>

      {timelineOpen && (
        <div className="timeline-panel">
          <RunTimeline events={pipelineEvents} />
        </div>
      )}

      {yamlOpen && (
        <div className="yaml-panel">
          <div className="yaml-panel-header">
            <span>Workflow Graph (JSON format)</span>
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
