package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

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
	NodeData  map[string]nodeData `json:"nodeData"`         // hover/click payload
	Timing    []timingSegment     `json:"timing,omitempty"` // per-step waterfall (#298)
}

// nodeData is what pointing at a node yields: the live facts for that node.
type nodeData struct {
	Status      string `json:"status"`
	Summary     string `json:"summary,omitempty"`
	RunID       string `json:"runID,omitempty"`
	ProducedAt  string `json:"producedAt,omitempty"`
	Duration    string `json:"duration,omitempty"` // humanized node execution time
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
				ProducedAt:  env.ProducedAt.Format("2006-01-02 15:04:05 MST"),                 // matches the run rows above
				Duration:    formatDuration(time.Duration(env.DurationMs) * time.Millisecond), // naming.go helper
				Claims:      len(env.Claims),
				Refs:        len(env.References),
				TriggeredBy: env.Provenance.TriggeredBy,
			}
		} else {
			view.NodeData[n.ID] = nodeData{Status: n.Status}
		}
	}
	view.Timing = buildTimingStrip(att, view.Nodes, latest)
	return view
}

// timingSegment is one bar in the waterfall strip: a node's execution window
// (start = producedAt - duration, end = producedAt), or an overhead window
// (trigger→pod→first node) before the first bar.
type timingSegment struct {
	Label  string `json:"label"`
	Status string `json:"status"` // segment color class (rg-state-*)
	X      int    `json:"x"`
	Width  int    `json:"width"`
	// Precomputed text anchors (templates stay arithmetic-free).
	TextX int    `json:"textX"`
	Right bool   `json:"right"` // label sits right of the bar (short bars)
	Title string `json:"title"` // humanized duration
}

// buildTimingStrip computes the per-step waterfall: one lane per node in
// graph order, bar width proportional to wall-clock share, plus an overhead
// lane (attempt creation → first node start). Nodes without envelopes are
// skipped (no timing known); an all-zero span degrades to an empty strip.
func buildTimingStrip(att *v1alpha1.Attempt, nodes []graphNodeView, latest map[string]v1alpha1.NodeResultEnvelope) []timingSegment {
	type lane struct {
		label, status string
		start, end    time.Time
	}
	var lanes []lane

	// Overhead lane: attempt creation → earliest node start.
	earliest := time.Time{}
	ordered := make([]graphNodeView, 0, len(nodes))
	for _, n := range nodes {
		env, ok := latest[n.ID]
		if !ok || env.ProducedAt.IsZero() || n.Status == "external" {
			continue
		}
		ordered = append(ordered, n)
		start := env.ProducedAt.Add(-time.Duration(env.DurationMs) * time.Millisecond)
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
	}
	if len(ordered) == 0 || att.CreationTimestamp.IsZero() || earliest.IsZero() {
		return nil
	}
	if create := att.CreationTimestamp.Time; create.Before(earliest) {
		lanes = append(lanes, lane{label: "queue+pod", status: "overhead", start: create, end: earliest})
	}
	for _, n := range ordered {
		env := latest[n.ID]
		lanes = append(lanes, lane{
			label:  n.Label,
			status: n.Status,
			start:  env.ProducedAt.Add(-time.Duration(env.DurationMs) * time.Millisecond),
			end:    env.ProducedAt.Time,
		})
	}

	spanStart, spanEnd := lanes[0].start, lanes[0].end
	for _, l := range lanes {
		if l.start.Before(spanStart) {
			spanStart = l.start
		}
		if l.end.After(spanEnd) {
			spanEnd = l.end
		}
	}
	total := spanEnd.Sub(spanStart)
	if total <= 0 {
		return nil
	}

	const stripW, barX, labelW = 640, 110, 520
	segs := make([]timingSegment, 0, len(lanes))
	for _, l := range lanes {
		x := int(float64(l.start.Sub(spanStart)) / float64(total) * float64(labelW))
		w := int(float64(l.end.Sub(l.start)) / float64(total) * float64(labelW))
		if w < 3 {
			w = 3 // sub-pixel bars stay visible
		}
		if x+w > labelW {
			x = labelW - w
		}
		segs = append(segs, timingSegment{
			Label:  truncateRunes(l.label, 14),
			Status: l.status,
			X:      barX + x,
			Width:  w,
			Title:  formatDuration(l.end.Sub(l.start)),
		})
	}
	// Short bars label to the right of the bar; long bars inside.
	for i := range segs {
		segs[i].TextX = segs[i].X + segs[i].Width + 5
		segs[i].Right = segs[i].Width < 60
		if !segs[i].Right {
			segs[i].TextX = segs[i].X + 5
		}
	}
	return segs
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
		// Envelope state never overrides the external classification: external
		// nodes never execute, so any envelope they carry is conceptual and
		// must not paint them failed.
		if env, ok := latest[n.ID]; ok && status != graphStateExternal {
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
			Label:  truncateRunes(label, graphLabelLimit),
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

// truncateRunes truncates by runes (byte slicing corrupts multi-byte labels)
// and appends an ellipsis. Shared with the template funcMap.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}

// handleRunGraphSSE streams the run-detail graph fragment for one attempt.
// The subscription is scoped to the attempt: events carrying a different
// attempt's name are filtered at the hub and cost nothing. Legacy events
// without attribution (pre-attribution workers) wake conservatively — during
// a rolling deploy, correctness beats noise. A 15s ticker is the convergence
// net for the pipeline.completed-before-outcome-recorded race: the last
// event can re-render before the worker records the terminal run outcome,
// and without a ticker the stale pulse would never be re-examined.
func (s *Server) handleRunGraphSSE(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attemptOr404(w, r)
	if !ok {
		return
	}
	wfName := workflowCRName(att.Spec.WorkflowRef)
	// Terminal tracking feeds the engine's isDone probe: once the attempt is
	// terminal (or deleted — nothing left to converge on), the 15s ticker
	// stops so an idle open tab on a finished run costs nothing. Single-writer
	// (the stream's own select loop calls render), so a plain bool is safe.
	terminal := false
	render := func() (string, error) {
		fresh, err := s.getAttempt(r, att.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				terminal = true
			}
			return "", err
		}
		terminal = attemptSettled(fresh)
		return s.renderRunGraph(r, fresh)
	}
	sub, cancel := s.hub.SubscribeFilter(wfName, func(ev Event) bool {
		return ev.Attempt == "" || ev.Attempt == att.Name
	})
	s.streamFragments(w, r, sub, cancel, runGraphEventName, render, func() bool { return terminal }, 15*time.Second)
}

// attemptSettled reports whether nothing in the attempt is in flight: the
// spine for the graph's pulse (no running run = no live position to show).
func attemptSettled(att *v1alpha1.Attempt) bool {
	for _, run := range att.Status.Runs {
		if run.Phase == "running" {
			return false
		}
	}
	return true
}

// attemptOr404 resolves the {name} path value to an attempt the caller owns:
// unknown, foreign, and unowned attempts are indistinguishable 404s (no
// existence leak), never a 200 error page. The multi-tenant gate lives HERE
// so every attempt-scoped fragment/stream endpoint inherits it.
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
	if att.Labels[v1alpha1.OwnerLabel] != identityFromContext(r.Context()).Username {
		http.NotFound(w, r)
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
