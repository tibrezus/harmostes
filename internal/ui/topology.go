package ui

// topology.go — the topology projection (ADR-0012 §3, issue #417): the same
// GraphSpec the run graph renders, projected for authoring. Read-only and
// auto-layouted; the layout engine is shared with the run graph (one
// geometry function, two paint jobs — the run graph paints execution state,
// the topology paints structure and, on the revisions view, deltas).
//
// ADR-0012 §3 named Cytoscape+dagre because the SPA asset was assumed to
// carry over; the console rewrite (#413) replaced the SPA with server-
// rendered pages, and the projection now extends the run graph's own SVG
// renderer — same intent, one fewer renderer. The drag canvas stays
// explicitly deferred (ADR-0012 §3).
//
// Revisions: a WorkflowTemplate's history lives in the
// `template.harmostes.dev/revisions` annotation until git-history lands with
// the MR-bridge (#420). Ascending JSON:
// [{"rev":1,"description":"…","spec":{…}},…] — the CR's live spec is the
// head revision.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// revisionsAnnotation carries a template's prior specs on the CR until the
// The revision history is annotation-carried, but no longer interim-stamped
// by hand: the template-history recorder (controller, #420) appends an entry
// whenever Flux delivers a spec change — git lands, the CR records it. The
// annotation name and entry type live on the api package (one contract for
// the controller's writer and this reader).
const RevisionsAnnotation = v1alpha1.TemplateRevisionsAnnotation

// topologyNodeView is one node of the topology projection. Known reports
// whether the node's type is in the CRD schema's vocabulary (the palette is
// schema-derived); DiffClass is set only on the revisions view
// ("" | added | removed | changed); Ghost marks a node that exists only in
// the other revision — kept in geometry, dimmed, so both panes share one
// union shape and the eye can trace a delta across the gutter.
type topologyNodeView struct {
	ID        string
	Label     string
	Type      string
	Known     bool
	X         int
	Y         int
	LabelX    int
	LabelY    int
	TypeX     int
	TypeY     int
	DiffClass string
	Ghost     bool
}

type topologyEdgeView struct {
	From      string
	To        string
	Path      string
	DiffClass string
	Ghost     bool
}

type topologyView struct {
	Available bool
	Reason    string
	Width     int
	Height    int
	Nodes     []topologyNodeView
	Edges     []topologyEdgeView
	// NodeLinks (#419): optional per-node hrefs — the template authoring
	// view links each node to its inspector panel. Nil elsewhere (frag
	// renders plain nodes).
	NodeLinks map[string]string
}

// graphGeometry is the pure output of the shared layout engine: node grid
// positions by ID, edge bezier paths, canvas size, and the column
// partition (topological order, for consumers that scan it — the run
// graph's live-position walk).
type graphGeometry struct {
	pos     map[string][2]int
	edgeOf  map[string]string // "from→to" → path
	order   []string          // node IDs in render order
	columns [][]string
	width   int
	height  int
}

// layoutGraphGeometry is the layout engine both projections share: layered
// columns by longest-path depth, deterministic rows by ID — identical input
// ⇒ identical geometry, on every render and in both diff panes.
func layoutGraphGeometry(nodes []v1alpha1.NodeSpec, rawEdges []v1alpha1.EdgeSpec) graphGeometry {
	sorted := make([]v1alpha1.NodeSpec, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	idSet := make(map[string]bool, len(sorted))
	for _, n := range sorted {
		idSet[n.ID] = true
	}
	edges := make([]v1alpha1.EdgeSpec, 0, len(rawEdges))
	preds := map[string][]string{}
	for _, e := range rawEdges {
		if idSet[e.From] && idSet[e.To] {
			edges = append(edges, e)
			preds[e.To] = append(preds[e.To], e.From)
		}
	}

	depths := make(map[string]int, len(sorted))
	var depth func(id string) int
	depth = func(id string) int {
		if d, ok := depths[id]; ok {
			return d
		}
		depths[id] = 0 // cycle guard (compiled graphs are DAGs)
		d := 0
		for _, p := range preds[id] {
			if pd := depth(p) + 1; pd > d {
				d = pd
			}
		}
		depths[id] = d
		return d
	}
	maxDepth := 0
	for _, n := range sorted {
		if d := depth(n.ID); d > maxDepth {
			maxDepth = d
		}
	}

	columns := make([][]string, maxDepth+1)
	for _, n := range sorted {
		columns[depths[n.ID]] = append(columns[depths[n.ID]], n.ID)
	}
	pos := map[string][2]int{}
	rows := 0
	for c, col := range columns {
		if len(col) > rows {
			rows = len(col)
		}
		for r, id := range col {
			pos[id] = [2]int{c, r}
		}
	}

	edgeOf := map[string]string{}
	for _, e := range edges {
		x1 := graphMargin + pos[e.From][0]*(graphNodeW+graphColGap) + graphNodeW
		y1 := graphMargin + pos[e.From][1]*(graphNodeH+graphRowGap) + graphNodeH/2
		x2 := graphMargin + pos[e.To][0]*(graphNodeW+graphColGap)
		y2 := graphMargin + pos[e.To][1]*(graphNodeH+graphRowGap) + graphNodeH/2
		mid := (x1 + x2) / 2
		edgeOf[e.From+"→"+e.To] = fmt.Sprintf("M %d %d C %d %d, %d %d, %d %d", x1, y1, mid, y1, mid, y2, x2, y2)
	}

	return graphGeometry{
		pos:     pos,
		edgeOf:  edgeOf,
		order:   sortedIDs(sorted),
		columns: columns,
		width:   graphMargin*2 + (maxDepth+1)*graphNodeW + maxDepth*graphColGap,
		height:  graphMargin*2 + rows*graphNodeH + (rows-1)*graphRowGap,
	}
}

func sortedIDs(nodes []v1alpha1.NodeSpec) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

// buildTopology projects a GraphSpec into render-ready views. palette is the
// schema-derived node-type vocabulary (nil ⇒ every node renders generic).
func buildTopology(gs v1alpha1.GraphSpec, palette map[string]bool) topologyView {
	sorted := make([]v1alpha1.NodeSpec, len(gs.Nodes))
	copy(sorted, gs.Nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	geo := layoutGraphGeometry(sorted, gs.Edges)
	specOf := map[string]v1alpha1.NodeSpec{}
	for _, n := range sorted {
		specOf[n.ID] = n
	}
	nodes := make([]topologyNodeView, 0, len(sorted))
	for _, id := range geo.order {
		n := specOf[id]
		x := graphMargin + geo.pos[id][0]*(graphNodeW+graphColGap)
		y := graphMargin + geo.pos[id][1]*(graphNodeH+graphRowGap)
		label := n.Label
		if label == "" {
			label = n.ID
		}
		nodes = append(nodes, topologyNodeView{
			ID:     id,
			Label:  truncateRunes(label, graphLabelLimit),
			Type:   n.Type,
			Known:  palette[n.Type],
			X:      x,
			Y:      y,
			LabelX: x + 30,
			LabelY: y + 25,
			TypeX:  x + graphNodeW - 8,
			TypeY:  y + 25,
		})
	}
	edges := make([]topologyEdgeView, 0, len(geo.edgeOf))
	for _, e := range gs.Edges {
		k := e.From + "→" + e.To
		if p, ok := geo.edgeOf[k]; ok {
			edges = append(edges, topologyEdgeView{From: e.From, To: e.To, Path: p})
		}
	}
	return topologyView{Available: true, Width: geo.width, Height: geo.height, Nodes: nodes, Edges: edges}
}

// Diff vocabulary of the revisions view. Node: added (only in the newer) /
// removed (only in the older) / changed (same ID, different config). Edge:
// added / removed.
const (
	diffAdded   = "added"
	diffRemoved = "removed"
	diffChanged = "changed"
)

// buildTopologyDiff projects two graph specs onto ONE union layout and
// returns the two panes (older, newer) with delta classes applied — the
// Windmill two-auto-layouts pattern, aligned: identical geometry both sides
// so a delta is the only visual difference between the panes.
func buildTopologyDiff(a, b v1alpha1.GraphSpec, palette map[string]bool) (older, newer topologyView) {
	nodeDelta, edgeDelta := diffGraphs(a, b)

	// Union graph: prefer the NEWER spec's node/edge (its label/config wins
	// for display); layout once, paint twice.
	unionNodes := map[string]v1alpha1.NodeSpec{}
	for _, n := range a.Nodes {
		unionNodes[n.ID] = n
	}
	for _, n := range b.Nodes {
		unionNodes[n.ID] = n
	}
	unionEdges := map[string]v1alpha1.EdgeSpec{}
	for _, e := range a.Edges {
		unionEdges[e.From+"→"+e.To] = e
	}
	for _, e := range b.Edges {
		unionEdges[e.From+"→"+e.To] = e
	}
	ids := make([]string, 0, len(unionNodes))
	for id := range unionNodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	gs := v1alpha1.GraphSpec{}
	for _, id := range ids {
		gs.Nodes = append(gs.Nodes, unionNodes[id])
	}
	edgeKeys := make([]string, 0, len(unionEdges))
	for k := range unionEdges {
		edgeKeys = append(edgeKeys, k)
	}
	sort.Strings(edgeKeys)
	for _, k := range edgeKeys {
		gs.Edges = append(gs.Edges, unionEdges[k])
	}

	geo := layoutGraphGeometry(gs.Nodes, gs.Edges)
	nodeIn := func(gs v1alpha1.GraphSpec) map[string]bool {
		m := map[string]bool{}
		for _, n := range gs.Nodes {
			m[n.ID] = true
		}
		return m
	}
	edgeIn := func(gs v1alpha1.GraphSpec) map[string]bool {
		m := map[string]bool{}
		for _, e := range gs.Edges {
			m[e.From+"→"+e.To] = true
		}
		return m
	}
	inA, edgeInA := nodeIn(a), edgeIn(a)
	inB, edgeInB := nodeIn(b), edgeIn(b)

	paint := func(own map[string]bool, ownEdges map[string]bool, ghostClass string) topologyView {
		tv := topologyView{Available: true, Width: geo.width, Height: geo.height}
		for _, id := range geo.order {
			n := unionNodes[id]
			x := graphMargin + geo.pos[id][0]*(graphNodeW+graphColGap)
			y := graphMargin + geo.pos[id][1]*(graphNodeH+graphRowGap)
			label := n.Label
			if label == "" {
				label = n.ID
			}
			v := topologyNodeView{
				ID: id, Label: truncateRunes(label, graphLabelLimit), Type: n.Type,
				Known: palette[n.Type],
				X:     x, Y: y, LabelX: x + 30, LabelY: y + 25, TypeX: x + graphNodeW - 8, TypeY: y + 25,
			}
			switch {
			case own[id]:
				// Present in this revision. When it also exists in the other,
				// nodeDelta says "" or changed; when it exists only here, the
				// delta is mine (added in the newer pane, removed in the older).
				v.DiffClass = nodeDelta[id]
			default:
				// Present only in the other revision: ghost in this pane,
				// painted with the delta seen from the comparison direction.
				v.Ghost, v.DiffClass = true, ghostClass
			}
			tv.Nodes = append(tv.Nodes, v)
		}
		for _, k := range edgeKeys {
			e := unionEdges[k]
			v := topologyEdgeView{From: e.From, To: e.To, Path: geo.edgeOf[k]}
			if ownEdges[k] {
				v.DiffClass = edgeDelta[k]
				tv.Edges = append(tv.Edges, v)
				continue
			}
			if edgeInA[k] || edgeInB[k] { // exists in the other revision only
				v.Ghost, v.DiffClass = true, ghostClass
				tv.Edges = append(tv.Edges, v)
			}
		}
		return tv
	}
	older = paint(inA, edgeInA, diffAdded)   // nodes only in b ghost here as "added"
	newer = paint(inB, edgeInB, diffRemoved) // nodes only in a ghost here as "removed"
	return older, newer
}

// diffGraphs classifies nodes and edges between two graph specs by ID.
// A node is "changed" when its full spec (type, label, config) differs —
// position is layout output, never a delta.
func diffGraphs(a, b v1alpha1.GraphSpec) (nodeDelta, edgeDelta map[string]string) {
	nodeDelta, edgeDelta = map[string]string{}, map[string]string{}
	nodes := func(gs v1alpha1.GraphSpec) map[string]v1alpha1.NodeSpec {
		m := map[string]v1alpha1.NodeSpec{}
		for _, n := range gs.Nodes {
			m[n.ID] = n
		}
		return m
	}
	an, bn := nodes(a), nodes(b)
	for id, n := range an {
		if _, ok := bn[id]; !ok {
			nodeDelta[id] = diffRemoved
		} else if nodeFingerprint(n) != nodeFingerprint(bn[id]) {
			nodeDelta[id] = diffChanged
		}
	}
	for id := range bn {
		if _, ok := an[id]; !ok {
			nodeDelta[id] = diffAdded
		}
	}
	edges := func(gs v1alpha1.GraphSpec) map[string]bool {
		m := map[string]bool{}
		for _, e := range gs.Edges {
			m[e.From+"→"+e.To] = true
		}
		return m
	}
	ae, be := edges(a), edges(b)
	for k := range ae {
		if !be[k] {
			edgeDelta[k] = diffRemoved
		}
	}
	for k := range be {
		if !ae[k] {
			edgeDelta[k] = diffAdded
		}
	}
	return nodeDelta, edgeDelta
}

// nodeFingerprint is the structure+config identity of a node — everything
// except layout position (which the engine derives).
func nodeFingerprint(n v1alpha1.NodeSpec) string {
	b, _ := json.Marshal(struct {
		ID     string          `json:"id"`
		Type   string          `json:"type"`
		Label  string          `json:"label"`
		Config json.RawMessage `json:"config"`
	}{n.ID, n.Type, n.Label, n.Config})
	return string(b)
}

// templateRevisions returns the template's revision history ascending, with
// the CR's live spec as the head entry. A missing/invalid annotation is not
// an error: the history is just the head.
func templateRevisions(tmpl *v1alpha1.WorkflowTemplate) []v1alpha1.TemplateRevision {
	revs := []v1alpha1.TemplateRevision{}
	if raw := tmpl.Annotations[RevisionsAnnotation]; raw != "" {
		var parsed []v1alpha1.TemplateRevision
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			revs = parsed
		}
	}
	revs = append(revs, v1alpha1.TemplateRevision{
		Rev:  len(revs) + 1,
		Spec: tmpl.Spec,
	})
	return revs
}

// graphForTemplate compiles a template spec (or revision spec) to a graph —
// one compiler, one defaults policy (graph.CompileTemplate's contract).
func graphForTemplate(spec v1alpha1.WorkflowTemplateSpec) v1alpha1.GraphSpec {
	return graph.CompileTemplate(&v1alpha1.WorkflowTemplate{Spec: spec})
}

// graphForWorkflow yields the workflow's graph: explicit spec.graph when
// present, else the compiled declarative shape. Thin instances pass their
// RESOLVED spec — the merged shape is what runs.
func graphForWorkflow(wf *v1alpha1.Workflow) v1alpha1.GraphSpec {
	if wf.Spec.Graph != nil {
		return *wf.Spec.Graph
	}
	return graph.CompileWorkflow(wf)
}

// nodeTypePalette reads the graph node-type vocabulary from the workflow
// CRD's OpenAPI schema (spec.graph.nodes.type.enum) — the palette is
// schema-derived, never hand-written (ADR-0012 §2). Schema unreachable ⇒
// nil: every node renders generic, degraded but honest.
func (s *Server) nodeTypePalette(ctx context.Context) map[string]bool {
	schema, _, err := s.crdOpenAPISchema(ctx, workflowsCRDName)
	if err != nil {
		return nil
	}
	spec := schema.Properties["spec"]
	gh := spec.Properties["graph"]
	nodes := gh.Properties["nodes"]
	if nodes.Items == nil || nodes.Items.Schema == nil {
		return nil
	}
	enum := nodes.Items.Schema.Properties["type"].Enum
	if len(enum) == 0 {
		return nil
	}
	palette := map[string]bool{}
	for _, v := range enum {
		var name string
		if err := json.Unmarshal(v.Raw, &name); err == nil && name != "" {
			palette[name] = true
		}
	}
	return palette
}

// templateRevisionsView is the page model for the revision graph diff.
type templateRevisionsView struct {
	Name           string
	Description    string
	RevisionCount  int
	Revs           []int // selectable revision numbers, ascending
	From           int
	To             int
	FromDesc       string
	ToDesc         string
	TopologyOlder  topologyView
	TopologyNewer  topologyView
	YAMLDiff       []diffLine
	SingleRevision bool
}

// handleTemplateRevisions renders the revision graph diff (ADR-0012 §3):
// two paintings of one union layout with delta classes + a YAML diff pane.
// Query ?from=N&to=M (default: latest historical revision vs the live head).
//
// Route: GET /templates/{name}/revisions
func (s *Server) handleTemplateRevisions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.renderError(w, r, "template name required")
		return
	}
	tmpl := &v1alpha1.WorkflowTemplate{}
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, tmpl); err != nil {
		s.renderError(w, r, "Failed to get template: "+err.Error())
		return
	}

	revs := templateRevisions(tmpl)
	if len(revs) < 2 {
		// History-less template: the page still renders — the head topology
		// twice, a notice instead of a diff — so the route is never a dead
		// end and e2e can pin the empty state.
		data := templateRevisionsView{
			Name: name, Description: tmpl.Spec.Description, RevisionCount: len(revs),
			From: 1, To: 1, Revs: []int{1}, SingleRevision: true,
		}
		palette := s.nodeTypePalette(r.Context())
		data.TopologyOlder = buildTopology(graphForTemplate(tmpl.Spec), palette)
		data.TopologyNewer = data.TopologyOlder
		s.render(w, r, "pages/template_revisions.html", data)
		return
	}
	from, to := len(revs)-1, len(revs)
	if n, ok := parseRevParam(r.URL.Query().Get("from"), len(revs)); ok {
		from = n
	}
	if n, ok := parseRevParam(r.URL.Query().Get("to"), len(revs)); ok {
		to = n
	}
	if from >= to {
		from = to - 1 // a diff needs an ordered pair
		if from < 1 {
			from = 1
		}
	}

	a, b := revs[from-1], revs[to-1]
	palette := s.nodeTypePalette(r.Context())
	older, newer := buildTopologyDiff(graphForTemplate(a.Spec), graphForTemplate(b.Spec), palette)

	data := templateRevisionsView{
		Name:           name,
		Description:    tmpl.Spec.Description,
		RevisionCount:  len(revs),
		Revs:           revNumbers(len(revs)),
		From:           from,
		To:             to,
		FromDesc:       a.Description,
		ToDesc:         b.Description,
		TopologyOlder:  older,
		TopologyNewer:  newer,
		YAMLDiff:       lineDiff(templateSpecYAML(a.Spec), templateSpecYAML(b.Spec)),
		SingleRevision: len(revs) < 2,
	}
	s.render(w, r, "pages/template_revisions.html", data)
}

func revNumbers(n int) []int {
	revs := make([]int, n)
	for i := range revs {
		revs[i] = i + 1
	}
	return revs
}

func parseRevParam(v string, max int) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// templateSpecYAML renders a template spec as YAML for the diff pane — the
// document projection of the Workflow Code view (identity elided: a
// revision diff is about the spec).
func templateSpecYAML(spec v1alpha1.WorkflowTemplateSpec) string {
	doc := struct {
		Spec v1alpha1.WorkflowTemplateSpec `json:"spec"`
	}{spec}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "# spec unrenderable: " + err.Error()
	}
	return string(b)
}
