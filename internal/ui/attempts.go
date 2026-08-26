package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/agent"
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
	WorkflowLink   string // platform prefix stripped — routable CR name
	Runs           []runSummary
	NodeResults    []v1alpha1.NodeResultEnvelope
	Evidence       []v1alpha1.EvidenceReference
	Owner          string
	AgentEnabled   bool
}

type runSummary struct {
	Name      string
	StartedAt string
	EndedAt   string
	Phase     string
}

// agentEnabledFor resolves whether a workflow runs an agent, matching the
// kernel's rule (compile.go, pipeline.go): Agent.Enabled == nil means
// agent-capable. Template-delegated workflows (the live pr-review shape)
// carry no Enabled on the instance — template defaults are applied first so
// an explicit template disable still wins.
func (s *Server) agentEnabledFor(ctx context.Context, wf *v1alpha1.Workflow) bool {
	return s.resolveWorkflow(ctx, wf).Spec.Agent.EnabledOrDefault()
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

	// Fetch the Workflow to determine whether the agent is enabled.
	// This controls whether the Session link is shown for runs. The ref is
	// platform-prefixed ("ns/name"); the CR is addressed by its bare name.
	agentEnabled := false
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: workflowCRName(att.Spec.WorkflowRef)}, &wf); err == nil {
		agentEnabled = s.agentEnabledFor(r.Context(), &wf)
	}

	data := attemptDetailData{
		Name:           att.Name,
		WorkflowLink:   workflowCRName(att.Spec.WorkflowRef),
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
		AgentEnabled:   agentEnabled,
	}
	s.render(w, r, "pages/attempt_detail.html", data)
}

// attemptSessionData is the template data for the agent session viewer.
type attemptSessionData struct {
	AttemptName   string
	WorkflowRef   string
	RunName       string
	Session       *agentSessionView
	Deterministic bool
	HasPiSession  bool
}

// agentSessionView is the session record adapted for template rendering.
type agentSessionView struct {
	Model     string
	Skill     string
	Green     bool
	StartedAt string
	EndedAt   string
	Turns     []turnView
	TotalIn   int
	TotalOut  int
	TotalCost float64
}

type turnView struct {
	Label    string
	Prompt   string
	Response string
	Tools    []toolView
	Gate     *gateView
	UsageIn  int
	UsageOut int
}

type toolView struct {
	Name    string
	Args    string
	Success bool
	Result  string
}

type gateView struct {
	Green  bool
	Output string
}

// handleAttemptSession renders the full agent transcript (prompts, tool calls
// with args+results, responses, gate feedback) from Dapr state.
func (s *Server) handleAttemptSession(w http.ResponseWriter, r *http.Request) {
	attName := r.PathValue("name")
	jobName := r.PathValue("job")
	if attName == "" || jobName == "" {
		s.renderError(w, r, "attempt name and job name required")
		return
	}

	// Resolve the workflow name from the attempt.
	att, err := s.getAttempt(r, attName)
	if err != nil {
		s.renderError(w, r, "Failed to get attempt: "+err.Error())
		return
	}

	identity := identityFromContext(r.Context())
	if att.Labels[v1alpha1.OwnerLabel] != identity.Username {
		s.renderError(w, r, "attempt not found")
		return
	}

	// The session writer keys state by the bare workflow name (the worker's
	// wfCtx.Name), and session.html back-links to /workflows/{name}/… —
	// both break on the raw platform-prefixed ref.
	wfName := workflowCRName(att.Spec.WorkflowRef)

	// Read the session from the worker's Dapr state store.
	// Key format: {workflow}:{runID}:session
	sessionKey := fmt.Sprintf("%s:%s:session", wfName, jobName)

	var raw struct {
		Workflow  string `json:"workflow"`
		RunID     string `json:"runId"`
		Model     string `json:"model"`
		Skill     string `json:"skill"`
		StartedAt string `json:"startedAt"`
		EndedAt   string `json:"endedAt"`
		Green     bool   `json:"green"`
		Turns     []struct {
			Label    string `json:"label"`
			Prompt   string `json:"prompt"`
			Response string `json:"response"`
			Tools    []struct {
				Name    string         `json:"name"`
				Args    map[string]any `json:"args"`
				Success *bool          `json:"success"`
				Result  string         `json:"result"`
			} `json:"tools"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
			Gate *struct {
				Green  bool   `json:"green"`
				Output string `json:"output"`
			} `json:"gate"`
		} `json:"turns"`
		TotalUsage struct {
			Input  int     `json:"input_tokens"`
			Output int     `json:"output_tokens"`
			Cost   float64 `json:"cost"`
		} `json:"totalUsage"`
	}

	var sessionView *agentSessionView

	if s.dapr != nil {
		found, err := s.dapr.GetStateFromStore(r.Context(), "statestore", sessionKey, &raw)
		if err != nil {
			s.logger.Error("get session state", "key", sessionKey, "err", err)
		}
		if found {
			sv := &agentSessionView{
				Model:     raw.Model,
				Skill:     raw.Skill,
				Green:     raw.Green,
				StartedAt: raw.StartedAt,
				EndedAt:   raw.EndedAt,
			}
			for _, t := range raw.Turns {
				tv := turnView{
					Label:    t.Label,
					Prompt:   t.Prompt,
					Response: t.Response,
					UsageIn:  t.Usage.Input,
					UsageOut: t.Usage.Output,
				}
				for _, tool := range t.Tools {
					argsBytes, _ := json.MarshalIndent(tool.Args, "", "  ")
					tv.Tools = append(tv.Tools, toolView{
						Name:    tool.Name,
						Args:    string(argsBytes),
						Success: tool.Success != nil && *tool.Success,
						Result:  tool.Result,
					})
				}
				if t.Gate != nil {
					tv.Gate = &gateView{Green: t.Gate.Green, Output: t.Gate.Output}
				}
				sv.Turns = append(sv.Turns, tv)
			}
			sv.TotalIn = raw.TotalUsage.Input
			sv.TotalOut = raw.TotalUsage.Output
			sv.TotalCost = raw.TotalUsage.Cost
			sessionView = sv
		}
	}

	// Determine whether the workflow uses an agent. Deterministic workflows
	// (agent disabled) have no session, so the empty state should reflect that.
	// Resolution matches the kernel: nil Enabled on instance and template
	// means agent-capable (template-delegated pr-review shape).
	deterministic := true
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: wfName}, &wf); err == nil {
		deterministic = !s.agentEnabledFor(r.Context(), &wf)
	}

	// Forkable pi session availability (#243): the button only renders when
	// the worker actually uploaded one (kernel ≥ 1.2.0-70 era runs have none).
	// Probe the tiny metadata key, never the blob (O(1) vs up to ~27 MB).
	hasPiSession := false
	if s.dapr != nil {
		var meta struct {
			Bytes   int    `json:"bytes"`
			SavedAt string `json:"savedAt"`
		}
		found2, err2 := s.dapr.GetStateFromStore(r.Context(), "statestore",
			fmt.Sprintf("%s:%s:pi-session", wfName, jobName), &meta)
		if err2 != nil {
			// A state-store outage silently hides the Fork button otherwise —
			// indistinguishable from "no session" (#244 r3).
			s.logger.Error("pi session availability probe failed", "err", err2)
		} else if found2 {
			hasPiSession = true
		}
	}

	data := attemptSessionData{
		AttemptName:   attName,
		WorkflowRef:   wfName,
		RunName:       jobName,
		Session:       sessionView,
		Deterministic: deterministic,
		HasPiSession:  hasPiSession,
	}
	s.render(w, r, "pages/session.html", data)
}

// handleAttemptPiSession serves the forkable native pi session file
// (worker.SavePiSession payload: redacted, gz1-wrapped). Auth chain mirrors
// the session page: attempt owner only.
func (s *Server) handleAttemptPiSession(w http.ResponseWriter, r *http.Request) {
	attName := r.PathValue("name")
	jobName := r.PathValue("job")
	att, err := s.getAttempt(r, attName)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if att.Labels[v1alpha1.OwnerLabel] != identityFromContext(r.Context()).Username {
		http.NotFound(w, r)
		return
	}
	if s.dapr == nil {
		http.NotFound(w, r)
		return
	}
	wfName := workflowCRName(att.Spec.WorkflowRef)
	var payload string
	found, err := s.dapr.GetStateFromStore(r.Context(), "statestore",
		fmt.Sprintf("%s:%s:pi-session/data", wfName, jobName), &payload)
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	raw, err := agent.LoadPiSession(payload)
	if err != nil {
		s.renderError(w, r, "Failed to decode pi session: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pi-session-%s.jsonl"`, jobName))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(raw)
}
