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
	Subject    string    `json:"subject,omitempty"` // what this session was about (timeline Subject.Ref)
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

	// Orientation: resolve the Subject index for these attempts (bulk get).
	var names []string
	for _, att := range attemptList.Items {
		names = append(names, att.Name)
	}
	var subjects map[string]string // attempt → Subject.Ref
	if s.timelineReader != nil {
		subs, err := s.timelineReader.Subjects(r.Context(), names)
		if err != nil {
			s.logger.Warn("subjects bulk-get failed", "err", err)
		} else {
			subjects = make(map[string]string, len(subs))
			for a, sub := range subs {
				if sub.Ref != "" {
					subjects[a] = sub.Ref
				}
			}
		}
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
			entry := sessionEntry{
				Workflow:   wfName,
				Run:        run.Name,
				Attempt:    att.Name,
				Phase:      run.Phase,
				StartedAt:  run.StartedAt.Time,
				SessionURL: fmt.Sprintf("/attempts/%s/runs/%s/session", att.Name, run.Name),
				Subject:    subjects[att.Name],
			}
			entries = append(entries, entry)
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
		"Workflows":    s.workflowNames(r, owner),
		"Sessions":     entries,
	})
}
