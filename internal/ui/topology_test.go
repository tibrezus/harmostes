package ui

// topology_test.go — the topology projection (ADR-0012 §3, #417): geometry
// equivalence with the run graph (one engine, two paintings), diff
// classification, the union-layout diff view, revision parsing, the YAML
// diff, the schema-derived palette, and the handler contract (testids,
// merged shape, degradation).

import (
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func specNodes(nodes ...v1alpha1.NodeSpec) []v1alpha1.NodeSpec { return nodes }

func chainGraph() v1alpha1.GraphSpec {
	return v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "prepare", Type: "plugin", Label: "pr-fetch"},
			{ID: "agent", Type: "agent", Label: "agent · mistral"},
			{ID: "deploy", Type: "plugin", Label: "post-review"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "prepare", To: "agent"},
			{From: "agent", To: "deploy"},
		},
	}
}

// The topology and the run graph share one geometry engine: identical
// GraphSpec ⇒ identical node anchors and edge paths. This is the contract
// that makes "one projection" true in the code, not just the prose.
func TestTopologyGeometry_MatchesRunGraph(t *testing.T) {
	gs := chainGraph()
	runNodes, runEdges, w, h := layoutGraph(gs, nil, false)
	topo := buildTopology(gs, nil)

	if topo.Width != w || topo.Height != h {
		t.Errorf("canvas = %dx%d, run graph = %dx%d", topo.Width, topo.Height, w, h)
	}
	if len(topo.Nodes) != len(runNodes) || len(topo.Edges) != len(runEdges) {
		t.Fatalf("counts differ: topo %d/%d, run %d/%d", len(topo.Nodes), len(topo.Edges), len(runNodes), len(runEdges))
	}
	for i := range runNodes {
		if topo.Nodes[i].ID != runNodes[i].ID ||
			topo.Nodes[i].X != runNodes[i].X || topo.Nodes[i].Y != runNodes[i].Y ||
			topo.Nodes[i].Label != runNodes[i].Label {
			t.Errorf("node %d anchors differ: %+v vs %+v", i, topo.Nodes[i], runNodes[i])
		}
	}
	for i := range runEdges {
		if topo.Edges[i].Path != runEdges[i].Path {
			t.Errorf("edge %d path differs", i)
		}
	}
}

// The palette is schema-derived: Known marks vocabulary membership, unknown
// types are flagged for the template (data-unknown-type), never dropped.
func TestBuildTopology_Palette(t *testing.T) {
	gs := chainGraph()
	gs.Nodes = append(gs.Nodes, v1alpha1.NodeSpec{ID: "weird", Type: "flux-hologram", Label: "w"})

	palette := map[string]bool{"plugin": true, "agent": true}
	topo := buildTopology(gs, palette)
	byID := map[string]topologyNodeView{}
	for _, n := range topo.Nodes {
		byID[n.ID] = n
	}
	if !byID["prepare"].Known || !byID["agent"].Known {
		t.Error("vocabulary types must be Known")
	}
	if byID["weird"].Known {
		t.Error("unknown type must not be Known")
	}
	// nil palette (schema unreachable) degrades: nothing Known, nothing lost.
	naked := buildTopology(gs, nil)
	if len(naked.Nodes) != len(topo.Nodes) {
		t.Errorf("nil palette lost nodes: %d vs %d", len(naked.Nodes), len(topo.Nodes))
	}
	for _, n := range naked.Nodes {
		if n.Known {
			t.Error("nil palette must mark nothing Known")
		}
	}
}

func TestDiffGraphs(t *testing.T) {
	a := chainGraph() // prepare → agent → deploy
	b := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "prepare", Type: "plugin", Label: "pr-fetch-v2"}, // changed label
			{ID: "deploy", Type: "plugin", Label: "post-review"},
			{ID: "notify", Type: "plugin", Label: "notify"}, // added
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "prepare", To: "deploy"}, // prepare→agent and agent→deploy removed
			{From: "deploy", To: "notify"},  // added
		},
	}
	nodeDelta, edgeDelta := diffGraphs(a, b)
	if nodeDelta["agent"] != diffRemoved {
		t.Errorf("agent = %q, want removed", nodeDelta["agent"])
	}
	if nodeDelta["notify"] != diffAdded {
		t.Errorf("notify = %q, want added", nodeDelta["notify"])
	}
	if nodeDelta["prepare"] != diffChanged {
		t.Errorf("prepare = %q, want changed", nodeDelta["prepare"])
	}
	if nodeDelta["deploy"] != "" {
		t.Errorf("deploy = %q, want no delta", nodeDelta["deploy"])
	}
	if edgeDelta["prepare→agent"] != diffRemoved || edgeDelta["agent→deploy"] != diffRemoved {
		t.Error("removed edges not classified")
	}
	if edgeDelta["deploy→notify"] != diffAdded {
		t.Error("added edge not classified")
	}
	if edgeDelta["prepare→deploy"] != diffAdded {
		t.Errorf("prepare→deploy exists only in b: got %q, want added", edgeDelta["prepare→deploy"])
	}
}

// Position is layout output, never a delta: same node spec in both graphs
// (even if the union layout would place it differently) is NOT changed.
func TestDiffGraphs_PositionIsNotDelta(t *testing.T) {
	a := chainGraph()
	b := chainGraph()
	nodeDelta, _ := diffGraphs(a, b)
	if len(nodeDelta) != 0 {
		t.Errorf("identical graphs have deltas: %v", nodeDelta)
	}
}

// The diff view projects both revisions onto ONE union layout: identical
// canvases, ghosts carry the delta seen from their pane.
func TestBuildTopologyDiff_UnionLayout(t *testing.T) {
	a := v1alpha1.GraphSpec{ // deterministic-only
		Nodes: specNodes(
			v1alpha1.NodeSpec{ID: "prepare", Type: "plugin", Label: "pr-fetch-stale"},
			v1alpha1.NodeSpec{ID: "deploy", Type: "plugin", Label: "post-review"},
		),
		Edges: []v1alpha1.EdgeSpec{{From: "prepare", To: "deploy"}},
	}
	b := chainGraph() // +agent, prepare relabeled

	older, newer := buildTopologyDiff(a, b, nil)
	if older.Width != newer.Width || older.Height != newer.Height {
		t.Errorf("panes must share the union canvas: %dx%d vs %dx%d",
			older.Width, older.Height, newer.Width, newer.Height)
	}

	olderByID := map[string]topologyNodeView{}
	for _, n := range older.Nodes {
		olderByID[n.ID] = n
	}
	newerByID := map[string]topologyNodeView{}
	for _, n := range newer.Nodes {
		newerByID[n.ID] = n
	}

	// agent: own+normal in newer, ghost+added in older
	if olderByID["agent"].Ghost != true || olderByID["agent"].DiffClass != diffAdded {
		t.Errorf("agent in older pane = ghost:%v class:%q, want ghost/added",
			olderByID["agent"].Ghost, olderByID["agent"].DiffClass)
	}
	if newerByID["agent"].Ghost || newerByID["agent"].DiffClass != diffAdded {
		t.Errorf("agent in newer pane must be own+added, got ghost:%v class:%q",
			newerByID["agent"].Ghost, newerByID["agent"].DiffClass)
	}
	// prepare changed in both panes (own in both)
	if olderByID["prepare"].DiffClass != diffChanged || newerByID["prepare"].DiffClass != diffChanged {
		t.Errorf("prepare must be changed in both panes, got %q/%q",
			olderByID["prepare"].DiffClass, newerByID["prepare"].DiffClass)
	}
	// identical geometry for shared nodes
	if olderByID["prepare"].X != newerByID["prepare"].X || olderByID["prepare"].Y != newerByID["prepare"].Y {
		t.Error("shared nodes must sit at identical anchors across panes")
	}
	// edges: prepare→deploy is own in older (no delta), ghost-added in newer? No —
	// prepare→deploy exists only in a: own in older, ghost+removed in newer.
	edgeOf := func(tv topologyView, k string) topologyEdgeView {
		for _, e := range tv.Edges {
			if e.From+"→"+e.To == k {
				return e
			}
		}
		return topologyEdgeView{From: "MISSING"}
	}
	// The delta class is marked ON the element in the pane where it lives:
	// prepare→deploy exists only in the older revision, so the older pane
	// carries it as own+removed (it vanishes by the newer).
	if oe := edgeOf(older, "prepare→deploy"); oe.DiffClass != diffRemoved || oe.Ghost {
		t.Errorf("older pane prepare→deploy = %+v, want own+removed", oe)
	}
	if ne := edgeOf(newer, "prepare→deploy"); !ne.Ghost || ne.DiffClass != diffRemoved {
		t.Errorf("newer pane prepare→deploy = %+v, want ghost/removed", ne)
	}
	if ne := edgeOf(newer, "prepare→agent"); ne.Ghost || ne.DiffClass != diffAdded {
		t.Errorf("newer pane prepare→agent = %+v, want own+added", ne)
	}
}

func TestTemplateRevisions(t *testing.T) {
	// No annotation: history is just the head.
	head := &v1alpha1.WorkflowTemplate{}
	revs := templateRevisions(head)
	if len(revs) != 1 || revs[0].Rev != 1 {
		t.Fatalf("headless history = %v", revs)
	}
	// Invalid annotation degrades the same way — never an error page.
	head.Annotations = map[string]string{RevisionsAnnotation: "{not json"}
	if revs := templateRevisions(head); len(revs) != 1 {
		t.Errorf("invalid annotation must degrade to head-only, got %d", len(revs))
	}
	// Valid annotation: ascending history + head appended with rev = n+1.
	head.Annotations = map[string]string{RevisionsAnnotation: `[
		{"rev":1,"description":"r1","spec":{"description":"old"}},
		{"rev":2,"description":"r2","spec":{"description":"mid"}}
	]`}
	revs = templateRevisions(head)
	if len(revs) != 3 {
		t.Fatalf("history = %d entries, want 3", len(revs))
	}
	if revs[0].Description != "r1" || revs[1].Description != "r2" || revs[2].Rev != 3 {
		t.Errorf("history misordered: %+v", revs)
	}
	if revs[2].Spec.Description != head.Spec.Description {
		t.Error("head entry must carry the live spec")
	}
}

func TestLineDiff(t *testing.T) {
	d := lineDiff("a: 1\nb: 2\nc: 3\n", "a: 1\nb: 22\nd: 4\n")
	kinds := map[string]int{}
	for _, l := range d {
		kinds[string(l.Kind)]++
	}
	if kinds["same"] != 1 || kinds["removed"] != 2 || kinds["added"] != 2 {
		t.Errorf("diff kinds = %v, want same=1 removed=2 added=2\n%+v", kinds, d)
	}
	// The changed b line: removed then added, adjacent.
	var seq []string
	for _, l := range d {
		if strings.HasPrefix(l.Text, "b:") {
			seq = append(seq, string(l.Kind))
		}
	}
	if len(seq) != 2 || seq[0] != "removed" || seq[1] != "added" {
		t.Errorf("b line sequence = %v, want removed,added", seq)
	}
	if got := lineDiff("", ""); len(got) != 0 {
		t.Errorf("empty documents diff to nothing, got %d lines", len(got))
	}
}
