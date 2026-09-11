package ui

// event_timeline.go — the Event Timeline (ADR-0012 §4): an append-only,
// Temporal-Event-History-style projection of ONE attempt, built from two
// sources and nothing else:
//
//  1. the timeline store (internal/timeline) — the worker's durable
//     node-boundary log (run/node/gate/agent events, 7-day TTL), and
//  2. the Attempt status ledger (the CR's Runs + Review fields) — facts the
//     store cannot supply: the attempt's trigger (creation timestamp), the
//     claim's arm/dispatch instants, and run boundaries that have already
//     TTL'd out of the store.
//
// The ledger stays the truth (ADR-0005); the projection adds no storage and
// no writes. SSE wakes re-render it; reload equals live.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// timelineStateStore is the Dapr state store the worker writes timeline
// events into (same store the transcript reader reads).
const timelineStateStore = "statestore"

// TimelineStateStore exposes the store name so main wiring and the reader
// agree by construction.
func TimelineStateStore() string { return timelineStateStore }

// eventTimelineEventName is the SSE event the timeline stream emits.
const eventTimelineEventName = "eventtimeline"

// etStateClass maps a chip-vocabulary state onto the Event Timeline marker's
// CSS class suffix. Exhaustive over the vocabulary the state-chip template
// arms; anything else is neutral.
func etStateClass(state string) string {
	switch state {
	case "failed", "dispatch lost":
		return "fail"
	case "validated", "verdict":
		return "ok"
	case "in flight", "reconciling", "armed", "queued":
		return "run"
	default:
		return "neutral"
	}
}

// timelineRow is one rendered Event Timeline row.
type timelineRow struct {
	ID      string // stable identity (kind|run|node|time) for the expander pairing
	At      string // human time (right column, mono)
	Kind    string // typed row kind (store kind verbatim, or a rowKind* label)
	Node    string // actor/node ("prepare", "agent", "gate"; "" for run-level)
	Run     string // run name (empty for attempt/gate-level rows)
	State   string // chip vocabulary member (eventState)
	Summary string // one-line human summary
	Payload string // pretty JSON for the expander ("" = no expander)
}

// eventTimelineView is the fragment's template data.
type eventTimelineView struct {
	Rows    []timelineRow
	Empty   string // non-empty → empty-state text (nothing recorded in either source)
	StoreOK bool   // false → the store is unreachable/unwired (notice row explains)
}

// buildEventTimeline projects one Attempt's history into rows, oldest first.
func (s *Server) buildEventTimeline(ctx context.Context, att *v1alpha1.Attempt) eventTimelineView {
	if s.timeline == nil {
		return eventTimelineView{Empty: "Timeline store not wired on this server.", StoreOK: false}
	}

	runNames := make([]string, 0, len(att.Status.Runs))
	for _, run := range att.Status.Runs {
		runNames = append(runNames, run.Name)
	}

	storeEvents, err := s.timeline.Attempt(ctx, att.Name, runNames, timeline.Filter{})
	if err != nil {
		// The projection must never take the page down: degrade to the
		// ledger-only rows with a store-unavailable notice row.
		return s.ledgerRowsWithNotice(att, fmt.Sprintf("Timeline store unreachable (%s) — showing ledger facts only.", errString(err)))
	}
	gateEvents, err := s.timeline.GateEvents(ctx, att.Name, timeline.Filter{})
	if err != nil {
		return s.ledgerRowsWithNotice(att, fmt.Sprintf("Timeline store unreachable (%s) — showing ledger facts only.", errString(err)))
	}

	rows, _ := s.ledgerRows(att)

	seenRunStarted := make(map[string]bool)
	seenRunCompleted := make(map[string]bool)
	for _, ev := range append(append([]timeline.Event{}, storeEvents...), gateEvents...) {
		if ev.Kind == timeline.KindRunStarted {
			seenRunStarted[ev.Run] = true
		}
		if ev.Kind == timeline.KindRunCompleted {
			seenRunCompleted[ev.Run] = true
		}
		rows = append(rows, storeRow(ev))
	}

	// Ledger run boundaries fill ONLY gaps: where the store still holds the
	// event, its richer payload wins; where it has TTL'd away, the CR's
	// RunRecord keeps the row in the story.
	for _, run := range att.Status.Runs {
		if !run.StartedAt.IsZero() && !seenRunStarted[run.Name] {
			rows = append(rows, timelineRow{
				ID:      rowID(rowKindRunStarted, run.Name, "", run.StartedAt.Time),
				At:      formatRowTime(run.StartedAt.Time),
				Kind:    rowKindRunStarted,
				Run:     run.Name,
				State:   eventState(rowKindRunStarted, ""),
				Summary: "run " + run.Name + " started",
			})
		}
		if !run.EndedAt.IsZero() && !seenRunCompleted[run.Name] {
			rows = append(rows, timelineRow{
				ID:      rowID(rowKindRunEnded, run.Name, "", run.EndedAt.Time),
				At:      formatRowTime(run.EndedAt.Time),
				Kind:    rowKindRunEnded,
				Run:     run.Name,
				State:   eventState(rowKindRunEnded, run.Phase),
				Summary: "run " + run.Name + " " + runPhaseText(run.Phase),
			})
		}
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at() < rows[j].at() })
	return eventTimelineView{Rows: rows, StoreOK: true, Empty: emptyTimelineText(att, len(rows))}
}

// ledgerRows projects the CR-side facts that carry timestamps: the trigger
// (creation timestamp — the objective anchoring) and the claim's arm/dispatch
// instants. Released-claim rows are NOT projected here: the ledger records a
// release without an instant, and an undated row would either sort wrong or
// lie; the gate's own store events carry the release transitions.
func (s *Server) ledgerRows(att *v1alpha1.Attempt) ([]timelineRow, bool) {
	var rows []timelineRow
	triggerAt := att.CreationTimestamp.Time
	if !triggerAt.IsZero() {
		payload, _ := json.MarshalIndent(map[string]any{
			"objective":     att.Spec.Objective.Kind,
			"subject":       att.Spec.Objective.PrimarySubject.Object,
			"desired":       att.Spec.Objective.DesiredOutcome,
			"targetedState": att.Spec.Objective.TargetedState,
			"workflow":      att.Spec.WorkflowRef,
		}, "", "  ")
		rows = append(rows, timelineRow{
			ID:      rowID(rowKindTrigger, "", "", triggerAt),
			At:      formatRowTime(triggerAt),
			Kind:    rowKindTrigger,
			State:   eventState(rowKindTrigger, ""),
			Summary: "objective anchored: " + att.Spec.Objective.Kind + " → " + att.Spec.Objective.TargetedState,
			Payload: string(payload),
		})
	}
	if rv := att.Status.Review; rv != nil {
		if rv.ArmedSince != nil && !rv.ArmedSince.Time.IsZero() {
			rows = append(rows, timelineRow{
				ID:      rowID(rowKindClaimArmed, "", rv.PR, rv.ArmedSince.Time),
				At:      formatRowTime(rv.ArmedSince.Time),
				Kind:    rowKindClaimArmed,
				Node:    rv.PR,
				State:   eventState(rowKindClaimArmed, ""),
				Summary: "claim armed at " + shortSHA(rv.HeadSHA),
				Payload: prettyField(map[string]any{"pr": rv.PR, "headSha": rv.HeadSHA, "label": rv.Label}),
			})
		}
		if rv.DispatchedAt != nil && !rv.DispatchedAt.Time.IsZero() {
			rows = append(rows, timelineRow{
				ID:      rowID(rowKindClaimDispatch, "", rv.PR, rv.DispatchedAt.Time),
				At:      formatRowTime(rv.DispatchedAt.Time),
				Kind:    rowKindClaimDispatch,
				Node:    rv.PR,
				State:   eventState(rowKindClaimDispatch, ""),
				Summary: "review dispatched",
				Payload: prettyField(map[string]any{"pr": rv.PR, "headSha": rv.HeadSHA}),
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at() < rows[j].at() })
	return rows, len(rows) > 0
}

// ledgerRowsWithNotice is the degraded path: ledger facts plus one notice row
// explaining the store is unreachable.
func (s *Server) ledgerRowsWithNotice(att *v1alpha1.Attempt, notice string) eventTimelineView {
	rows, _ := s.ledgerRows(att)
	rows = append(rows, timelineRow{
		ID:      rowID("store-unavailable", "", "", time.Time{}),
		At:      "",
		Kind:    "store unavailable",
		State:   "notice",
		Summary: notice,
	})
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].at() < rows[j].at() })
	return eventTimelineView{Rows: rows, StoreOK: false}
}

// storeRow renders one timeline-store event.
func storeRow(ev timeline.Event) timelineRow {
	var payload map[string]any
	_ = json.Unmarshal(ev.Payload, &payload) // malformed payload → nil: summary falls back

	summary := storeSummary(ev.Kind, payload)
	state := eventState(ev.Kind, payloadString(payload, "status"))

	node := ev.Node
	if strings.HasPrefix(ev.Kind, "gate.") && node == "" {
		node = "gate"
	}
	row := timelineRow{
		ID:      rowID(ev.Kind, ev.Run, ev.Node, ev.At),
		At:      formatRowTime(ev.At),
		Kind:    ev.Kind,
		Node:    node,
		Run:     ev.Run,
		State:   state,
		Summary: summary,
	}
	if len(ev.Payload) > 0 && string(ev.Payload) != "null" {
		var pretty bytes.Buffer
		if json.Indent(&pretty, ev.Payload, "", "  ") == nil {
			row.Payload = pretty.String()
		}
	}
	return row
}

// storeSummary derives a row's one-line summary from the store payload.
func storeSummary(kind string, p map[string]any) string {
	switch kind {
	case timeline.KindRunStarted:
		return "run started" + fromSource(p)
	case timeline.KindRunCompleted:
		msg := payloadString(p, "message")
		if msg != "" {
			return "run ended: " + msg
		}
		return "run ended (" + payloadString(p, "status") + ")" + fromSource(p)
	case timeline.KindNodeStarted:
		return "node started (" + payloadString(p, "type") + ")"
	case timeline.KindNodeCompleted:
		status := payloadString(p, "status")
		dur := payloadString(p, "durationMs")
		line := "node finished (" + status
		if dur != "" {
			if ms, err := strconv.Atoi(dur); err == nil {
				line += ", " + formatDuration(time.Duration(ms)*time.Millisecond)
			}
		}
		if fb := payloadString(p, "feedback"); fb != "" {
			line += ": " + fb
		}
		return line + ")"
	case timeline.KindPluginTail:
		return "plugin output" + lineSuffix(payloadString(p, "line"))
	case timeline.KindAgentTurn:
		label := payloadString(p, "label")
		line := "agent turn"
		if label != "" {
			line += ": " + label
		}
		if tok := payloadString(p, "tokensIn"); tok != "" {
			line += " (" + tok + "→" + payloadString(p, "tokensOut") + " tok)"
		}
		return line
	case timeline.KindAgentTool:
		return "agent tool: " + payloadString(p, "tool")
	case timeline.KindGateArmed:
		return "gate armed at " + shortSHA(payloadString(p, "head"))
	case timeline.KindGateWaiting:
		return "gate waiting: " + payloadString(p, "reason")
	case timeline.KindGateProceed:
		return "gate proceeding: " + payloadString(p, "reason")
	case timeline.KindGateStanddown:
		return "gate stood down: " + payloadString(p, "reason")
	case timeline.KindGateCancel:
		return "gate cancelled: " + payloadString(p, "reason")
	default:
		return kind
	}
}

func fromSource(p map[string]any) string {
	if s := payloadString(p, "source"); s != "" {
		return " (" + s + ")"
	}
	return ""
}

func lineSuffix(line string) string {
	if line == "" {
		return ""
	}
	return ": " + line
}

// payloadString reads a string (or numeric) field from a decoded payload.
func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	// Numbers arrive as float64 from encoding/json.
	if f, ok := p[key].(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

// prettyField marshals a small map for a row's expander; errors yield "".
func prettyField(m map[string]any) string {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// rowID is a stable row identity for the payload expander pairing.
func rowID(kind, run, node string, at time.Time) string {
	return fmt.Sprintf("%s|%s|%s|%d", kind, run, node, at.UnixNano())
}

// at is the sort key (Unix nanos as string — fixed width until 2262, so
// lexicographic order is numeric order). Reconstructed from the ID so the
// row carries one time representation, not two.
func (r timelineRow) at() string {
	if i := strings.LastIndex(r.ID, "|"); i >= 0 {
		return r.ID[i+1:]
	}
	return ""
}

// emptyTimelineText distinguishes "nothing happened" from "history expired".
func emptyTimelineText(att *v1alpha1.Attempt, n int) string {
	if n > 0 {
		return ""
	}
	if att.Status.TotalRuns() == 0 {
		return "No events yet — the workflow has not run inside this attempt."
	}
	return "No events in the store window (7-day TTL) and no ledger facts with timestamps."
}

// runPhaseText renders a ledger run phase for the run-ended row.
func runPhaseText(phase string) string {
	switch phase {
	case "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	case "running":
		return "still running"
	default:
		return phase
	}
}

// shortSHA — see wall.go (shared 7-char truncation for display).

// formatRowTime renders an event instant for dense rows.
func formatRowTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05 MST")
}

// errString flattens an error for inline display.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// handleRunEvents renders the Event Timeline fragment (HTMX target of the
// Events tab). Same owner gate as every attempt-scoped endpoint.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attemptOr404(w, r)
	if !ok {
		return
	}
	frag, err := s.renderEventTimeline(r, att)
	if err != nil {
		s.renderError(w, r, "Failed to render timeline: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(frag))
}

// handleRunEventsSSE streams the Event Timeline fragment on lifecycle wakes.
// Same subscription discipline as the graph stream: attempt-filtered at the
// hub, 15s convergence ticker, stops once the attempt settles or is deleted.
func (s *Server) handleRunEventsSSE(w http.ResponseWriter, r *http.Request) {
	att, ok := s.attemptOr404(w, r)
	if !ok {
		return
	}
	wfName := workflowCRName(att.Spec.WorkflowRef)
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
		return s.renderEventTimeline(r, fresh)
	}
	sub, cancel := s.hub.SubscribeFilter(wfName, func(ev Event) bool {
		return ev.Attempt == "" || ev.Attempt == att.Name
	})
	s.streamFragments(w, r, sub, cancel, eventTimelineEventName, render, func() bool { return terminal }, 15*time.Second)
}

// renderEventTimeline renders the timeline fragment to a string.
func (s *Server) renderEventTimeline(r *http.Request, att *v1alpha1.Attempt) (string, error) {
	view := s.buildEventTimeline(r.Context(), att)
	return s.renderFragmentString("pages/frag_event_timeline.html", map[string]any{
		"Name": att.Name,
		"TL":   view,
	})
}
