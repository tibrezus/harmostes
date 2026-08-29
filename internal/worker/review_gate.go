package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/review"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// newReviewAPI is the seam the tests swap for a server-pinned API.
var newReviewAPI = func() review.API {
	return &review.RESTAPI{Client: http.DefaultClient, TokenLookup: os.Getenv}
}

// reviewGate evaluates the Review-Ready Gate (ADR-0006) when the workflow is
// woken by a pull_request event or is armed from a previous wake. It returns
// the Trigger Envelope when the gate proceeds, nil otherwise (waiting, stood
// down, or gate not configured / nothing armed).
//
// The gate's armed state lives on the Workflow status (persisted via the
// status patcher). Cost per sweep: armed → one PR fetch (+ protection,
// contexts, and a verdict-window comment scan while dispatched); unarmed →
// one cheap list call per scope repo (the #249 backlog pass).
// RunReviewGate is the PRODUCTION seam: the one-shot worker calls it after
// template resolution and BEFORE graph execution (the pipeline.Run path is
// legacy — production has routed through graph.ExecuteGraph since #177).
// Returns the Trigger Envelope on proceed; nil otherwise (waiting, stood
// down, or gate not configured). The caller exits before provisioning when
// nil is returned for a gated workflow.
func RunReviewGate(ctx context.Context, status StatusPatcher, logFn func(string, ...any), wf *v1alpha1.Workflow) *review.Envelope {
	return RunReviewGateWithTimeline(ctx, status, logFn, wf, nil)
}

// RunReviewGateWithTimeline also appends gate lifecycle transitions to the
// timeline evidence layer (armed / waiting / proceed / standdown — state
// CHANGES only, not every armed-poll re-evaluation).
func RunReviewGateWithTimeline(ctx context.Context, status StatusPatcher, logFn func(string, ...any), wf *v1alpha1.Workflow, tl timeline.Writer) *review.Envelope {
	deps := Deps{Status: status, Log: logFn, TL: tl}
	return reviewGate(ctx, deps, wf)
}

func reviewGate(ctx context.Context, deps Deps, wf *v1alpha1.Workflow) *review.Envelope {
	rr := wf.Spec.ReviewReady
	if rr == nil {
		return nil // gate not configured for this workflow
	}

	// Decisions start from LIVE state, never the run-start snapshot (#257):
	// the consumer fetched wf when the trigger arrived — a long evaluate, a
	// parallel writer, or a lost marker in between makes the snapshot lie,
	// and the gate re-proceeds forever (the live #257 incident).
	if live, err := deps.Status.GetStatus(ctx, wf.Name); err != nil {
		deps.log()("review-ready: live status read failed (%v) — falling back to run-start snapshot", err)
	} else {
		wf.Status = *live
	}
	armed := wf.Status.ReviewReady
	// The controller clears the trigger annotations at schedule time (it must,
	// or isDue rapid-fires), so the PR pointer rides the TriggerEvent payload
	// through the consumer into the one-shot worker's environment. The
	// annotation is the fallback for direct/manual invocations.
	trigPR := os.Getenv("HARMOSTES_TRIGGER_PR")
	if trigPR == "" {
		trigPR = wf.Annotations["harmostes.dev/trigger-pr"]
	}
	action := os.Getenv("HARMOSTES_TRIGGER_ACTION")
	if action == "" {
		action = wf.Annotations["harmostes.dev/trigger-action"]
	}

	var repo string
	var pr int
	switch {
	case trigPR != "":
		r, n, err := parsePRPointer(trigPR)
		if err != nil {
			deps.log()("review-ready: bad trigger annotation %q: %v", trigPR, err)
			return nil
		}
		repo, pr = r, n
	case armed != nil && armed.ArmedPR != 0:
		repo, pr = armed.ArmedRepo, armed.ArmedPR
	default:
		// Nothing armed, no wake event: backlog pass (#249). A label added
		// while the gate was busy on another PR produces no further event —
		// the newest label stole the single armed slot and the older labeled
		// PRs starve. Discover the oldest labeled open PR across the scope
		// and arm it; evaluation stays single-flight, only arming becomes
		// queue-aware.
		r, n := oldestLabeledOpen(ctx, deps, wf)
		if n == 0 {
			return nil // nothing labeled anywhere in scope — genuinely idle
		}
		repo, pr = r, n
		deps.log()("review-ready: backlog pass arming pr=%d (%s)", pr, repo)
	}

	params := review.Params{
		Repo:            repo,
		PR:              pr,
		Label:           rr.EffectiveLabel(),
		Horizon:         rr.HorizonDuration(),
		DispatchTimeout: rr.DispatchTimeoutDuration(),
		DisarmHint:      action == "closed",
		WakeSHA:         wakeRevision(wf),
	}
	if armed != nil {
		params.ArmedSha = armed.ArmedSha
		if armed.ArmedSince != nil {
			params.ArmedAt = armed.ArmedSince.Time
		}
		// A durable dispatch marker (set at proceed, preserved across
		// in-flight waiting, cleared on consume) — LastDecision alone is
		// overwritten by every evaluation and evaporates after one sweep.
		if armed.DispatchedAt != nil {
			params.DispatchedAt = armed.DispatchedAt.Time
		}
	}
	// A wake event targeting a DIFFERENT PR than the armed one re-targets
	// the gate ONLY when the wake is request-shaped (a label was touched —
	// a human or the skill asked for something). A synchronize/push wake on
	// an unrelated PR must NOT hijack an armed review: observed live when a
	// push on #1577 re-targeted an armed #1566, whose "label absent"
	// standdown then disarmed it. The armed PR's OWN synchronize arrives as
	// its own wake and re-arms its head correctly.
	if armed != nil && trigPR != "" && armed.ArmedPR != 0 && (armed.ArmedRepo != repo || armed.ArmedPR != pr) {
		switch action {
		case "labeled", "unlabeled", "label_updated":
			// request-shaped: re-target (reset the armed head AND the
			// dispatch marker — it belongs to the OLD armed PR's review;
			// carried across, Evaluate would scan the NEW PR's comments
			// for a verdict that only ever posts on the old one, and the
			// new PR waits "in flight" up to a full Horizon).
			params.ArmedSha = ""
			params.ArmedAt = time.Time{}
			params.DispatchedAt = time.Time{}
		default:
			// push-shaped on another PR: ignore — keep the armed review
			deps.log()("review-ready: wake for pr=%d (%s) while armed on pr=%d — ignored, armed review preserved", pr, action, armed.ArmedPR)
			return nil
		}
	}

	// Scope allowlist (defense-in-depth): spec.config.repos declares which
	// repos this instance serves. A signed webhook from an undeclared repo
	// (mis-pointed hook) stands down instead of arming.
	if !repoInScope(wf, repo) {
		deps.log()("review-ready: %s not in spec.config.repos — ignoring wake", repo)
		patchIdle(ctx, deps, wf.Name, "wake for out-of-scope repo "+repo)
		return nil
	}

	result := review.Evaluate(ctx, newReviewAPI(), params)

	// Persist the armed state (and the decision, for the UI).
	var since *time.Time
	if !result.NewArmedAt.IsZero() {
		t := result.NewArmedAt
		since = &t
	}
	if err := deps.Status.PatchStatus(ctx, wf.Name, func(s *v1alpha1.WorkflowStatus) {
		if result.NewArmedSha == "" {
			// Standdown/idle: armed slot released — any dispatch marker
			// goes with it (consumed, horizon, closed).
			s.ReviewReady = &v1alpha1.ReviewReadyStatus{
				LastDecision: string(result.Decision),
				LastReason:   result.Reason,
			}
		} else {
			// Armed persists: set the dispatch marker at proceed; for waiting
			// decisions PRESERVE the live marker — same claim still in
			// flight. The run-start snapshot (`armed`) is not authoritative
			// here: deriving from it nils a marker another writer set after
			// this run fetched, and the gate re-proceeds forever (#257).
			// A different live claim (armedPR/armedSha mismatch — a retarget
			// or a newer arm) means the marker belongs to the OLD review:
			// cleared with it.
			var dispatched *metav1.Time
			if result.Decision == review.DecisionProceed {
				// Restamp unconditionally: a proceed is a NEW dispatch — the
				// previous marker's review has ended (verdict consumed or the
				// claim failed) — so the dispatch-timeout clock restarts with
				// it. Waiting paths preserve the live marker instead (below).
				now := metav1.Now()
				dispatched = &now
			} else if live := s.ReviewReady; live != nil &&
				live.ArmedPR == pr && live.ArmedSha == result.NewArmedSha {
				dispatched = live.DispatchedAt
				if live.ArmedSince != nil {
					since = &live.ArmedSince.Time // the live horizon clock wins
				}
			}
			s.ReviewReady = &v1alpha1.ReviewReadyStatus{
				ArmedRepo:    repo,
				ArmedPR:      pr,
				ArmedSha:     result.NewArmedSha,
				ArmedSince:   metaTime(since),
				DispatchedAt: dispatched,
				LastDecision: string(result.Decision),
				LastReason:   result.Reason,
			}
		}
	}); err != nil {
		deps.log()("review-ready: status patch failed: %v", err)
	}

	deps.log()("review-ready: %s pr=%d — %s", result.Decision, pr, result.Reason)
	emitGateTransition(ctx, deps.TL, armed, result, repo, pr)
	if result.Decision == review.DecisionProceed {
		return result.Envelope
	}
	return nil
}

// emitGateTransition records state CHANGES only: a re-evaluation that repeats
// the previous waiting decision+reason (the armed poll, ~every 5 min) is a
// non-event.
func emitGateTransition(ctx context.Context, tl timeline.Writer, armed *v1alpha1.ReviewReadyStatus, result review.Result, repo string, pr int) {
	if tl == nil {
		return
	}
	kind := ""
	switch result.Decision {
	case review.DecisionProceed:
		kind = timeline.KindGateProceed
	case review.DecisionStanddown:
		kind = timeline.KindGateStanddown
	case review.DecisionWaiting:
		kind = timeline.KindGateWaiting
		if armed != nil && armed.LastDecision == string(review.DecisionWaiting) && armed.LastReason == result.Reason {
			return // same waiting state — not a transition
		}
	}
	if kind == "" {
		return
	}
	if result.NewArmedSha != "" && (armed == nil || armed.ArmedSha != result.NewArmedSha) {
		_ = tl.Emit(ctx, timeline.KindGateArmed, "", map[string]any{"pr": pr, "repo": repo, "sha": result.NewArmedSha})
	}
	_ = tl.Emit(ctx, kind, "", map[string]any{"reason": result.Reason, "pr": pr, "repo": repo})
}

// wakeRevision returns the wake event's trigger-revision (env first — the
// controller clears annotations at schedule time — annotation fallback).
func wakeRevision(wf *v1alpha1.Workflow) string {
	if rev := os.Getenv("HARMOSTES_TRIGGER_REVISION"); rev != "" {
		return rev
	}
	return wf.Annotations["harmostes.dev/trigger-revision"]
}

// repoInScope reports whether repo matches the instance's configured repos.
// The config stores either "host/owner/name" or bare "owner/name" (GitHub);
// both forms must match the annotation's normalized "host/owner/name".
// An empty/missing config accepts nothing (fail closed).
// scopeRepos lists the configured repos verbatim (spec.config.repos).
func scopeRepos(wf *v1alpha1.Workflow) []string {
	if len(wf.Spec.Config) == 0 {
		return nil
	}
	var cfg struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(wf.Spec.Config, &cfg); err != nil {
		return nil
	}
	return cfg.Repos
}

func repoInScope(wf *v1alpha1.Workflow, repo string) bool {
	for _, r := range scopeRepos(wf) {
		if r == repo {
			return true
		}
		// bare 2-segment "owner/name" form: GitHub implied
		if len(strings.Split(r, "/")) == 2 && repo == "github.com/"+r {
			return true
		}
	}
	return false
}

func patchIdle(ctx context.Context, deps Deps, name, reason string) {
	// Preserve any armed state: an out-of-scope wake must not disarm a
	// legitimately armed in-scope PR (self-heals, but do not cause it).
	if err := deps.Status.PatchStatus(ctx, name, func(s *v1alpha1.WorkflowStatus) {
		if s.ReviewReady != nil {
			s.ReviewReady.LastDecision = "idle"
			s.ReviewReady.LastReason = reason
			return
		}
		s.ReviewReady = &v1alpha1.ReviewReadyStatus{LastDecision: "idle", LastReason: reason}
	}); err != nil {
		deps.log()("review-ready: status patch failed: %v", err)
	}
}

// parsePRPointer splits "host/owner/name#N" (the webhook's annotation form).
func parsePRPointer(s string) (string, int, error) {
	i := strings.LastIndex(s, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("missing #")
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("bad PR number")
	}
	repo := s[:i]
	if !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("bad repo path")
	}
	return repo, n, nil
}

func metaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	m := metav1.NewTime(*t)
	return &m
}

// oldestLabeledOpen returns the oldest labeled open PR of the FIRST scope
// repo (config order) that has one. Per-repo oldest (host API sorts
// oldest-first); cross-repo ordering is config order, not updated_at — with
// the single-repo scopes this instance runs, the distinction is moot, and
// the comment says what the code does. API-shaped failures degrade to
// "nothing found" — a broken listing must not wedge the gate, the next sweep
// retries.
func oldestLabeledOpen(ctx context.Context, deps Deps, wf *v1alpha1.Workflow) (string, int) {
	api := newReviewAPI()
	rrCfg := wf.Spec.ReviewReady
	if rrCfg == nil {
		rrCfg = &v1alpha1.ReviewReadySpec{}
	}
	label := rrCfg.EffectiveLabel()
	for _, repo := range scopeRepos(wf) {
		prs, err := api.ListLabeledOpenPulls(ctx, repo, label)
		if err != nil {
			deps.log()("review-ready: backlog list failed for %s: %v", repo, err)
			continue
		}
		if len(prs) > 0 {
			return repo, prs[0].Number
		}
	}
	return "", 0
}
