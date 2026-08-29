package ui

import (
	"encoding/json"
	"fmt"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// ---------------------------------------------------------------------------
// Pipeline View Model — dynamic graph rendering for templates + workflows
// ---------------------------------------------------------------------------
//
// The pipeline graph renderer normalizes ANY template/workflow spec into a
// display-ready structure. The fixed prepare → agent → gate → deploy form is
// just one topology; graph-native specs (custom nodes + edges) are another.
// Both compile into the same PipelineView, so the template renders identically.
//
// The graph is topologically sorted for left-to-right display. Each node
// carries a type (for CSS styling), a label, a sublabel, and optional
// metadata key-value pairs shown beneath the node.

// PipelineNode is a single node in the rendered pipeline graph.
type PipelineNode struct {
	ID       string             // node ID (prepare, agent, gate, deploy, or custom)
	Type     string             // node type: prepare, agent, gate, deploy, plugin, branch, custom
	Label    string             // display label (Silkscreen): PREPARE, AGENT, GATE, DEPLOY
	Sublabel string             // plugin or model name (VT323): rig-emit, litellm/zai/anthropic/glm-5.3-flash
	Meta     []PipelineNodeMeta // optional metadata rows beneath the node
}

// PipelineNodeMeta is a key-value row displayed inside a node.
type PipelineNodeMeta struct {
	Key   string
	Value string
}

// PipelineView is the complete display model for a pipeline graph.
type PipelineView struct {
	Nodes  []PipelineNode // topologically sorted for left-to-right flow
	Linear bool           // true if the graph is a simple linear chain (no branches)
}

// buildPipelineView compiles any graph spec into a display-ready PipelineView.
// It topologically sorts the nodes and extracts human-readable labels from
// each node's JSON config.
func buildPipelineView(gs v1alpha1.GraphSpec) PipelineView {
	sorted := topoSort(gs.Nodes, gs.Edges)
	nodes := make([]PipelineNode, 0, len(sorted))

	for _, n := range sorted {
		pn := nodeToPipelineNode(n)
		nodes = append(nodes, pn)
	}

	return PipelineView{
		Nodes:  nodes,
		Linear: isLinear(gs.Edges),
	}
}

// buildTemplatePipelineView compiles a WorkflowTemplate into a PipelineView.
func buildTemplatePipelineView(tmpl *v1alpha1.WorkflowTemplate) PipelineView {
	gs := graph.CompileTemplate(tmpl)
	return buildPipelineView(gs)
}

// buildWorkflowPipelineView compiles a Workflow into a PipelineView.
func buildWorkflowPipelineView(wf *v1alpha1.Workflow) PipelineView {
	var gs v1alpha1.GraphSpec
	if wf.Spec.Graph != nil {
		gs = *wf.Spec.Graph
	} else {
		gs = graph.CompileWorkflow(wf)
	}
	return buildPipelineView(gs)
}

// nodeToPipelineNode converts a graph NodeSpec into a display-ready PipelineNode.
// It parses the node's JSON config to extract plugin names, model, skill, etc.
func nodeToPipelineNode(n v1alpha1.NodeSpec) PipelineNode {
	pn := PipelineNode{
		ID:   n.ID,
		Type: n.Type,
	}

	switch n.Type {
	case "agent":
		pn.Label = "AGENT"
		var cfg graph.AgentNodeConfig
		if json.Unmarshal(n.Config, &cfg) == nil {
			if cfg.Model != "" {
				pn.Sublabel = cfg.Model
			}
			if cfg.Skill != "" {
				pn.Meta = append(pn.Meta, PipelineNodeMeta{Key: "skill", Value: cfg.Skill})
			}
			if len(cfg.Tools) > 0 {
				pn.Meta = append(pn.Meta, PipelineNodeMeta{Key: "tools", Value: fmt.Sprintf("%v", cfg.Tools)})
			}
			if cfg.MaxFixes > 0 {
				pn.Meta = append(pn.Meta, PipelineNodeMeta{Key: "maxFixes", Value: fmt.Sprintf("%d", cfg.MaxFixes)})
			}
			if cfg.Gate != nil && cfg.Gate.Plugin.Name != "" {
				pn.Meta = append(pn.Meta, PipelineNodeMeta{Key: "gate", Value: cfg.Gate.Plugin.Name})
			}
		}
	case "plugin":
		pn.Label = pluginTypeLabel(n.ID)
		var cfg graph.PluginNodeConfig
		if json.Unmarshal(n.Config, &cfg) == nil && cfg.Name != "" {
			pn.Sublabel = cfg.Name
		}
	default:
		pn.Label = labelFromID(n.ID)
		pn.Type = "custom"
	}

	return pn
}

// pluginTypeLabel maps a plugin node ID to its Silkscreen display label.
func pluginTypeLabel(id string) string {
	switch id {
	case "prepare":
		return "PREPARE"
	case "deploy":
		return "DEPLOY"
	case "gate":
		return "GATE"
	default:
		return labelFromID(id)
	}
}

// labelFromID converts a kebab-case ID to an UPPERCASE Silkscreen label.
func labelFromID(id string) string {
	return toUpper(id)
}

// toUpper is a simple ASCII uppercase without importing strings (keeps the
// import list minimal).
func toUpper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}

// isLinear returns true if the edges form a simple chain (each node has at
// most one outgoing edge and at most one incoming edge, except endpoints).
func isLinear(edges []v1alpha1.EdgeSpec) bool {
	outDeg := map[string]int{}
	inDeg := map[string]int{}
	for _, e := range edges {
		outDeg[e.From]++
		inDeg[e.To]++
	}
	for _, d := range outDeg {
		if d > 1 {
			return false
		}
	}
	for _, d := range inDeg {
		if d > 1 {
			return false
		}
	}
	return true
}

// topoSort returns nodes in topological order (left-to-right pipeline flow).
// Uses Kahn's algorithm. Falls back to original order if the graph has cycles.
func topoSort(nodes []v1alpha1.NodeSpec, edges []v1alpha1.EdgeSpec) []v1alpha1.NodeSpec {
	// Build adjacency + in-degree maps.
	nodeByID := map[string]v1alpha1.NodeSpec{}
	adj := map[string][]string{}
	inDeg := map[string]int{}

	for _, n := range nodes {
		nodeByID[n.ID] = n
		inDeg[n.ID] = 0
	}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		inDeg[e.To]++
	}

	// Seed queue with zero-in-degree nodes (preserving original order).
	var queue []string
	for _, n := range nodes {
		if inDeg[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}

	var result []v1alpha1.NodeSpec
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		result = append(result, nodeByID[id])
		for _, next := range adj[id] {
			inDeg[next]--
			if inDeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	// If cycle detected (not all nodes processed), fall back to original order.
	if len(result) < len(nodes) {
		return nodes
	}
	return result
}
