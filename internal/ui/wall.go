package ui

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// ---------------------------------------------------------------------------
// The live wall (`/`) — the zero-click surface: what is running, in which
// workflow, where is it. Read the way the operator reads it (#554):
// template → workflow → subject. Groups are the ADR-0007 attempt rollups;
// agent metadata (model, tokens, turns) arrives on the lifecycle event bus
// and is cached per workflow; each workflow block carries its compiled
// shape as a step-timing strip of the latest attempt.
// ---------------------------------------------------------------------------

const (
	wallDebounce  = 500 * time.Millisecond // coalesce per-node event bursts
	wallRerender  = 30 * time.Second       // keep relative times fresh without events
	sseHeartbeat  = 15 * time.Second       // shared with the other SSE handlers
	wallMaxGroups = 12                     // density cap: a wall is a glance, not a list
	wallEventName = "wall"
	wallStripW    = 140 // step-timing strip width, px
)

// wallUngrouped is the section header for workflows without a templateRef
// (graph-native instances defined entirely in their CR).
const wallUngrouped = "other workflows"

// wallUsage is the cached agent metadata for one workflow. Populated from
// node.completed lifecycle events (which carry usage/model/turns since the
// agent executor publishes them); when the cache is cold it is partially
// hydrated from the durable `<workflow>:usage:last` state-store record —
// that record carries token totals and attempts but NOT model/turns, so
// those stay unknown until the next agent node completes.
type wallUsage struct {
	Model        string
	InputTokens  int
	OutputTokens int
	Turns        int
	Attempts     int
}

// wallGroup is one subject row: a PR (review class) or a scheduled subject,
// with the latest state and a single drill-down link. Rows live inside a
// workflow block (rowspan), which lives inside a template section.
type wallGroup struct {
	WorkflowRef  string
	Subject      string
	IsReview     bool
	Phase        string
	ClaimState   string
	HeadSHA      string
	Count        int
	LastActivity string // relative ("3m ago")
	LastRunURL   string
	Usage        *wallUsage
}

// wallStep is one segment of a workflow's step-timing strip: a compiled
// node painted proportional to its wall-clock share of the workflow's
// latest attempt (pending nodes as fixed slots — the shape stays visible
// before timing exists). Geometry precomputed; templates stay arithmetic-free.
type wallStep struct {
	ID     string
	Status string // rg-state-* paint class (ok|failed|skipped|running|pending)
	Title  string // "agent · 13.0m · ✓ ok"
	X      int
	Width  int
}

// wallWorkflow is one workflow block inside a template section: identity,
// the step-timing strip of its latest attempt, and its live subject rows
// (a review workflow tracks one row per PR).
type wallWorkflow struct {
	Name   string
	URL    string
	Strip  []wallStep
	StripW int
	Groups []wallGroup
	Last   string // newest subject activity (RFC3339, for ordering)
}

// wallSection groups the wall by the workflow's owning template — the
// operator reads the wall template-first (what class is live), then
// workflow, then subject (#554).
type wallSection struct {
	Name      string
	Workflows []wallWorkflow
}

// noteWallEvent updates the per-workflow agent metadata cache from a
// lifecycle event. Called from the Dapr event ingress; cheap and best-effort.
func (s *Server) noteWallEvent(ev Event) {
	if ev.Pipeline == "" || len(ev.Outputs) == 0 {
		return
	}
	usageRaw, ok := ev.Outputs["usage"]
	if !ok {
		return
	}
	u := &wallUsage{
		Attempts: jsonInt(ev.Outputs["attempts"]),
		Turns:    jsonInt(ev.Outputs["turns"]),
	}
	if m, ok := ev.Outputs["model"].(string); ok {
		u.Model = m
	}
	// The event bus JSON-roundtrips Outputs, so usage arrives as a generic map
	// keyed by agent.Usage's json tags.
	if m, ok := usageRaw.(map[string]any); ok {
		u.InputTokens = jsonInt(m["input_tokens"])
		u.OutputTokens = jsonInt(m["output_tokens"])
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	s.wallMu.Lock()
	s.wallMeta[ev.Pipeline] = u
	s.wallMu.Unlock()
}

// usageFor returns cached agent metadata for a workflow. When the event cache
// is cold (UI restart), hydrates once from the durable usage:last record —
// model/turns are event-only and stay unknown until the next agent node runs.
// hydrate=false (SSE re-renders) is cache-only, never touches the state store.
func (s *Server) usageFor(r *http.Request, workflow string, hydrate bool) *wallUsage {
	s.wallMu.Lock()
	u := s.wallMeta[workflow]
	s.wallMu.Unlock()
	if u != nil || !hydrate || s.dapr == nil {
		return u
	}
	var last struct {
		Input    int `json:"input"`
		Output   int `json:"output"`
		Attempts int `json:"attempts"`
	}
	found, err := s.dapr.GetStateFromStore(r.Context(), "statestore", workflow+":usage:last", &last)
	if err != nil || !found || (last.Input == 0 && last.Output == 0) {
		return nil
	}
	u = &wallUsage{InputTokens: last.Input, OutputTokens: last.Output, Attempts: last.Attempts}
	s.wallMu.Lock()
	s.wallMeta[workflow] = u
	s.wallMu.Unlock()
	return u
}

// handleWall renders the live wall page (the `/` surface).
func (s *Server) handleWall(w http.ResponseWriter, r *http.Request) {
	// Only match exact "/" — Go 1.22 mux matches subtree for "/", and the
	// wall must not serve (or 200-fallback) for dead routes like /map.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	owner := s.visibleOwner(identityFromContext(r.Context()))
	sections, overflow, err := s.wallSections(r, owner, true)
	if err != nil {
		s.renderError(w, r, "Failed to load wall: "+err.Error())
		return
	}
	s.render(w, r, "pages/wall.html", map[string]any{"Sections": sections, "Overflow": overflow})
}

// attemptActivity is an attempt's ordering timestamp (last run, else creation).
func attemptActivity(a *v1alpha1.Attempt) time.Time {
	if !a.Status.LastRunAt.IsZero() {
		return a.Status.LastRunAt.Time
	}
	return a.CreationTimestamp.Time
}

// wallSections organizes the wall the operator reads it: template →
// workflow → subject (#554). Subject rows are the existing groupAttempts
// rollups; they fold under their workflow, which carries the step-timing
// strip of its newest attempt. Row budget: wallMaxGroups subject rows
// across all sections — the overflow count feeds a single "+N more" line.
func (s *Server) wallSections(r *http.Request, owner string, hydrate bool) ([]wallSection, int, error) {
	attempts, err := s.listAttempts(r, owner)
	if err != nil {
		return nil, 0, fmt.Errorf("list attempts: %w", err)
	}
	groups := groupAttempts(attempts, time.Time{})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LastActivity > groups[j].LastActivity
	})

	// Newest full attempt per workflow: the strip's envelope source.
	newest := map[string]*v1alpha1.Attempt{}
	for i := range attempts {
		a := &attempts[i]
		name := workflowCRName(a.Spec.WorkflowRef)
		if cur, ok := newest[name]; !ok || attemptActivity(a).After(attemptActivity(cur)) {
			newest[name] = a
		}
	}

	// Template membership + compiled-shape source. Owner-scoped, same as
	// the attempts — a workflow the viewer cannot see contributes nothing.
	wfs, err := s.listWorkflows(r, owner)
	if err != nil {
		return nil, 0, fmt.Errorf("list workflows: %w", err)
	}
	tmplOf := map[string]string{}
	wfByName := map[string]v1alpha1.Workflow{}
	for _, wf := range wfs {
		tmplOf[wf.Name] = wf.Spec.TemplateRef
		wfByName[wf.Name] = wf
	}

	// Fold subject groups under their workflow, honoring the row budget.
	wfOrder := []string{}
	wfGroups := map[string][]wallGroup{}
	rows := 0
	for _, g := range groups {
		if rows >= wallMaxGroups {
			break
		}
		name := workflowCRName(g.WorkflowRef)
		wg := wallGroup{
			WorkflowRef:  name,
			Subject:      g.Subject,
			IsReview:     g.IsReview,
			Phase:        g.LatestPhase,
			ClaimState:   g.ClaimState,
			HeadSHA:      shortSHA(g.HeadSHA),
			Count:        g.Count,
			LastActivity: relTime(g.LastActivity, time.Now()),
		}
		if g.LatestAttempt != "" {
			wg.LastRunURL = "/runs/" + g.LatestAttempt
		}
		// The usage cache and the durable record key on the bare CR name.
		wg.Usage = s.usageFor(r, name, hydrate)
		if _, seen := wfGroups[name]; !seen {
			wfOrder = append(wfOrder, name)
		}
		wfGroups[name] = append(wfGroups[name], wg)
		rows++
	}
	overflow := len(groups) - rows

	secOrder := []string{}
	secs := map[string]*wallSection{}
	for _, name := range wfOrder {
		ww := wallWorkflow{Name: name, URL: "/workflows/" + name, Groups: wfGroups[name], Last: wfGroups[name][0].LastActivity}
		if att := newest[name]; att != nil {
			if wf, ok := wfByName[name]; ok {
				ww.Strip, ww.StripW = s.wallStepsFor(r.Context(), &wf, att)
			}
		}
		sec := tmplOf[name]
		if sec == "" {
			sec = wallUngrouped
		}
		if _, ok := secs[sec]; !ok {
			secs[sec] = &wallSection{Name: sec}
			secOrder = append(secOrder, sec)
		}
		secs[sec].Workflows = append(secs[sec].Workflows, ww)
	}
	out := make([]wallSection, 0, len(secOrder))
	for _, name := range secOrder {
		out = append(out, *secs[name])
	}
	return out, overflow, nil
}

// renderWallFragment renders the wall grid to a string for SSE delivery.
func (s *Server) renderWallFragment(r *http.Request, owner string) (string, error) {
	sections, overflow, err := s.wallSections(r, owner, false)
	if err != nil {
		return "", err
	}
	tmpl := s.templates.Lookup("pages/frag_wall.html")
	if tmpl == nil {
		return "", fmt.Errorf("template not found: pages/frag_wall.html")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Sections": sections, "Overflow": overflow}); err != nil {
		return "", fmt.Errorf("render wall fragment: %w", err)
	}
	return buf.String(), nil
}

// wallStepsFor projects one workflow's latest attempt into step-timing
// segments: the compiled shape (template defaults applied — a thin
// instance shows its template's real structure), statuses from the same
// classifier the run-detail canvas uses, widths from the envelopes.
func (s *Server) wallStepsFor(ctx context.Context, wf *v1alpha1.Workflow, att *v1alpha1.Attempt) ([]wallStep, int) {
	resolved := s.resolveWorkflow(ctx, wf)
	var gs v1alpha1.GraphSpec
	if resolved.Spec.Graph != nil {
		gs = *resolved.Spec.Graph
	} else {
		gs = graph.CompileWorkflow(&resolved)
	}
	if len(gs.Nodes) == 0 {
		return nil, 0
	}
	latest := map[string]v1alpha1.NodeResultEnvelope{}
	for _, env := range att.Status.NodeResults {
		if cur, ok := latest[env.NodeID]; !ok || env.ProducedAt.After(cur.ProducedAt.Time) {
			latest[env.NodeID] = env
		}
	}
	inFlight := false
	for _, run := range att.Status.Runs {
		if run.Phase == "running" {
			inFlight = true
			break
		}
	}
	views, _, _, _ := layoutGraph(gs, &resolved.Spec, latest, inFlight)
	return wallSteps(views, gs, latest, wallStripW)
}

// wallSteps paints the single-lane strip: dependency order left to right
// (the geometry's topo columns — the same order the canvas resolves
// dependencies in), segment width proportional to the node's wall-clock
// share. Envelope-less and zero-duration nodes render as fixed pending
// slots so the workflow's SHAPE is legible before any timing exists.
// External nodes (env kernels) are not steps — omitted.
func wallSteps(views []graphNodeView, gs v1alpha1.GraphSpec, latest map[string]v1alpha1.NodeResultEnvelope, width int) ([]wallStep, int) {
	nodes := make([]v1alpha1.NodeSpec, len(gs.Nodes))
	copy(nodes, gs.Nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	geo := layoutGraphGeometry(nodes, gs.Edges)

	meta := map[string]graphNodeView{}
	for _, v := range views {
		meta[v.ID] = v
	}
	type seg struct {
		view     graphNodeView
		durMs    int64
		pending  bool
		titledur string
	}
	var segs []seg
	for _, col := range geo.columns {
		for _, id := range col {
			v, ok := meta[id]
			if !ok || v.Status == graphStateExternal {
				continue // env kernels and the virtual trigger are not steps
			}
			s := seg{view: v}
			if env, ok := latest[id]; ok && env.DurationMs > 0 {
				s.durMs = env.DurationMs
				s.titledur = formatDuration(time.Duration(env.DurationMs) * time.Millisecond)
			} else {
				s.pending = true
			}
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return nil, 0
	}

	const gap = 2
	drawable := width - gap*(len(segs)-1)
	var timed int64
	pend := 0
	for _, s := range segs {
		if s.pending {
			pend++
		} else {
			timed += s.durMs
		}
	}
	// Width per pending slot: a bounded share of the strip when timing
	// exists (timing dominates — the question is "where did the time go"),
	// equal shares when nothing has run (the question is "what is the shape").
	pendEach := 0
	timedW := drawable
	if pend > 0 {
		if timed > 0 {
			pendEach = max(drawable*3/10/pend, 6)
		} else {
			pendEach = drawable / len(segs)
		}
		timedW = drawable - pendEach*pend
	}

	// Pass 1: proportional widths, then the floor-rounding remainder folds
	// into the last timed segment (or the plain last one when nothing has
	// run) so the svg tiles exactly to StripW.
	lastTimed := -1
	for i, s := range segs {
		if !s.pending {
			lastTimed = i
		}
	}
	widths := make([]int, len(segs))
	total := 0
	for i, s := range segs {
		if s.pending {
			widths[i] = pendEach
		} else {
			widths[i] = int(float64(s.durMs) / float64(timed) * float64(timedW))
			if widths[i] < 3 {
				widths[i] = 3 // sub-pixel bars stay visible
			}
		}
		total += widths[i]
	}
	fold := len(segs) - 1
	if lastTimed >= 0 {
		fold = lastTimed
	}
	widths[fold] = max(widths[fold]+drawable-total, 3)

	// Pass 2: positions + titles.
	steps := make([]wallStep, 0, len(segs))
	x := 0
	for i, s := range segs {
		w := widths[i]
		title := s.view.Label
		if s.titledur != "" {
			title += " · " + s.titledur
		}
		if s.view.StatusText != "" {
			title += " · " + s.view.StatusText
		}
		steps = append(steps, wallStep{ID: s.view.ID, Status: s.view.Status, Title: title, X: x, Width: w})
		x += w + gap
	}
	return steps, x - gap
}

// handleWallSSE streams the wall grid: re-rendered on every lifecycle event
// (debounced), plus a slow ticker that keeps relative ages fresh without
// events. Rendering is cache-only — never the state store.
func (s *Server) handleWallSSE(w http.ResponseWriter, r *http.Request) {
	owner := s.visibleOwner(identityFromContext(r.Context()))
	render := func() (string, error) { return s.renderWallFragment(r, owner) }
	sub, cancel := s.hub.Subscribe("")
	s.streamFragments(w, r, sub, cancel, wallEventName, render, nil, wallRerender)
}

// jsonInt extracts an int from a JSON-roundtripped value (numbers decode as
// float64 into any).
func jsonInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// shortSHA truncates a head SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// relTime renders an RFC3339 timestamp as a coarse relative age. Empty input
// (never ran) renders empty.
func relTime(rfc3339 string, now time.Time) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// wallState collapses a wall row to the shared console state vocabulary —
// the same chips the runs list speaks, so the whole UI reads as one system.
func wallState(g wallGroup) string {
	if g.IsReview {
		switch g.ClaimState {
		case claimDispatchLost:
			return "dispatch lost"
		case claimInFlight:
			return "in flight"
		case claimVerdict:
			return "verdict"
		case claimQueued, claimHorizon, claimExpired:
			return "queued"
		case claimSuperseded:
			return "superseded"
		}
		return "queued"
	}
	switch g.Phase {
	case "failed":
		return "failed"
	case "reconciling":
		return "reconciling"
	case "validated":
		return "validated"
	case "superseded":
		return "superseded"
	}
	return g.Phase
}
