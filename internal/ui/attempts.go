package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	attempts, err := s.listAttempts(r, s.visibleOwner(identity))
	if err != nil {
		s.renderError(w, r, "Failed to list attempts: "+err.Error())
		return
	}

	// ADR-0007 anti-bloat contract: with Job-per-attempt every run creates
	// an Attempt, so the list aggregates. Review attempts roll up per PR
	// (one row: PR, review count, claim state — expandable); scheduled
	// classes collapse to a single last-run row per workflow. Default
	// window 24h, explicit filters (?window=24h|7d|all).
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	status := r.URL.Query().Get("status")
	cutoff := windowCutoff(window, time.Now())

	groups := groupAttempts(attempts, cutoff)
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].LastActivity > groups[j].LastActivity
	})

	// The strip counts the WHOLE window; the tabs filter it. Rank order
	// floats failures to the top regardless of activity (the orchestration-
	// list convention: failed first, then by recency).
	counts := runCounts{Total: len(groups)}
	for _, g := range groups {
		switch groupState(g) {
		case "failed", "dispatch lost":
			counts.Failed++
		case "in flight", "reconciling", "queued":
			counts.InFlight++
		case "verdict", "validated":
			counts.Verdicts++
		}
	}

	if status == "failed" || status == "inflight" || status == "verdicts" {
		filtered := groups[:0]
		for _, g := range groups {
			st := groupState(g)
			ok := (status == "failed" && (st == "failed" || st == "dispatch lost")) ||
				(status == "inflight" && (st == "in flight" || st == "reconciling" || st == "queued")) ||
				(status == "verdicts" && (st == "verdict" || st == "validated"))
			if ok {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return stateRank(groupState(groups[i])) < stateRank(groupState(groups[j]))
	})

	s.render(w, r, "pages/attempts.html", map[string]any{
		"Groups": groups,
		"Window": window,
		"Status": status,
		"Counts": counts,
	})
}

// runCounts feeds the summary strip: the window at a glance.
type runCounts struct {
	Total    int
	Failed   int
	InFlight int
	Verdicts int
}

// groupState collapses a group to one console state — the single vocabulary
// every list, chip, and rank in the UI shares.
func groupState(g attemptGroup) string {
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
	switch g.LatestPhase {
	case "failed":
		return "failed"
	case "reconciling":
		return "reconciling"
	case "validated":
		return "validated"
	case "superseded":
		return "superseded"
	}
	return g.LatestPhase
}

// stateRank: failures first, then work in motion, then outcomes, then history.
func stateRank(state string) int {
	switch state {
	case "failed", "dispatch lost":
		return 0
	case "in flight", "reconciling", "queued":
		return 1
	case "verdict", "validated":
		return 2
	default:
		return 3
	}
}

// shortAttemptName renders an attempt CR name for humans: the workflow's
// tail plus a short hash. The full name stays one click away.
func shortAttemptName(name string) string {
	n := strings.TrimPrefix(name, "attempt-")
	if i := strings.LastIndex(n, "-"); i > 0 {
		wf, hash := n[:i], n[i+1:]
		if len(hash) > 8 {
			hash = hash[:8]
		}
		return wf + " · " + hash
	}
	return n
}

// attemptGroup is one rollup row: a PR (review class) or a workflow
// (scheduled classes) with its attempts expandable beneath it.
type attemptGroup struct {
	WorkflowRef   string
	Subject       string // PR pointer (review) or workflow ref (scheduled)
	IsReview      bool
	Count         int
	LatestPhase   string
	ClaimState    string // human claim state for reviews: in flight, queued, consumed, ...
	HeadSHA       string // review head SHA the latest attempt is armed/dispatched for
	LatestAttempt string // name of the most recently active attempt (wall + drill-down)
	LastActivity  string
	Attempts      []attemptSummary
}

// windowCutoff resolves the list window; unknown values fall back to 24h.
func windowCutoff(window string, now time.Time) time.Time {
	switch window {
	case "all":
		return time.Time{}
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	default: // "24h"
		return now.Add(-24 * time.Hour)
	}
}

// groupAttempts folds attempts into rollup rows (ADR-0007): reviews per PR,
// everything else per workflow. Order within a group: most recent first.
func groupAttempts(attempts []v1alpha1.Attempt, cutoff time.Time) []attemptGroup {
	byKey := map[string]*attemptGroup{}
	var order []string
	for _, a := range attempts {
		last := a.Status.LastRunAt.Time
		if last.IsZero() {
			last = a.CreationTimestamp.Time
		}
		if !cutoff.IsZero() && last.Before(cutoff) {
			continue
		}
		isReview := a.Spec.Objective.Kind == v1alpha1.ObjectiveKindPRReview
		subject := a.Spec.Objective.PrimarySubject.Object
		key := a.Spec.WorkflowRef + "/" + subject
		if isReview && a.Status.Review != nil {
			subject = a.Status.Review.PR
			key = a.Spec.WorkflowRef + "/" + subject
		}
		g, ok := byKey[key]
		if !ok {
			g = &attemptGroup{
				WorkflowRef: a.Spec.WorkflowRef,
				Subject:     subject,
				IsReview:    isReview,
			}
			byKey[key] = g
			order = append(order, key)
		}
		g.Count++
		lastRun := ""
		if !a.Status.LastRunAt.IsZero() {
			lastRun = a.Status.LastRunAt.Time.Format("2006-01-02 15:04")
		}
		g.Attempts = append(g.Attempts, attemptSummary{
			Name:           a.Name,
			WorkflowRef:    a.Spec.WorkflowRef,
			ObjectiveKind:  a.Spec.Objective.Kind,
			PrimarySubject: subject,
			Phase:          a.Status.Phase,
			LastRunAt:      lastRun,
			RunCount:       a.Status.TotalRuns(),
		})
		if last.Format(time.RFC3339) > g.LastActivity {
			g.LastActivity = last.Format(time.RFC3339)
			g.LatestPhase = a.Status.Phase
			g.LatestAttempt = a.Name
			if isReview && a.Status.Review != nil {
				g.ClaimState = claimState(a.Status.Review)
				g.HeadSHA = a.Status.Review.HeadSHA
			}
		}
	}
	groups := make([]attemptGroup, 0, len(order))
	for _, k := range order {
		g := byKey[k]
		sort.Slice(g.Attempts, func(i, j int) bool {
			return g.Attempts[i].LastRunAt > g.Attempts[j].LastRunAt
		})
		groups = append(groups, *g)
	}
	return groups
}

// claimState renders the gate claim's human state (ADR-0007).
func claimState(r *v1alpha1.ReviewClaimStatus) string {
	switch {
	case r.Released:
		switch r.ReleaseReason {
		case "consumed":
			return "verdict posted"
		case "superseded":
			return "superseded by newer head"
		case "dispatch-timeout":
			return "run expired"
		case "dispatch-lost":
			return "dispatch lost, re-arming"
		case "horizon":
			return "horizon reached"
		default:
			return "released"
		}
	case r.DispatchedAt != nil:
		return "review in flight"
	default:
		return "queued"
	}
}

// attemptDetailData is the template data for the run detail page
// (an Attempt rendered as a Run — the execution spine).
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
	TotalRuns      int
	TotalNodeRes   int
	TotalEvidence  int
	NodeResults    []v1alpha1.NodeResultEnvelope
	Evidence       []v1alpha1.EvidenceReference
	Owner          string
	AgentEnabled   bool
	Claim          *claimView // review-gate claim state (nil = not a gated attempt)
	LastRunAt      string
	TotalDuration  string       // earliest run start → latest run end ("" if unknown)
	Graph          runGraphView // timeline graph: compiled workflow + node state
	NodeDataJSON   template.JS  // hover payload (own json.Marshal output: safe raw)
}

// claimView is the review-gate claim state attached to a run.
type claimView struct {
	PR            string // host/owner/name#N (normalized)
	HeadSHA       string
	State         string // dispatched | released | armed/waiting…
	ArmedSince    string
	DispatchedAt  string
	Released      bool
	ReleaseReason string
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

	if !s.mayViewAttempt(att, identityFromContext(r.Context())) {
		s.renderError(w, r, "attempt not found")
		return
	}

	runs := make([]runSummary, 0, len(att.Status.Runs))
	var earliest, latest time.Time
	for _, run := range att.Status.Runs {
		started := ""
		ended := ""
		if !run.StartedAt.IsZero() {
			started = run.StartedAt.Time.Format("2006-01-02 15:04:05 MST")
			if earliest.IsZero() || run.StartedAt.Time.Before(earliest) {
				earliest = run.StartedAt.Time
			}
		}
		if !run.EndedAt.IsZero() {
			ended = run.EndedAt.Time.Format("2006-01-02 15:04:05 MST")
			if run.EndedAt.Time.After(latest) {
				latest = run.EndedAt.Time
			}
		}
		runs = append(runs, runSummary{
			Name:      run.Name,
			StartedAt: started,
			EndedAt:   ended,
			Phase:     run.Phase,
		})
	}
	// Total span needs BOTH ends: an in-flight run has StartedAt but no
	// EndedAt — subtracting a zero time would render a negative geological
	// age in the header.
	totalDuration := ""
	if !earliest.IsZero() && !latest.IsZero() {
		totalDuration = formatDuration(latest.Sub(earliest))
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
		TotalRuns:      att.Status.TotalRuns(),
		TotalNodeRes:   att.Status.TotalNodeResults(),
		TotalEvidence:  att.Status.TotalEvidence(),
		NodeResults:    att.Status.NodeResults,
		Evidence:       att.Status.Evidence,
		Owner:          att.Spec.Owner,
		AgentEnabled:   agentEnabled,
		LastRunAt:      formatMetaTime(att.Status.LastRunAt),
		TotalDuration:  totalDuration,
	}
	if rv := att.Status.Review; rv != nil {
		data.Claim = &claimView{
			PR:            rv.PR,
			HeadSHA:       rv.HeadSHA,
			State:         claimState(rv),
			Released:      rv.Released,
			ReleaseReason: rv.ReleaseReason,
			ArmedSince:    formatMetaTimePtr(rv.ArmedSince),
			DispatchedAt:  formatMetaTimePtr(rv.DispatchedAt),
		}
	}
	// The timeline graph: compiled workflow + per-node state + live position.
	// The page includes the same fragment the SSE stream re-renders.
	data.Graph = s.buildRunGraph(r.Context(), att)
	nodeJSON, err := json.Marshal(data.Graph.NodeData)
	if err != nil {
		nodeJSON = []byte("{}")
	}
	data.NodeDataJSON = template.JS(nodeJSON)
	s.render(w, r, "pages/attempt_detail.html", data)
}

// formatMetaTime renders a metav1.Time for dense display; zero → "".
func formatMetaTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Time.Format("2006-01-02 15:04:05 MST")
}

func formatMetaTimePtr(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return formatMetaTime(*t)
}

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

	if !s.mayViewAttempt(att, identityFromContext(r.Context())) {
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
	if !s.mayViewAttempt(att, identityFromContext(r.Context())) {
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

// chipState maps an attempt phase onto the shared chip vocabulary.
func chipState(phase string) string {
	switch phase {
	case "failed":
		return "failed"
	case "validated":
		return "validated"
	case "superseded":
		return "superseded"
	default:
		return "reconciling"
	}
}
