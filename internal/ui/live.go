package ui

import (
	"net/http"
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

// activeRun represents a currently-running or recently-completed worker Job
// for a workflow. Used by the Live TUI to show run context.
type activeRun struct {
	Name   string    `json:"name"`
	Phase  string    `json:"phase"` // running | succeeded | failed
	Active bool      `json:"active"`
	Start  time.Time `json:"start,omitempty"`
}

// handleLiveView renders the live agent terminal.
func (s *Server) handleLiveView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")

	// Find the most recent run for context
	var run *activeRun
	if workflowName != "" {
		owner := identityFromContext(r.Context()).Username
		jobs, err := s.listJobs(r, workflowName, owner)
		if err == nil && len(jobs) > 0 {
			// Find most recent job (sorted by creation timestamp descending)
			var latest batchv1.Job
			for i := range jobs {
				if jobs[i].CreationTimestamp.After(latest.CreationTimestamp.Time) {
					latest = jobs[i]
				}
			}
			run = &activeRun{
				Name:   latest.Name,
				Active: latest.Status.Active > 0,
				Start:  latest.CreationTimestamp.Time,
			}
			if latest.Status.Succeeded > 0 {
				run.Phase = "succeeded"
			} else if latest.Status.Failed > 0 {
				run.Phase = "failed"
			} else if latest.Status.Active > 0 {
				run.Phase = "running"
			}
		}
	}

	s.render(w, r, "pages/live.html", map[string]any{
		"WorkflowName": workflowName,
		"Run":          run,
	})
}
