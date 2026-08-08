package ui

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// sessionEntry is one row in the Sessions list.
type sessionEntry struct {
	Workflow   string    `json:"workflow"`
	Run        string    `json:"run"`
	Attempt    string    `json:"attempt"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"startedAt"`
	SessionURL string    `json:"sessionUrl"`
}

// handleSessionsView lists all agent session transcripts (from Attempt
// RunRecords), accessible per-workflow.
func (s *Server) handleSessionsView(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	workflowFilter := r.URL.Query().Get("workflow")

	var attemptList v1alpha1.AttemptList
	opts := []client.ListOption{client.InNamespace(s.namespace)}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(r.Context(), &attemptList, opts...); err != nil {
		s.logger.Error("list attempts for sessions", "err", err)
		s.renderError(w, r, "Failed to load sessions: "+err.Error())
		return
	}

	var entries []sessionEntry
	for _, att := range attemptList.Items {
		wfName := att.Labels["harmostes.dev/workflow"]
		if wfName == "" {
			wfName = att.Spec.WorkflowRef
		}
		if workflowFilter != "" && wfName != workflowFilter {
			continue
		}

		for _, run := range att.Status.Runs {
			entries = append(entries, sessionEntry{
				Workflow:   wfName,
				Run:        run.Name,
				Attempt:    att.Name,
				Phase:      run.Phase,
				StartedAt:  run.StartedAt.Time,
				SessionURL: fmt.Sprintf("/attempts/%s/runs/%s/session", att.Name, run.Name),
			})
		}
	}

	// Sort by start time descending
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].StartedAt.After(entries[j].StartedAt)
	})

	// Limit to 100
	if len(entries) > 100 {
		entries = entries[:100]
	}

	s.render(w, r, "pages/sessions.html", map[string]any{
		"WorkflowName": workflowFilter,
		"Sessions":     entries,
	})
}
