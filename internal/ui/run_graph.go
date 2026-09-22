package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
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

// Geometry of the layered layout (server-side, deterministic). Cards are
// 224×92 identity cards (#541): type chip, headline, two fact lines.
const (
	graphNodeW      = 224
	graphNodeH      = 92
	graphColGap     = 56
	graphRowGap     = 20
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
	// Identity card (#541): what this node IS, on the canvas.
	Chip       string `json:"chip"`
	Title      string `json:"title"`
	Fact1      string `json:"fact1,omitempty"`
	Fact2      string `json:"fact2,omitempty"`
	StatusText string `json:"statusText"` // glyph + word (grayscale-safe)
	// Precomputed SVG anchors (the template stays arithmetic-free).
	cardAnchors
}

type graphEdgeView struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Path  string `json:"path"`  // SVG path data, right edge → left edge
	Cause bool   `json:"cause"` // dashed: the trigger's cause-edge (not data flow)
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
	TimingH   int                 `json:"timingH,omitempty"`
	TimingW   int                 `json:"timingW,omitempty"` // strip width; template viewBox reads this
}

// nodeData is what pointing at a node yields: the identity rows first
// (what this node IS — same facts as the card, spelled out), then the
// live facts, then artifact links. Identity/config split (#541): the
// click carries the config.
type nodeData struct {
	Status      string   `json:"status"`
	Defs        []defRow `json:"defs,omitempty"` // identity rows (from the card path)
	Summary     string   `json:"summary,omitempty"`
	RunID       string   `json:"runID,omitempty"`
	ProducedAt  string   `json:"producedAt,omitempty"`
	Duration    string   `json:"duration,omitempty"` // humanized node execution time
	Attempts    int      `json:"attempts,omitempty"` // >1: kernel retried a transient failure (ADR-0012 §9)
	Claims      int      `json:"claims,omitempty"`
	Refs        int      `json:"refs,omitempty"`
	TriggeredBy string   `json:"triggeredBy,omitempty"`
	RanWith     string   `json:"ranWith,omitempty"`    // window-resolved model (#494) from session meta
	SessionURL  string   `json:"sessionURL,omitempty"` // agent nodes: the transcript viewer
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
	// The trigger joins the canvas as a virtual root (#541): cause, not
	// step — the projection decorates the document with its source; the
	// compiled graph (what the worker walks) is untouched.
	gs = withTriggerNode(gs, resolved.Spec.Source)
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
	view.Nodes, view.Edges, view.Width, view.Height = layoutGraph(gs, &resolved.Spec, latest, inFlight)

	// The agent node's session link + pinned model (#494): the worker
	// resolves time-windowed models in-memory only, but session metadata
	// persists the run's actual model — the panel reports the truth, the
	// card reports the identity.
	lastRun := ""
	var lastAt time.Time
	for _, run := range att.Status.Runs {
		if run.StartedAt.After(lastAt) {
			lastAt, lastRun = run.StartedAt.Time, run.Name
		}
	}
	specOf := map[string]v1alpha1.NodeSpec{}
	for _, n := range gs.Nodes {
		specOf[n.ID] = n
	}
	for _, n := range view.Nodes {
		spec := specOf[n.ID]
		data := nodeData{Status: n.Status, Defs: cardDefs(spec, &resolved.Spec)}
		if n.ID == triggerNodeID && lastRun != "" {
			// The trigger card panel: the latest run's provenance is the
			// cause made concrete.
			data.Defs = append(data.Defs, defRow{Label: "Last run", Value: lastRun})
		}
		if env, ok := latest[n.ID]; ok {
			if n.Type == "agent" {
				// The agent's OWN run (the envelope's RunID — not the
				// lexicographic last, which on multi-node runs is whatever
				// sorts last, e.g. the prepare run).
				agentRun := env.RunID
				if agentRun == "" {
					agentRun = runForNode(att.Status.Runs, n.ID, lastRun)
				}
				data.SessionURL = "/runs/" + att.Name + "/runs/" + agentRun + "/session"
				if m := s.runSessionModel(ctx, att.Spec.WorkflowRef, agentRun); m != "" {
					data.RanWith = m
				}
			}
			data.Summary = env.Summary
			data.RunID = env.RunID
			data.ProducedAt = env.ProducedAt.Format("2006-01-02 15:04:05 MST") // matches the run rows above
			data.Duration = formatDuration(time.Duration(env.DurationMs) * time.Millisecond)
			data.Attempts = env.Attempt
			data.Claims = len(env.Claims)
			data.Refs = len(env.References)
			data.TriggeredBy = env.Provenance.TriggeredBy
		}
		view.NodeData[n.ID] = data
	}
	view.Timing = buildTimingStrip(view.Nodes, latest)
	view.TimingH = len(view.Timing) * 22 // lane height lives here; templates stay arithmetic-free
	view.TimingW = timingViewW
	return view
}

// timingSegment is one bar in the waterfall strip: a node's execution window
// (start = producedAt - duration, end = producedAt).
type timingSegment struct {
	Label  string `json:"label"`
	Status string `json:"status"` // segment color class (rg-state-*)
	X      int    `json:"x"`
	Y      int    `json:"y"` // lane offset (index * laneHeight), precomputed
	Width  int    `json:"width"`
	// Precomputed text anchors (templates stay arithmetic-free).
	TextX int  `json:"textX"`
	Right bool `json:"right"` // label sits right of the bar (short bars)
	// Anchor is the SVG text-anchor for the duration label: "end" when the
	// label flipped to the LEFT of a short bar that ends at the viewBox
	// edge (start-anchored text would clip past TimingW), empty for the
	// default start anchor.
	Anchor string `json:"anchor,omitempty"`
	Title  string `json:"title"` // humanized duration
}

// buildTimingStrip computes the per-step waterfall: one lane per node in
// graph order, bar width proportional to wall-clock share (#298). Nodes
// without envelopes are skipped (no timing known); an all-zero span degrades
// to an empty strip. There is deliberately NO overhead lane: with event-
// driven triggers (#557) the run starts when the conditions are met — there
// is no queue-wait phase to measure, and pre-#557 the lane's only real
// content was the dispatch latency the wake path deleted.
// timingViewW is the waterfall's fixed viewBox width in user units; the
// template reads it back through Graph.TimingW. One number, two consumers
// (bar math and the right-edge label flip) — keep it singular.
const timingViewW = 640

func buildTimingStrip(nodes []graphNodeView, latest map[string]v1alpha1.NodeResultEnvelope) []timingSegment {
	type lane struct {
		label, status string
		start, end    time.Time
		retries       int // envelope attempt count (>1: retried transient failure)
	}
	var lanes []lane

	// Filter to nodes with known timing, preserving graph order.
	ordered := make([]graphNodeView, 0, len(nodes))
	for _, n := range nodes {
		env, ok := latest[n.ID]
		if !ok || env.ProducedAt.IsZero() || n.Status == "external" {
			continue
		}
		ordered = append(ordered, n)
	}
	if len(ordered) == 0 {
		return nil
	}
	// Node lanes in graph order.
	for _, n := range ordered {
		env := latest[n.ID]
		lanes = append(lanes, lane{
			label:   n.Label,
			status:  n.Status,
			start:   env.ProducedAt.Add(-time.Duration(env.DurationMs) * time.Millisecond),
			end:     env.ProducedAt.Time,
			retries: env.Attempt,
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

	// segmentTitle is a waterfall bar's hover text: the humanized duration,
	// plus the retry count when the kernel had to retry a transient failure
	// (ADR-0012 §9) — the title is where a scanner looks first.
	segmentTitle := func(l lane) string {
		title := formatDuration(l.end.Sub(l.start))
		if l.retries > 1 {
			title += fmt.Sprintf(" · retry ×%d", l.retries)
		}
		return title
	}

	const barX, labelW = 110, 520
	// All-zero node durations (pre-#298 envelopes): nothing to proportion —
	// an empty strip is more honest than 3px floors implying distribution.
	timed := false
	for _, l := range lanes {
		if l.end.After(l.start) {
			timed = true
			break
		}
	}
	if !timed {
		return nil
	}
	segs := make([]timingSegment, 0, len(lanes))
	for _, l := range lanes {
		x := int(float64(l.start.Sub(spanStart)) / float64(total) * float64(labelW))
		w := int(float64(l.end.Sub(l.start)) / float64(total) * float64(labelW))
		if w < 3 {
			w = 3 // sub-pixel bars stay visible
		}
		segs = append(segs, timingSegment{
			Label:  truncateRunes(l.label, 12), // fits the 110px gutter
			Status: l.status,
			X:      barX + x,
			Width:  w,
			Title:  segmentTitle(l),
		})
	}
	// Short bars label to the right of the bar; long bars inside. A short
	// bar whose right-side label would run past the viewBox edge (the last
	// node to finish — its text, not its bar, is what clipped) flips to the
	// left of the bar, end-anchored: the lane left of a bar is always its
	// own empty gutter, so the flip can never collide.
	const laneH = 22
	for i := range segs {
		segs[i].Y = i * laneH // templates have no arithmetic: {{$i}}22 would concatenate
		segs[i].TextX = segs[i].X + segs[i].Width + 5
		segs[i].Right = segs[i].Width < 60
		if !segs[i].Right {
			segs[i].TextX = segs[i].X + 5
			continue
		}
		// ~6 viewBox units per glyph at the label's 10px monospace — a
		// deliberate over-estimate keeps the longest retry title inside.
		if segs[i].TextX+6*len(segs[i].Title) > timingViewW {
			segs[i].TextX = segs[i].X - 6
			segs[i].Anchor = "end"
		}
	}
	return segs
}

// layoutGraph computes the run graph's painting over the shared layout
// engine (topology.go): execution state per node, the pulsing live position,
// timing anchors. Geometry itself is identical to the topology projection.
func layoutGraph(gs v1alpha1.GraphSpec, spec *v1alpha1.WorkflowSpec, latest map[string]v1alpha1.NodeResultEnvelope, inFlight bool) ([]graphNodeView, []graphEdgeView, int, int) {
	// Deterministic node order.
	nodes := make([]v1alpha1.NodeSpec, len(gs.Nodes))
	copy(nodes, gs.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	geo := layoutGraphGeometry(nodes, gs.Edges)

	// Live position: first envelope-less executable node in topological
	// order (columns already are a topo partition; scan column by column).
	runningNode := ""
	if inFlight {
		for _, col := range geo.columns {
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
		c, r := geo.pos[n.ID][0], geo.pos[n.ID][1]
		status := graphStatePending
		label := n.Label
		if label == "" {
			label = n.ID
		}
		// External = never-executable classification: env kernels AND the
		// virtual trigger (cause, not step — it can never be pending or
		// live, and must not read as unsettled work).
		if n.Type == "external" || n.ID == triggerNodeID {
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
		card := cardFacts(n, spec) // identity from the compiled node, spec fallback
		view := graphNodeView{
			ID:     n.ID,
			Label:  truncateRunes(label, graphLabelLimit),
			Type:   n.Type,
			Status: status,
			X:      x,
			Y:      y,
			Chip:   card.Chip,
			Title:  card.Title,
			StatusText: map[string]string{
				graphStatePending:  "· pending",
				graphStateRunning:  "↻ running",
				graphStateOK:       "✓ ok",
				graphStateFailed:   "✗ failed",
				graphStateSkipped:  "− skipped",
				graphStateExternal: "◇ external",
			}[status],
			cardAnchors: cardAnchorsAt(x, y),
		}
		if len(card.Facts) > 0 {
			view.Fact1 = card.Facts[0]
		}
		if len(card.Facts) > 1 {
			view.Fact2 = card.Facts[1]
		}
		views = append(views, view)
	}

	// Edges in spec order (noise-filtered by the engine), paths from the
	// shared geometry — identical curves to the topology projection.
	edgeViews := make([]graphEdgeView, 0, len(gs.Edges))
	for _, e := range gs.Edges {
		if p, ok := geo.edgeOf[e.From+"→"+e.To]; ok {
			edgeViews = append(edgeViews, graphEdgeView{From: e.From, To: e.To, Path: p, Cause: e.From == triggerNodeID})
		}
	}
	return views, edgeViews, geo.width, geo.height
}

func isExternalNode(nodes []v1alpha1.NodeSpec, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			// External = never-executable: env kernels AND the virtual
			// trigger (cause, not step — it can never be the live position).
			return n.Type == "external" || n.ID == triggerNodeID
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

// runForNode picks the run matching a node when the envelope carries no
// RunID (graph-native fixtures name runs {prefix}-{node}); the lexical
// last run is the final fallback.
func runForNode(runs []v1alpha1.RunRecord, nodeID, fallback string) string {
	for _, r := range runs {
		if strings.HasSuffix(r.Name, "-"+nodeID) {
			return r.Name
		}
	}
	return fallback
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
	if !s.mayViewAttempt(att, identityFromContext(r.Context())) {
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
