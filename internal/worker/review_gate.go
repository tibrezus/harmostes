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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batchv1 "k8s.io/api/batch/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/review"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// reDispatchGrace bounds how long an armed-but-never-dispatched claim
// holds its PR before a sweep releases it for refill (#279).
const reDispatchGrace = 5 * time.Minute

// jobDeathGrace lets a just-created Job's controller sync (a new Job
// reports no status until then — the race the dispatcher's dedupe also
// tolerates) before a sweep treats a missing live Job as a dead run.
const jobDeathGrace = 2 * time.Minute

// newReviewAPI is the seam the tests swap for a server-pinned API.
var newReviewAPI = func() review.API {
	return &review.RESTAPI{Client: http.DefaultClient, TokenLookup: os.Getenv}
}

// GateDeps are the Review-Ready Gate's collaborators (ADR-0007 phase 4).
// Claims live on Attempt CRs (Client); the Workflow status carries
// aggregates only (Status).
type GateDeps struct {
	Status StatusPatcher
	Client client.Client
	Scheme *runtime.Scheme
	// FleetMaxConcurrent is the chart default; spec.reviewReady.maxConcurrent
	// overrides per workflow.
	FleetMaxConcurrent int
	Log                func(format string, args ...any)
	TL                 timeline.Writer
}

func (d GateDeps) log() func(string, ...any) {
	if d.Log != nil {
		return d.Log
	}
	return func(string, ...any) {}
}

// GateDispatch is one accepted review: the Trigger Envelope to execute and
// the claim (Attempt) it dispatches under.
type GateDispatch struct {
	Envelope *review.Envelope
	Attempt  string // the claim's Attempt name → HARMOSTES_ATTEMPT on the Job
}

// RunReviewGateSweep evaluates the gate drain-to-capacity (ADR-0007): the
// dispatcher calls it per trigger — in-flight claims are checked for
// consume/expiry, then the oldest-first labeled set fills every free slot.
// A request-shaped wake jumps the queue as a priority candidate. Returns
// one dispatch per accepted review.
func RunReviewGateSweep(ctx context.Context, deps GateDeps, wf *v1alpha1.Workflow) ([]GateDispatch, error) {
	return runGate(ctx, deps, wf, false)
}

// RunReviewGateWake evaluates ONLY the wake PR — the manual/direct run path
// (harmostes-worker run without a dispatcher), which must never fan out into
// multiple dispatches.
func RunReviewGateWake(ctx context.Context, deps GateDeps, wf *v1alpha1.Workflow) ([]GateDispatch, error) {
	return runGate(ctx, deps, wf, true)
}

// candidate is a PR the gate may arm/dispatch this cycle.
type candidate struct {
	repo    string
	pr      int
	pointer string // host/owner/name#N (normalized)
	sha     string // wake revision, when the wake carried one
	isWake  bool
	request bool // request-shaped (label touched): may supersede a live claim
}

func runGate(ctx context.Context, deps GateDeps, wf *v1alpha1.Workflow, wakeOnly bool) ([]GateDispatch, error) {
	rr := wf.Spec.ReviewReady
	if rr == nil {
		return nil, nil // gate not configured for this workflow
	}
	log := deps.log()
	now := time.Now()
	capacity := rr.EffectiveMaxConcurrent(deps.FleetMaxConcurrent)
	api := newReviewAPI()
	label := rr.EffectiveLabel()

	// Live status read: the timeline-transition dedupe reads the CURRENT
	// aggregates, never the fetch-time snapshot (#257).
	var liveAgg *v1alpha1.ReviewReadyStatus
	if st, err := deps.Status.GetStatus(ctx, wf.Name); err == nil && st.ReviewReady != nil {
		liveAgg = st.ReviewReady
	}

	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}

	// ── A. In-flight claims: consume / expiry / refresh — never dispatch. ──
	liveDispatched := 0
	liveOn := map[string]bool{} // normalized pointer → live claim present
	for _, c := range claims {
		r := c.Status.Review
		liveOn[r.PR] = true
		if r.DispatchedAt == nil {
			continue // armed-queued: holds no capacity slot
		}
		liveDispatched++
		repo, pr, perr := parsePRPointer(r.PR)
		if perr != nil {
			releaseClaim(ctx, deps, c, "closed", log)
			continue
		}
		p := review.Params{
			Repo: repo, PR: pr, Label: r.Label,
			Horizon: rr.HorizonDuration(), DispatchTimeout: rr.DispatchTimeoutDuration(),
			ArmedSha: r.HeadSHA,
			Now:      now,
		}
		if r.ArmedSince != nil {
			p.ArmedAt = r.ArmedSince.Time
		}
		p.DispatchedAt = r.DispatchedAt.Time
		res := review.Evaluate(ctx, api, p)
		if res.Decision == review.DecisionStanddown {
			releaseClaim(ctx, deps, c, classifyRelease(res.Reason), log)
			emitGate(ctx, deps.TL, liveAgg, res, repo, pr)
			liveDispatched--
		}
	}
	// A dispatched claim whose Job is already terminal does not wait out
	// the DispatchTimeout: Job-per-attempt makes the death observable, so
	// recovery is fact-based (ListActiveJobs), not timer-based (#285).
	var liveJobs []batchv1.Job
	for _, c := range claims {
		if r := c.Status.Review; r.DispatchedAt != nil && time.Since(r.DispatchedAt.Time) > jobDeathGrace {
			var err error
			liveJobs, err = k8s.ListActiveJobs(ctx, deps.Client, wf.Namespace, wf.Name)
			if err != nil {
				log("review-ready: live-job list failed (%v) — deferring to the dispatch-timeout bound", err)
			}
			break
		}
	}
	if liveJobs != nil {
		for _, c := range claims {
			r := c.Status.Review
			if r.DispatchedAt == nil || time.Since(r.DispatchedAt.Time) <= jobDeathGrace {
				continue
			}
			live := false
			for _, j := range liveJobs {
				if j.Labels["harmostes.dev/attempt"] == c.Name {
					live = true
					break
				}
			}
			if !live {
				log("review-ready: claim %s (%s) has no live job — releasing as dispatch-lost", c.Name, r.PR)
				releaseClaim(ctx, deps, c, "dispatch-lost", log)
			}
		}
	}

	// Armed but never dispatched past the grace window: the sweep that
	// armed the claim died before its Job landed (crash, API blip, or
	// the #277 scheme-bug class). Release as dispatch-lost so this same
	// sweep's drain re-evaluates and re-fills the slot — the createMu
	// and live-Job dedupe make the refill safe (#279).
	for _, c := range claims {
		r := c.Status.Review
		if r.DispatchedAt != nil {
			continue
		}
		arm := time.Time{}
		if r.ArmedSince != nil {
			arm = r.ArmedSince.Time
		}
		if time.Since(arm) <= reDispatchGrace {
			continue // fresh arm: its sweep's dispatch loop is still in flight
		}
		log("review-ready: claim %s (%s) never dispatched — releasing as dispatch-lost", c.Name, r.PR)
		releaseClaim(ctx, deps, c, "dispatch-lost", log)
	}

	// Re-list: releases in the loop above must be visible to the drain
	// (liveOn/claims snapshots are stale the moment a claim releases).
	claims, err = attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil {
		return nil, fmt.Errorf("re-list claims: %w", err)
	}
	liveOn = make(map[string]bool, len(claims))
	liveDispatched = 0
	for _, c := range claims {
		liveOn[c.Status.Review.PR] = true
		if c.Status.Review.DispatchedAt != nil {
			liveDispatched++
		}
	}
	free := capacity - liveDispatched

	// ── B. Candidates: the wake (priority) + the labeled set (oldest first). ──
	var cands []candidate
	seen := map[string]bool{}
	addCand := func(c candidate) {
		if seen[c.pointer] {
			return
		}
		seen[c.pointer] = true
		cands = append(cands, c)
	}
	if wake := parseWake(wf); wake != nil {
		addCand(*wake)
	}
	if !wakeOnly {
		for _, repo := range scopeRepos(wf) {
			norm := normalizeRepoPointer(repo, wf)
			pulls, err := api.ListLabeledOpenPulls(ctx, norm, label)
			if err != nil {
				log("review-ready: labeled scan %s failed: %v", norm, err)
				continue
			}
			for _, pr := range pulls {
				addCand(candidate{repo: norm, pr: pr.Number,
					pointer: fmt.Sprintf("%s#%d", norm, pr.Number)})
			}
		}
	}

	// ── C. Evaluate + drain-to-capacity. ──────────────────────────────────
	var out []GateDispatch
	lastDecision, lastReason := "waiting", "nothing to evaluate this cycle"
	for _, cand := range cands {
		claimed := liveOn[cand.pointer]
		if claimed {
			// Request-shaped wakes may supersede (head moved since the
			// claim armed — an explicit human re-request); push-shaped and
			// scan candidates leave the in-flight claim alone (r5).
			if !cand.request {
				continue
			}
			claimFor := findClaim(claims, cand.pointer)
			if claimFor != nil && claimFor.Status.Review.HeadSHA == candSha(cand) {
				continue // same head — nothing to re-request
			}
			if claimFor != nil {
				if err := attempt.ReleaseClaim(ctx, deps.Client, wf.Namespace, claimFor.Name, "superseded"); err != nil {
					log("review-ready: supersede %s failed: %v", claimFor.Name, err)
					continue
				}
				liveOn[cand.pointer] = false
				if claimFor.Status.Review.DispatchedAt != nil {
					liveDispatched--
					free++
				}
			}
		}
		if free <= 0 {
			break // durable queue: the labeled set re-fills on the next sweep
		}

		res := review.Evaluate(ctx, api, review.Params{
			Repo: cand.repo, PR: cand.pr, Label: label,
			Horizon: rr.HorizonDuration(), DispatchTimeout: rr.DispatchTimeoutDuration(),
			WakeSHA: candSha(cand),
			Now:     now,
		})
		switch res.Decision {
		case review.DecisionProceed:
			sha := res.Envelope.HeadSHA
			at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, cand.pointer, sha, label)
			if err != nil {
				log("review-ready: arm claim %s failed: %v", cand.pointer, err)
				lastDecision, lastReason = string(res.Decision), res.Reason
				continue
			}
			out = append(out, GateDispatch{Envelope: res.Envelope, Attempt: at.Name})
			free--
			lastDecision, lastReason = string(res.Decision), res.Reason
			emitGate(ctx, deps.TL, liveAgg, res, cand.repo, cand.pr)
		case review.DecisionWaiting:
			sha := res.NewArmedSha
			if sha == "" {
				sha = candSha(cand)
			}
			if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, cand.pointer, sha, label); err != nil {
				log("review-ready: arm claim %s failed: %v", cand.pointer, err)
			}
			lastDecision, lastReason = string(res.Decision), res.Reason
			emitGate(ctx, deps.TL, liveAgg, res, cand.repo, cand.pr)
		case review.DecisionStanddown:
			lastDecision, lastReason = string(res.Decision), res.Reason
			emitGate(ctx, deps.TL, liveAgg, res, cand.repo, cand.pr)
		}
	}

	// ── D. Aggregates (the Workflow status stops being a hot field). ──
	if err := deps.Status.PatchStatus(ctx, wf.Name, func(s *v1alpha1.WorkflowStatus) {
		s.ReviewReady = &v1alpha1.ReviewReadyStatus{
			LiveClaims:   liveDispatched,
			Capacity:     capacity,
			LastDecision: lastDecision,
			LastReason:   lastReason,
		}
	}); err != nil {
		log("review-ready: aggregates patch failed: %v", err)
	}

	return out, nil
}

// parseWake reads the trigger annotations (env first — the controller clears
// annotations at schedule time). nil when this cycle has no wake.
func parseWake(wf *v1alpha1.Workflow) *candidate {
	trigPR := os.Getenv("HARMOSTES_TRIGGER_PR")
	if trigPR == "" {
		trigPR = wf.Annotations["harmostes.dev/trigger-pr"]
	}
	if trigPR == "" {
		return nil
	}
	action := os.Getenv("HARMOSTES_TRIGGER_ACTION")
	if action == "" {
		action = wf.Annotations["harmostes.dev/trigger-action"]
	}
	repo, pr, err := parsePRPointer(trigPR)
	if err != nil {
		return nil
	}
	repo = normalizeRepoPointer(repo, wf)
	if !repoInScope(wf, repo) {
		return nil // out-of-scope wake: arm nothing (defense-in-depth)
	}
	sha := wakeRevision(wf)
	return &candidate{
		repo: repo, pr: pr, pointer: fmt.Sprintf("%s#%d", repo, pr), sha: sha,
		isWake:  true,
		request: action == "labeled" || action == "unlabeled" || action == "label_updated",
	}
}

func candSha(c candidate) string { return c.sha }

func findClaim(claims []v1alpha1.Attempt, pointer string) *v1alpha1.Attempt {
	for i := range claims {
		if claims[i].Status.Review != nil && claims[i].Status.Review.PR == pointer && !claims[i].Status.Review.Released {
			return &claims[i]
		}
	}
	return nil
}

// classifyRelease maps a standdown reason onto the claim's release-reason
// vocabulary.
func classifyRelease(reason string) string {
	switch {
	case strings.Contains(reason, "consumed"):
		return "consumed"
	case strings.Contains(reason, "presumed dead"):
		return "dispatch-timeout"
	case strings.Contains(reason, "horizon exceeded"):
		return "horizon"
	case strings.Contains(reason, "closed"):
		return "closed"
	default:
		return "standdown"
	}
}

func releaseClaim(ctx context.Context, deps GateDeps, at v1alpha1.Attempt, reason string, log func(string, ...any)) {
	if err := attempt.ReleaseClaim(ctx, deps.Client, at.Namespace, at.Name, reason); err != nil {
		log("review-ready: release %s failed: %v", at.Name, err)
		return
	}
	log("review-ready: claim %s released (%s)", at.Name, reason)
}

// emitGateTransition records state CHANGES only: a re-evaluation that repeats
// the previous waiting decision+reason (the armed poll, ~every 5 min) is a
// non-event.
func emitGate(ctx context.Context, tl timeline.Writer, agg *v1alpha1.ReviewReadyStatus, result review.Result, repo string, pr int) {
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
		if agg != nil && agg.LastDecision == string(review.DecisionWaiting) && agg.LastReason == result.Reason {
			return // same waiting state — not a transition
		}
	}
	if kind == "" {
		return
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

// normalizeRepoPointer qualifies a repo pointer to host/owner/name. A bare
// "owner/name" resolves via a scope entry whose suffix matches (self-hosted
// Forgejo has no guessable host) or — matching ResolveHost's bare handling —
// github.com. Pointers that already carry a host pass through unchanged.
func normalizeRepoPointer(repo string, wf *v1alpha1.Workflow) string {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return repo // full form, or malformed (parse/scope checks reject)
	}
	for _, r := range scopeRepos(wf) {
		// Only a full-form scope entry supplies a host (self-hosted Forgejo);
		// a bare entry means "GitHub implied" (repoInScope), not a host.
		if len(strings.Split(r, "/")) == 3 && strings.HasSuffix(r, "/"+repo) {
			return r
		}
	}
	return "github.com/" + repo
}

// scopeRepos lists the configured repos verbatim (spec.config.repos).
func scopeRepos(wf *v1alpha1.Workflow) []string {
	if len(wf.Spec.Config) == 0 {
		return nil
	}
	var cfg struct {
		Repos []string `json:"repos"`
	}
	if err := jsonUnmarshalScope(wf.Spec.Config, &cfg); err != nil {
		return nil
	}
	return cfg.Repos
}

// repoInScope reports whether repo matches the instance's configured repos.
// repo arrives normalized to "host/owner/name" (normalizeRepoPointer); the
// config may store either that or bare "owner/name" (GitHub implied).
// An empty/missing config accepts nothing (fail closed).
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

// jsonUnmarshalScope indirection keeps encoding/json out of the gate's hot
// imports (single use).
func jsonUnmarshalScope(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
