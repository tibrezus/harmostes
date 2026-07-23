// Shared graph utilities for rendering GraphSpec in React Flow.
// Used by both PipelineEditor (editable) and WorkflowCanvas (read-only).

import {
  type Edge,
  type Node,
  MarkerType,
} from "@xyflow/react";

import type { EdgeSpec, GraphSpec, NodeType, RFNodeData } from "./types";
import { NODE_TYPES } from "./nodeTypes";

// Auto-layout: BFS-based layered layout. Nodes without incoming edges go at
// layer 0. Each subsequent layer goes 160px lower. Nodes in the same layer are
// spread horizontally with 260px spacing.
export function autoLayout(graph: GraphSpec): { nodes: Node<RFNodeData>[]; edges: Edge[] } {
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

// Edge label: shows condition + retry count.
export function edgeLabel(e: EdgeSpec): string | undefined {
  if (!e.when) return undefined;
  if (e.maxRetries) return `${e.when} (×${e.maxRetries})`;
  return e.when;
}

// Derive execution states from a stream of lifecycle events.
// Used by both the pipeline editor and workflow canvas for live overlays.
export function deriveExecStates(
  events: { event: string; node?: string; status?: string; feedback?: string; outputs?: Record<string, unknown>; durationMs?: number; timestamp: string }[]
): Record<string, { state: string; feedback?: string; outputs?: Record<string, unknown>; durationMs?: number; startedAt?: string; completedAt?: string } | undefined> {
  const map: Record<string, { state: string; feedback?: string; outputs?: Record<string, unknown>; durationMs?: number; startedAt?: string; completedAt?: string }> = {};
  for (const ev of events) {
    if (!ev.node) continue;
    switch (ev.event) {
      case "node.started":
        map[ev.node] = { state: "running", startedAt: ev.timestamp };
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
