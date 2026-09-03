package ui

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// The live wall (`/`) — the zero-click surface: what is running, in which
// workflow, where is it. Groups are the ADR-0007 attempt rollups; agent
// metadata (model, tokens, turns) arrives on the lifecycle event bus and is
// cached per workflow.
// ---------------------------------------------------------------------------

const (
	wallDebounce  = 500 * time.Millisecond // coalesce per-node event bursts
	wallRerender  = 30 * time.Second       // keep relative times fresh without events
	sseHeartbeat  = 15 * time.Second       // shared with the other SSE handlers
	wallMaxGroups = 12                     // density cap: a wall is a glance, not a list
	wallEventName = "wall"
)

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

// wallGroup is one card on the wall: a PR (review class) or a workflow
// (scheduled classes), with the latest state and a single drill-down link.
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
	groups, err := s.wallGroups(r, owner, true)
	if err != nil {
		s.renderError(w, r, "Failed to load wall: "+err.Error())
		return
	}
	s.render(w, r, "pages/wall.html", map[string]any{"Groups": groups})
}

// wallGroups folds attempts into wall cards. No window cutoff: an
// armed-but-quiet review is live state, not history — the wall answers
// "what is the platform doing", /runs answers "what happened".
func (s *Server) wallGroups(r *http.Request, owner string, hydrate bool) ([]wallGroup, error) {
	attempts, err := s.listAttempts(r, owner)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	groups := groupAttempts(attempts, time.Time{})
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LastActivity > groups[j].LastActivity
	})
	out := make([]wallGroup, 0, min(len(groups), wallMaxGroups))
	for _, g := range groups {
		if len(out) >= wallMaxGroups {
			break
		}
		// WorkflowRef is namespaced (ns/name); the event cache and the
		// durable usage:last record are keyed by the bare CR name (which is
		// also the lifecycle event's Pipeline field).
		wfName := workflowCRName(g.WorkflowRef)
		wg := wallGroup{
			WorkflowRef:  wfName,
			Subject:      g.Subject,
			IsReview:     g.IsReview,
			Phase:        g.LatestPhase,
			ClaimState:   g.ClaimState,
			HeadSHA:      shortSHA(g.HeadSHA),
			Count:        g.Count,
			LastActivity: relTime(g.LastActivity, time.Now()),
			Usage:        s.usageFor(r, wfName, hydrate),
		}
		if g.LatestAttempt != "" {
			wg.LastRunURL = "/runs/" + g.LatestAttempt
		}
		out = append(out, wg)
	}
	return out, nil
}

// renderWallFragment renders the wall grid to a string for SSE delivery.
func (s *Server) renderWallFragment(r *http.Request, owner string) (string, error) {
	groups, err := s.wallGroups(r, owner, false)
	if err != nil {
		return "", err
	}
	tmpl := s.templates.Lookup("pages/frag_wall.html")
	if tmpl == nil {
		return "", fmt.Errorf("template not found: pages/frag_wall.html")
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Groups": groups}); err != nil {
		return "", fmt.Errorf("render wall fragment: %w", err)
	}
	return buf.String(), nil
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
		if strings.Contains(g.ClaimState, "dispatch lost") {
			return "dispatch lost"
		}
		switch g.ClaimState {
		case "review in flight":
			return "in flight"
		case "verdict posted":
			return "verdict"
		case "queued":
			return "queued"
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
