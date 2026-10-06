package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
	"github.com/tibrezus/harmostes/internal/review"
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

	// wallVerdictGrace: how long a terminal verdict stays on the wall after
	// its last activity. The wall is LIVE — in-flight, queued, and
	// failed work always show; outcomes linger only briefly before Runs
	// owns them (kestra/temporal law: live is not history).
	wallVerdictGrace = time.Hour
)

// wallUngrouped is the section header for workflows without a templateRef
// (graph-native instances defined entirely in their CR).
const wallUngrouped = "other workflows"

// wallUsage is one subject's own agent usage, read from its latest
// attempt's envelope payload (#586 — the Attempt CR is the per-PR source of
// record). The old workflow-level cache (usage:last hydration) is gone: it
// painted one session's numbers onto every payload-less row of the
// workflow — six queued PRs showing identical tokens (user-reported).
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
	// Live columns (in-flight groups only): what is executing right now
	// and since when — the wall answers "where is it" without a click.
	CurrentNode string
	Elapsed     string
	// Live is the executing run's in-flight usage (the attempt's Progress
	// window), shown instead of the post-hoc envelope numbers.
	Live *wallLiveTokens
	// Hold is the parked claim's why + since (#user wall refactor): the
	// wall renders it as the queued row's second line — "ci pending at
	// head … — dispatch on green · waiting 26m". A bare "queued" chip
	// answered nothing; the gate knew the reason every sweep.
	Hold *wallHold
	// Cause qualifies the CHIP itself ("queued · waiting ci") — the
	// short gate vocabulary, first-glance; Hold.Note is the detail line.
	Cause string
	// Strip is THIS subject's own attempt progress (its steps, per-step
	// states, widths ∝ wall clock) — the progress bar lives on the row
	// whose progress it describes (owner direction: "state is the better
	// spot"), not aggregated at the workflow block.
	Strip  []wallStep
	StripW int
}

// wallState collapses a wall row
type wallHold struct {
	// Note is the gate's waiting reason, verbatim (CSS clamps the line;
	// server-side truncation ate the exit behind an ellipsis).
	Note string
	// Since is how long the claim has been parked ("26m", "3h", "2d").
	Since string
}

// wallLiveTokens is one in-flight usage sample for the wall's token column.
type wallLiveTokens struct {
	In    int
	Out   int
	Turns int
	Model string // the run's pinned model — the live row answers "which model", not the cache
}

// wallProgressFreshness bounds how long a Progress sample is trusted. A
// crashed run's last write lingers on the CR; the wall shows a dash rather
// than a lie. Generous: agent turns can run long (research, big reviews).
const wallProgressFreshness = 15 * time.Minute

// wallCounts is the wall header's state tally (kestra's execution tabs): a
// count per live state over the whole live selection — pre-budget, so the
// tally doesn't flicker when the row cap clips. Aged-out history is not
// counted: the wall counts LIVE work only.
type wallCounts struct {
	InFlight int // claim in flight / reconciling
	Queued   int
	Failed   int // failed + dispatch lost
	Verdict  int // verdict/validated within the grace window
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
	Groups []wallGroup
	Last   string // newest subject activity (RFC3339, for ordering)
	// Counts is the workflow-level state rollup, TEXT ONLY ("8 queued · 1
	// verdict") — the aggregate answer lives in words; every bar lives on
	// the row whose progress it describes.
	Counts []wallStateCount
}

// wallStateCount is one state's count in the workflow rollup line.
type wallStateCount struct {
	State string
	Count int
}

// wallSection groups the wall by the workflow's owning template — the
// operator reads the wall template-first (what class is live), then
// workflow, then subject (#554).
type wallSection struct {
	Name      string
	Workflows []wallWorkflow
}

// livePosition names the node currently executing on an in-flight attempt
// (the layout engine's running state — the same live position the run graph
// pulses) and when it started (the executing run's StartedAt), for the
// wall's Now column.
func livePosition(resolved *v1alpha1.Workflow, att *v1alpha1.Attempt) (string, string) {
	var gs v1alpha1.GraphSpec
	if resolved.Spec.Graph != nil {
		gs = *resolved.Spec.Graph
	} else {
		gs = graph.CompileWorkflow(resolved)
	}
	if len(gs.Nodes) == 0 {
		return "", ""
	}
	latest := map[string]v1alpha1.NodeResultEnvelope{}
	for _, env := range att.Status.NodeResults {
		if cur, ok := latest[env.NodeID]; !ok || env.ProducedAt.After(cur.ProducedAt.Time) {
			latest[env.NodeID] = env
		}
	}
	views, _, _, _ := layoutGraph(gs, &resolved.Spec, latest, true)
	node := ""
	for _, v := range views {
		if v.Status == graphStateRunning {
			node = v.Label
		}
	}
	since := time.Time{}
	for _, run := range att.Status.Runs {
		if run.Phase == "running" && run.StartedAt.After(since) {
			since = run.StartedAt.Time
		}
	}
	if node == "" || since.IsZero() {
		return node, ""
	}
	return node, relTime(since.Format(time.RFC3339), time.Now())
}

// usageFromAttempt extracts the agent node's structured usage from an
// attempt's envelope payloads (agent_executor stamps usage/model/turns into
// the payload). Nil when the attempt is unknown or predates payloads —
// callers fall back to the workflow-level cache.
func usageFromAttempt(att *v1alpha1.Attempt) *wallUsage {
	if att == nil {
		return nil
	}
	// Newest agent envelope wins (retries/multi-node runs: the last agent
	// execution carried the session to its end).
	var latest *v1alpha1.NodeResultEnvelope
	for i := range att.Status.NodeResults {
		env := &att.Status.NodeResults[i]
		if env.Payload == nil {
			continue
		}
		var p struct {
			Usage *struct {
				Input  int `json:"input"`
				Output int `json:"output"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(env.Payload, &p); err != nil || p.Usage == nil {
			continue
		}
		if latest == nil || env.ProducedAt.After(latest.ProducedAt.Time) {
			latest = env
		}
	}
	if latest == nil {
		return nil
	}
	var p struct {
		Usage struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"usage"`
		Model string `json:"model"`
		Turns int    `json:"turns"`
	}
	if err := json.Unmarshal(latest.Payload, &p); err != nil {
		return nil
	}
	return &wallUsage{Model: p.Model, InputTokens: p.Usage.Input, OutputTokens: p.Usage.Output, Turns: p.Turns}
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
	sections, overflow, counts, err := s.wallSections(r, owner)
	if err != nil {
		s.renderError(w, r, "Failed to load wall: "+err.Error())
		return
	}
	s.render(w, r, "pages/wall.html", map[string]any{"Sections": sections, "Overflow": overflow, "Counts": counts})
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
func (s *Server) wallSections(r *http.Request, owner string) ([]wallSection, int, wallCounts, error) {
	attempts, err := s.listAttempts(r, owner)
	if err != nil {
		return nil, 0, wallCounts{}, fmt.Errorf("list attempts: %w", err)
	}
	groups := groupAttempts(attempts, time.Time{})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LastActivity > groups[j].LastActivity
	})

	// Attempt index by CR name: per-subject usage reads the SUBJECT's own
	// latest attempt — the workflow-level usage cache (usage:last) aggregates
	// across subjects and sessions, which read as identical numbers on every
	// row (user-reported). The attempt's agent envelope payload is the
	// per-PR source of record.
	byName := map[string]*v1alpha1.Attempt{}
	for i := range attempts {
		a := &attempts[i]
		byName[a.Name] = a
	}

	// Template membership + compiled-shape source. Owner-scoped, same as
	// the attempts — a workflow the viewer cannot see contributes nothing.
	wfs, err := s.listWorkflows(r, owner)
	if err != nil {
		return nil, 0, wallCounts{}, fmt.Errorf("list workflows: %w", err)
	}
	tmplOf := map[string]string{}
	wfByName := map[string]v1alpha1.Workflow{}
	for _, wf := range wfs {
		tmplOf[wf.Name] = wf.Spec.TemplateRef
		wfByName[wf.Name] = wf
	}

	// Fold subject groups under their workflow, honoring the row budget.
	// LIVE selection: active work (in flight, queued, failed, dispatch
	// lost) always shows; terminal verdicts linger only wallVerdictGrace
	// past their last activity, then Runs owns them. Superseded never
	// shows — a replaced targeted state is pure history.
	wfOrder := []string{}
	wfGroups := map[string][]wallGroup{}
	resolvedCache := map[string]v1alpha1.Workflow{}
	rows := 0
	hidden := 0 // aged-out rows: history, not overflow
	counts := wallCounts{}
	for _, g := range groups {
		state := groupState(g)
		switch state {
		case "superseded":
			hidden++
			continue
		case "verdict", "validated":
			att := byName[g.LatestAttempt]
			if att != nil && time.Since(attemptActivity(att)) > wallVerdictGrace {
				hidden++
				continue
			}
		}
		// The tally counts the whole live selection — pre-budget, so the
		// header reads "what is live" even when the table clips.
		switch state {
		case "in flight", "reconciling":
			counts.InFlight++
		case "queued":
			counts.Queued++
		case "failed", "dispatch lost":
			counts.Failed++
		case "verdict", "validated":
			counts.Verdict++
		}
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
		if g.Subject == "" {
			// Attempts that never recorded their subject (torn writes, the
			// pre-#629 debris classes): the row stays honest — "unattributed"
			// — instead of a blank cell that reads as a rendering bug.
			wg.Subject = "unattributed"
		}
		if g.LatestAttempt != "" {
			wg.LastRunURL = "/runs/" + g.LatestAttempt
		}
		// Per-subject usage: the subject's OWN latest attempt's agent
		// envelope — full stop. The workflow-level cache is a fallback NO
		// MORE: it carried the workflow's LAST SESSION to every payload-less
		// row, so six queued PRs showed one PR's numbers (user-reported
		// identical tokens). A queued row with no payload of its own shows
		// an em-dash — honest, and the hold line below tells the real story.
		wg.Usage = usageFromAttempt(byName[g.LatestAttempt])
		// The chip qualification (first glance) + the note (the story).
		wg.Cause = g.HoldCause
		// THIS row's attempt progress: the strip lives on the subject
		// (state column), not the workflow block — a bar at the block
		// described one attempt while standing for many subjects (owner,
		// twice). The compiled shape resolves per workflow (cached).
		if att := byName[g.LatestAttempt]; att != nil {
			if wf, ok := wfByName[name]; ok {
				wg.Strip, wg.StripW = s.wallStepsFor(r.Context(), &wf, att)
			}
		}
		// The parked claim's why: chip + note + waiting age. The note
		// rides VERBATIM — CSS line-clamps the row; truncating server-side
		// ate the reason's exit ("dispatch on green") behind an ellipsis.
		if state == "queued" && g.HoldNote != "" {
			wg.Hold = &wallHold{Note: g.HoldNote}
			if g.WaitingSince != nil && !g.WaitingSince.IsZero() {
				wg.Hold.Since = relDuration(time.Since(g.WaitingSince.Time))
			}
		}
		// Live columns: what is executing on this subject right now.
		if state == "in flight" || state == "reconciling" {
			if att := byName[g.LatestAttempt]; att != nil {
				if wf, ok := wfByName[name]; ok {
					res, cached := resolvedCache[name]
					if !cached {
						res = s.resolveWorkflow(r.Context(), &wf)
						resolvedCache[name] = res
					}
					wg.CurrentNode, wg.Elapsed = livePosition(&res, att)
				}
				if p := liveProgressOf(att); p != nil {
					wg.Live = &wallLiveTokens{In: p.TokensIn, Out: p.TokensOut, Turns: p.Turns, Model: p.Model}
				}
			}
		}
		if _, seen := wfGroups[name]; !seen {
			wfOrder = append(wfOrder, name)
		}
		wfGroups[name] = append(wfGroups[name], wg)
		rows++
	}
	overflow := len(groups) - rows - hidden

	secOrder := []string{}
	secs := map[string]*wallSection{}
	for _, name := range wfOrder {
		ww := wallWorkflow{Name: name, URL: "/workflows/" + name, Groups: wfGroups[name], Last: wfGroups[name][0].LastActivity}
		// The rollup line: text, not a bar — the aggregate in words while
		// every visual lives on the row whose progress it describes.
		tally := map[string]int{}
		for _, g := range wfGroups[name] {
			tally[wallState(g)]++
		}
		for _, st := range []string{"in flight", "reconciling", "queued", "dispatch lost", "failed", "verdict", "validated"} {
			if n := tally[st]; n > 0 {
				ww.Counts = append(ww.Counts, wallStateCount{State: st, Count: n})
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
	return out, overflow, counts, nil
}

// renderWallFragment renders the wall grid to a string for SSE delivery.
func (s *Server) renderWallFragment(r *http.Request, owner string) (string, error) {
	sections, overflow, counts, err := s.wallSections(r, owner)
	if err != nil {
		return "", err
	}
	tmpl := s.templates.Lookup("pages/frag_wall.html")
	if tmpl == nil {
		return "", fmt.Errorf("template not found: pages/frag_wall.html")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Sections": sections, "Overflow": overflow, "Counts": counts}); err != nil {
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

// relDuration renders a coarse duration for the hold line: "4m", "3h",
// "2d". Under a minute reads "now" — the hold is fresh, the poll will
// refine it.
func relDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// wallChipLabel qualifies the chip text the way windmill names what a flow
// is waiting for: "queued" alone answers nothing. The gate's short cause
// rides beside it — "queued · waiting ci", "queued · ci red", "queued ·
// needs label" — first-glance, before the hold line's prose. In flight and
// verdict keep their canonical words (the Now column carries the in-flight
// detail; verdict is terminal).
func wallChipLabel(g wallGroup) string {
	base := wallState(g)
	if base != "queued" || g.Cause == "" {
		return base
	}
	switch review.HoldCause(g.Cause) {
	case review.HoldCauseCIPending:
		return "queued · waiting ci"
	case review.HoldCauseCIRed:
		return "queued · ci red"
	case review.HoldCauseLabelAbsent:
		return "queued · needs label"
	case review.HoldCauseVerdictCheck:
		return "queued · checking verdict"
	case review.HoldCauseAPIError:
		return "queued · forge unreachable"
	}
	return base
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
