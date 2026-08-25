package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// The Timeline page is the execution runtime view: one time-ordered stream
// per workflow, merged from the evidence layer (timeline.Reader) across the
// workflow's recent Attempts + gate cycles. It REPLACES the former Flows and
// Live pages (run.started rows + forward-only terminal): the same stream,
// with history, orientation (Subject), and tails.

// maxTimelineAttempts bounds how many Attempts are probed per page load.
const maxTimelineAttempts = 10

// timelineRow is one rendered event.
type timelineRow struct {
	At      time.Time
	Kind    string
	Node    string
	Attempt string
	Run     string
	Ref     string // Subject.Ref — "tibrez/rhesadox#1566"
	Title   string // Subject.Title
	SHA     string

	// Detail (kind-specific, already short):
	Summary    string   // one line: feedback / reason / status
	Tail       []string // plugin.tail lines
	AttemptURL string
}

// handleTimelineView renders the merged event stream for one workflow.
func (s *Server) handleTimelineView(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflowName := r.URL.Query().Get("workflow")

	rows := s.loadTimeline(r.Context(), owner, workflowName)

	s.render(w, r, "pages/timeline.html", map[string]any{
		"WorkflowName": workflowName,
		"Rows":         rows,
	})
}

// loadTimeline merges evidence events across a workflow's recent Attempts.
func (s *Server) loadTimeline(ctx context.Context, owner, workflowFilter string) []timelineRow {
	if s.timelineReader == nil {
		return nil
	}

	var attemptList v1alpha1.AttemptList
	opts := []client.ListOption{client.InNamespace(s.namespace)}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(ctx, &attemptList, opts...); err != nil {
		s.logger.Error("list attempts for timeline", "err", err)
		return nil
	}

	// Most recent attempts for the selected workflow.
	var attempts []v1alpha1.Attempt
	for i := range attemptList.Items {
		att := attemptList.Items[i]
		wfName := att.Labels["harmostes.dev/workflow"]
		if wfName == "" {
			wfName = att.Spec.WorkflowRef
		}
		if workflowFilter != "" && wfName != workflowFilter {
			continue
		}
		attempts = append(attempts, att)
	}
	sort.Slice(attempts, func(i, j int) bool {
		return attempts[i].CreationTimestamp.After(attempts[j].CreationTimestamp.Time)
	})
	if len(attempts) > maxTimelineAttempts {
		attempts = attempts[:maxTimelineAttempts]
	}

	var rows []timelineRow
	for _, att := range attempts {
		runs := make([]string, 0, len(att.Status.Runs))
		for _, run := range att.Status.Runs {
			runs = append(runs, run.Name)
		}
		events, err := s.timelineReader.Attempt(ctx, att.Name, runs, timeline.Filter{})
		if err != nil {
			s.logger.Warn("timeline read failed", "attempt", att.Name, "err", err)
			continue
		}
		// Gate lifecycle events live under the attempt's gate keyspace —
		// merge them into the same stream (they precede the run's events).
		gateEvents, err := s.timelineReader.GateEvents(ctx, att.Name, timeline.Filter{})
		if err != nil {
			s.logger.Warn("timeline gate read failed", "attempt", att.Name, "err", err)
		} else {
			events = append(events, gateEvents...)
		}
		for _, ev := range events {
			rows = append(rows, timelineRow{
				At:         ev.At,
				Kind:       ev.Kind,
				Node:       ev.Node,
				Attempt:    att.Name,
				Run:        ev.Run,
				Ref:        ev.Subject.Ref,
				Title:      ev.Subject.Title,
				SHA:        ev.Subject.SHA,
				Summary:    eventSummary(ev),
				Tail:       eventTail(ev),
				AttemptURL: "/attempts/" + att.Name,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].At.After(rows[j].At) })
	if len(rows) > 200 {
		rows = rows[:200]
	}
	return rows
}

// eventSummary extracts a one-line detail from a kind-specific payload.
func eventSummary(ev timeline.Event) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(ev.Payload, &p) != nil {
		return ""
	}
	switch ev.Kind {
	case timeline.KindNodeCompleted:
		if fb, ok := p["feedback"].(string); ok && fb != "" {
			return firstLine(fb)
		}
		if st, ok := p["status"].(string); ok {
			return st
		}
	case timeline.KindRunStarted, timeline.KindRunCompleted:
		if m, ok := p["message"].(string); ok && m != "" {
			return m
		}
		if st, ok := p["status"].(string); ok {
			return st
		}
	case timeline.KindGateArmed, timeline.KindGateWaiting, timeline.KindGateProceed, timeline.KindGateStanddown:
		if reason, ok := p["reason"].(string); ok {
			return reason
		}
	case timeline.KindAgentTurn:
		if label, ok := p["label"].(string); ok {
			return label
		}
	case timeline.KindAgentTool:
		if tool, ok := p["tool"].(string); ok {
			return tool
		}
	}
	return ""
}

// eventTail extracts plugin tail lines when present.
func eventTail(ev timeline.Event) []string {
	if ev.Kind != timeline.KindPluginTail || len(ev.Payload) == 0 {
		return nil
	}
	var p struct {
		Tail []string `json:"tail"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil {
		return nil
	}
	return p.Tail
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// handleTimelineSSE streams lifecycle events live so the timeline page
// tails in place (replaces the old Flows/Live terminal stream).
func (s *Server) handleTimelineSSE(w http.ResponseWriter, r *http.Request) {
	workflow := r.URL.Query().Get("workflow")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sub, cancel := s.hub.Subscribe(workflow)
	defer cancel()

	fmt.Fprintf(w, ": connected to timeline stream\n\n")
	flusher.Flush()

	// Heartbeat: idle streams survive buffering proxies (mirrors the
	// deleted flows handler and handlePipelineSSE).
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}
