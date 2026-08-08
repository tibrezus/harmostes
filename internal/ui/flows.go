package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// flowRow is a single row in the Flows table — a unified representation of
// either a live SSE event or a historical run/node record from an Attempt.
type flowRow struct {
	Timestamp time.Time `json:"timestamp"`
	Workflow  string    `json:"workflow"`
	Node      string    `json:"node"`
	Event     string    `json:"event"`
	Status    string    `json:"status"`
	Duration  string    `json:"duration"`
	Run       string    `json:"run,omitempty"`
	Summary   string    `json:"summary,omitempty"`
}

// handleFlowsView renders the real-time flow table.
func (s *Server) handleFlowsView(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflowName := r.URL.Query().Get("workflow")

	// Load historical flow rows from Attempts.
	historical := s.loadHistoricalFlows(r, owner, workflowName)

	s.render(w, r, "pages/flows.html", map[string]any{
		"WorkflowName": workflowName,
		"Historical":   historical,
	})
}

// loadHistoricalFlows queries Attempt CRs and converts their run records +
// node results into flow rows for the table.
func (s *Server) loadHistoricalFlows(r *http.Request, owner, workflowFilter string) []flowRow {
	var attemptList v1alpha1.AttemptList
	opts := []client.ListOption{client.InNamespace(s.namespace)}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(r.Context(), &attemptList, opts...); err != nil {
		s.logger.Error("list attempts for flows", "err", err)
		return nil
	}

	var rows []flowRow
	for _, att := range attemptList.Items {
		wfName := att.Labels["harmostes.dev/workflow"]
		if wfName == "" {
			wfName = att.Spec.WorkflowRef
		}
		if workflowFilter != "" && wfName != workflowFilter {
			continue
		}

		// Run-level events (started/completed from RunRecords)
		for _, run := range att.Status.Runs {
			if !run.StartedAt.IsZero() {
				rows = append(rows, flowRow{
					Timestamp: run.StartedAt.Time,
					Workflow:  wfName,
					Node:      "—",
					Event:     "run.started",
					Status:    run.Phase,
					Run:       run.Name,
				})
			}
			if !run.EndedAt.IsZero() {
				dur := ""
				if !run.StartedAt.IsZero() {
					d := run.EndedAt.Time.Sub(run.StartedAt.Time)
					if d > 0 {
						dur = formatDuration(d)
					}
				}
				rows = append(rows, flowRow{
					Timestamp: run.EndedAt.Time,
					Workflow:  wfName,
					Node:      "—",
					Event:     "run." + phaseToEvent(run.Phase),
					Status:    run.Phase,
					Duration:  dur,
					Run:       run.Name,
				})
			}
		}

		// Node-level events (from NodeResultEnvelopes)
		for _, nr := range att.Status.NodeResults {
			if nr.ProducedAt.IsZero() {
				continue
			}
			rows = append(rows, flowRow{
				Timestamp: nr.ProducedAt.Time,
				Workflow:  wfName,
				Node:      nr.NodeID,
				Event:     "node." + nr.Status,
				Status:    nr.Status,
				Summary:   nr.Summary,
				Run:       nr.RunID,
			})
		}
	}

	// Sort by timestamp descending (most recent first)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Timestamp.After(rows[j].Timestamp)
	})

	// Limit to 200 rows for the initial page load
	if len(rows) > 200 {
		rows = rows[:200]
	}

	return rows
}

func phaseToEvent(phase string) string {
	switch phase {
	case "succeeded":
		return "completed"
	case "failed":
		return "failed"
	case "running":
		return "started"
	default:
		return phase
	}
}

// handleFlowsSSE streams ALL lifecycle events as SSE (for the Flows view's
// live update). When a workflow query param is present, it subscribes to that
// pipeline only; otherwise it subscribes globally (empty key = all events).
func (s *Server) handleFlowsSSE(w http.ResponseWriter, r *http.Request) {
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

	// Subscribe globally (empty string) or per-workflow.
	sub, cancel := s.hub.Subscribe(workflow)
	defer cancel()

	fmt.Fprintf(w, ": connected to flows stream\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
