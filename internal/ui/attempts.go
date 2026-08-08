package ui

import (
	"fmt"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// attemptListData is the template data for the attempt list page.
type attemptListData struct {
	Attempts []attemptSummary
}

type attemptSummary struct {
	Name           string
	WorkflowRef    string
	ObjectiveKind  string
	PrimarySubject string
	Phase          string
	LastRunAt      string // human-readable, empty if never
	RunCount       int
}

// listAttempts returns all Attempt CRs owned by the given user.
func (s *Server) listAttempts(r *http.Request, owner string) ([]v1alpha1.Attempt, error) {
	var list v1alpha1.AttemptList
	opts := []client.ListOption{client.InNamespace(s.namespace)}
	if owner != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: owner})
	}
	if err := s.k8sClient.List(r.Context(), &list, opts...); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	return list.Items, nil
}

// getAttempt retrieves a single Attempt by name.
func (s *Server) getAttempt(r *http.Request, name string) (*v1alpha1.Attempt, error) {
	att := &v1alpha1.Attempt{}
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, att); err != nil {
		return nil, fmt.Errorf("get attempt %s: %w", name, err)
	}
	return att, nil
}

// handleAttemptList renders the primary observability view: all Attempts for
// the authenticated user, ordered by most recent activity.
func (s *Server) handleAttemptList(w http.ResponseWriter, r *http.Request) {
	identity := identityFromContext(r.Context())
	attempts, err := s.listAttempts(r, identity.Username)
	if err != nil {
		s.renderError(w, r, "Failed to list attempts: "+err.Error())
		return
	}

	summaries := make([]attemptSummary, 0, len(attempts))
	for _, a := range attempts {
		lastRun := ""
		if !a.Status.LastRunAt.IsZero() {
			lastRun = a.Status.LastRunAt.Time.Format("2006-01-02 15:04 MST")
		}
		summaries = append(summaries, attemptSummary{
			Name:           a.Name,
			WorkflowRef:    a.Spec.WorkflowRef,
			ObjectiveKind:  a.Spec.Objective.Kind,
			PrimarySubject: a.Spec.Objective.PrimarySubject.Object,
			Phase:          a.Status.Phase,
			LastRunAt:      lastRun,
			RunCount:       len(a.Status.Runs),
		})
	}

	// Sort by most recent activity (LastRunAt desc, then name).
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].LastRunAt > summaries[j].LastRunAt
	})

	s.render(w, r, "pages/attempts.html", map[string]any{
		"Attempts": summaries,
	})
}

// attemptDetailData is the template data for the attempt detail page.
type attemptDetailData struct {
	Name           string
	WorkflowRef    string
	ObjectiveKind  string
	PrimarySubject string
	DesiredOutcome string
	TargetedState  string
	Phase          string
	Message        string
	Runs           []runSummary
	NodeResults    []v1alpha1.NodeResultEnvelope
	Evidence       []v1alpha1.EvidenceReference
	Owner          string
}

type runSummary struct {
	Name      string
	StartedAt string
	EndedAt   string
	Phase     string
}

// handleAttemptDetail renders one Attempt's full detail: objective, runs
// timeline, node result envelopes, and evidence references.
func (s *Server) handleAttemptDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		s.renderError(w, r, "attempt name required")
		return
	}

	att, err := s.getAttempt(r, name)
	if err != nil {
		s.renderError(w, r, "Failed to get attempt: "+err.Error())
		return
	}

	identity := identityFromContext(r.Context())
	if att.Labels[v1alpha1.OwnerLabel] != identity.Username {
		s.renderError(w, r, "attempt not found")
		return
	}

	runs := make([]runSummary, 0, len(att.Status.Runs))
	for _, run := range att.Status.Runs {
		started := ""
		ended := ""
		if !run.StartedAt.IsZero() {
			started = run.StartedAt.Time.Format("2006-01-02 15:04:05 MST")
		}
		if !run.EndedAt.IsZero() {
			ended = run.EndedAt.Time.Format("2006-01-02 15:04:05 MST")
		}
		runs = append(runs, runSummary{
			Name:      run.Name,
			StartedAt: started,
			EndedAt:   ended,
			Phase:     run.Phase,
		})
	}

	data := attemptDetailData{
		Name:           att.Name,
		WorkflowRef:    att.Spec.WorkflowRef,
		ObjectiveKind:  att.Spec.Objective.Kind,
		PrimarySubject: att.Spec.Objective.PrimarySubject.Object,
		DesiredOutcome: att.Spec.Objective.DesiredOutcome,
		TargetedState:  att.Spec.Objective.TargetedState,
		Phase:          att.Status.Phase,
		Message:        att.Status.Message,
		Runs:           runs,
		NodeResults:    att.Status.NodeResults,
		Evidence:       att.Status.Evidence,
		Owner:          att.Spec.Owner,
	}
	s.render(w, r, "pages/attempt_detail.html", data)
}
