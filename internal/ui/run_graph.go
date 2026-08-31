package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// ---------------------------------------------------------------------------
// The run-detail timeline graph: the attempt's compiled workflow graph with
// per-node state (durable envelopes), the live position (pulsing current
// node), and per-node data on hover/click. Server-rendered SVG; the fragment
// re-renders over SSE from the same event bus that feeds the wall.
// ---------------------------------------------------------------------------

// Node states rendered on the graph. Envelope status is ok|skipped|failed;
// "pending" = no envelope yet; "running" = the live position.
const (
	graphStatePending  = "pending"
	graphStateRunning  = "running"
	graphStateOK       = "ok"
	graphStateFailed   = "failed"
	graphStateSkipped  = "skipped"
	graphStateExternal = "external"

	runGraphEventName = "rungraph"
)

// Geometry of the layered layout (server-side, deterministic).
const (
	graphNodeW      = 168
	graphNodeH      = 40
	graphColGap     = 44
	graphRowGap     = 16
	graphMargin     = 12
	graphLabelLimit = 22
)

type graphNodeView struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Type   string `json:"type"`
	Status string `json:"status"` // pending|running|ok|failed|skipped|external
	X      int    `json:"x"`
	Y      int    `json:"y"`
	// Precomputed SVG anchors (the template stays arithmetic-free).
	PulseX int `json:"pulseX"`
	PulseY int `json:"pulseY"`
	LabelX int `json:"labelX"`
	LabelY int `json:"labelY"`
	TypeX  int `json:"typeX"`
	TypeY  int `json:"typeY"`
}

type graphEdgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
	Path string `json:"path"` // SVG path data, right edge → left edge
}

type runGraphView struct {
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"` // why not available
	Workflow  string              `json:"workflow"`
	Width     int                 `json:"width"`
	Height    int                 `json:"height"`
	Nodes     []graphNodeView     `json:"nodes"`
	Edges     []graphEdgeView     `json:"edges"`
	NodeData  map[string]nodeData `json:"nodeData"` // hover/click payload
}

// nodeData is what pointing at a node yields: the live facts for that node.
type nodeData struct {
	Status      string `json:"status"`
	Summary     string `json:"summary,omitempty"`
	RunID       string `json:"runID,omitempty"`
	ProducedAt  string `json:"producedAt,omitempty"`
	Claims      int    `json:"claims,omitempty"`
	Refs        int    `json:"refs,omitempty"`
	TriggeredBy string `json:"triggeredBy,omitempty"`
}

// buildRunGraph compiles the attempt's workflow and merges the attempt's
// durable node results into a renderable graph view. The workflow lookup is
// best-effort: a deleted spec degrades to a placeholder, never an error —
// the attempt and its envelopes remain the spine.
func (s *Server) buildRunGraph(ctx context.Context, att *v1alpha1.Attempt) runGraphView {
	view := runGraphView{Available: false, NodeData: map[string]nodeData{}}
	wfName := workflowCRName(att.Spec.WorkflowRef)
	view.Workflow = wfName

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: wfName}, &wf); err != nil {
		view.Reason = "workflow spec unavailable"
		return view
	}
	resolved := s.resolveWorkflow(ctx, &wf)
	var gs v1alpha1.GraphSpec
	if resolved.Spec.Graph != nil {
		gs = *resolved.Spec.Graph
	} else {
		gs = graph.CompileWorkflow(&resolved)
	}
	if len(gs.Nodes) == 0 {
		view.Reason = "workflow has no compiled graph"
		return view
	}

	// Latest envelope wins per node (multiple runs of one attempt record the
	// same node; ProducedAt orders them).
	latest := map[string]v1alpha1.NodeResultEnvelope{}
	for _, env := range att.Status.NodeResults {
		if cur, ok := latest[env.NodeID]; !ok || env.ProducedAt.After(cur.ProducedAt.Time) {
			latest[env.NodeID] = env
		}
	}

	// Live position: an in-flight run means the first envelope-less
	// executable node in topological order is (approximately) executing.
	// Durable, unambiguous across concurrent attempts of one workflow —
	// no event attribution needed; SSE only accelerates freshness.
	inFlight := false
	for _, run := range att.Status.Runs {
		if run.Phase == "running" {
			inFlight = true
			break
		}
	}

	view.Available = true
	view.Nodes, view.Edges, view.Width, view.Height = layoutGraph(gs, latest, inFlight)
	for _, n := range view.Nodes {
		if env, ok := latest[n.ID]; ok {
			view.NodeData[n.ID] = nodeData{
				Status:      env.Status,
				Summary:     env.Summary,
				RunID:       env.RunID,
				ProducedAt:  env.ProducedAt.Format("2006-01-02 15:04:05"),
				Claims:      len(env.Claims),
				Refs:        len(env.References),
				TriggeredBy: env.Provenance.TriggeredBy,
			}
		} else {
			view.NodeData[n.ID] = nodeData{Status: n.Status}
		}
	}
	return view
}

// layoutGraph computes the layered layout: columns by longest-path depth,
// deterministic order within columns, bezier edges right→left.
func layoutGraph(gs v1alpha1.GraphSpec, latest map[string]v1alpha1.NodeResultEnvelope, inFlight bool) ([]graphNodeView, []graphEdgeView, int, int) {
	// Deterministic node order.
	nodes := make([]v1alpha1.NodeSpec, len(gs.Nodes))
	copy(nodes, gs.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	idSet := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		idSet[n.ID] = true
	}
	// Edges reference only known nodes (spec noise guard).
	edges := make([]v1alpha1.EdgeSpec, 0, len(gs.Edges))
	preds := map[string][]string{}
	for _, e := range gs.Edges {
		if idSet[e.From] && idSet[e.To] {
			edges = append(edges, e)
			preds[e.To] = append(preds[e.To], e.From)
		}
	}

	// Longest-path depth (memoized DFS; compiled graphs are DAGs — the visit
	// cap guards against pathological specs).
	depths := make(map[string]int, len(nodes))
	var depth func(id string) int
	depth = func(id string) int {
		if d, ok := depths[id]; ok {
			return d
		}
		depths[id] = 0 // cycle guard
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
	for _, n := range nodes {
		if d := depth(n.ID); d > maxDepth {
			maxDepth = d
		}
	}

	// Columns, rows deterministic by ID order.
	columns := make([][]string, maxDepth+1)
	for _, n := range nodes {
		d := depths[n.ID]
		columns[d] = append(columns[d], n.ID)
	}
	pos := map[string][2]int{}
	for c, col := range columns {
		for r, id := range col {
			pos[id] = [2]int{c, r}
		}
	}
	coord := func(id string) (int, int) {
		cr := pos[id]
		return cr[0], cr[1]
	}

	// Live position: first envelope-less executable node in topological
	// order (columns already are a topo partition; scan column by column).
	runningNode := ""
	if inFlight {
		for _, col := range columns {
			for _, id := range col {
				if _, done := latest[id]; done {
					continue
				}
				if isExternalNode(nodes, id) {
					continue
				}
				runningNode = id
				break
			}
			if runningNode != "" {
				break
			}
		}
	}

	views := make([]graphNodeView, 0, len(nodes))
	for _, n := range nodes {
		c, r := coord(n.ID)
		status := graphStatePending
		label := n.Label
		if label == "" {
			label = n.ID
		}
		if n.Type == "external" {
			status = graphStateExternal
		}
		if env, ok := latest[n.ID]; ok {
			switch env.Status {
			case "ok":
				status = graphStateOK
			case "skipped":
				status = graphStateSkipped
			default:
				status = graphStateFailed
			}
		}
		if n.ID == runningNode {
			status = graphStateRunning
		}
		x := graphMargin + c*(graphNodeW+graphColGap)
		y := graphMargin + r*(graphNodeH+graphRowGap)
		views = append(views, graphNodeView{
			ID:     n.ID,
			Label:  truncate(label, graphLabelLimit),
			Type:   n.Type,
			Status: status,
			X:      x,
			Y:      y,
			PulseX: x + 14,
			PulseY: y + graphNodeH/2,
			LabelX: x + 30,
			LabelY: y + 25,
			TypeX:  x + graphNodeW - 8,
			TypeY:  y + 25,
		})
	}

	edgeViews := make([]graphEdgeView, 0, len(edges))
	for _, e := range edges {
		fc, fr := coord(e.From)
		tc, tr := coord(e.To)
		x1 := graphMargin + fc*(graphNodeW+graphColGap) + graphNodeW
		y1 := graphMargin + fr*(graphNodeH+graphRowGap) + graphNodeH/2
		x2 := graphMargin + tc*(graphNodeW+graphColGap)
		y2 := graphMargin + tr*(graphNodeH+graphRowGap) + graphNodeH/2
		mid := (x1 + x2) / 2
		edgeViews = append(edgeViews, graphEdgeView{
			From: e.From,
			To:   e.To,
			Path: fmt.Sprintf("M %d %d C %d %d, %d %d, %d %d", x1, y1, mid, y1, mid, y2, x2, y2),
		})
	}

	width := graphMargin*2 + (maxDepth+1)*graphNodeW + maxDepth*graphColGap
	height := graphMargin*2 + maxRows(columns)*graphNodeH + (maxRows(columns)-1)*graphRowGap
	return views, edgeViews, width, height
}

func maxRows(columns [][]string) int {
	m := 1
	for _, c := range columns {
		if len(c) > m {
			m = len(c)
		}
	}
	return m
}

func isExternalNode(nodes []v1alpha1.NodeSpec, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return n.Type == "external"
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// handleRunGraphSSE streams the run-detail graph fragment for one attempt.
// Events for the attempt's workflow wake a re-render; the render reads the
// attempt's durable state fresh, so cross-attempt events are harmless noise.
func (s *Server) handleRunGraphSSE(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attemptOr404(w, r)
	if !ok {
		return
	}
	wfName := workflowCRName(att.Spec.WorkflowRef)
	render := func() (string, error) {
		fresh, err := s.getAttempt(r, att.Name)
		if err != nil {
			return "", err
		}
		return s.renderRunGraph(r, fresh)
	}
	sub, cancel := s.hub.Subscribe(wfName)
	s.streamFragments(w, r, sub, cancel, runGraphEventName, render, 0)
}

// attemptOr404 resolves the {name} path value to an owned attempt; unknown
// or foreign attempts are 404, never a 200 error page.
func (s *Server) attemptOr404(w http.ResponseWriter, r *http.Request) (*v1alpha1.Attempt, bool) {
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return nil, false
	}
	att, err := s.getAttempt(r, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			http.NotFound(w, r)
			return nil, false
		}
		s.renderError(w, r, "Failed to get attempt: "+err.Error())
		return nil, false
	}
	return att, true
}

// renderRunGraph renders the graph fragment (SVG + hover data) to a string.
func (s *Server) renderRunGraph(r *http.Request, att *v1alpha1.Attempt) (string, error) {
	view := s.buildRunGraph(r.Context(), att)
	dataJSON, err := json.Marshal(view.NodeData)
	if err != nil {
		return "", fmt.Errorf("marshal node data: %w", err)
	}
	return s.renderFragmentString("pages/frag_run_graph.html", map[string]any{
		"Graph":        view,
		"NodeDataJSON": template.JS(dataJSON),
	})
}
