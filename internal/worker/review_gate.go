package worker

import (
	"context"
	"encoding/json"
	"errors"
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
	// labeled: the wake was the label being APPLIED — the breaker's human
	// override. unlabeled/label_updated touch the label without asking for
	// a retry, so they must not reset the dead-dispatch counter (#328).
	labeled bool
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
	lastDecision, lastReason := "waiting", "nothing to evaluate this cycle"

	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil {
		return nil, fmt.Errorf("list claims: %w", err)
	}

	// One lazily-fetched active-Job snapshot per sweep — fetched once,
	// error and all: a failed list latches for the whole sweep so the
	// fact pass is skipped too. Release is destructive (breaker strike +
	// ledger finalization), so unknown must fail closed. Shared by the
	// timer pass (dispatch-timeout) and the job-death pass: a run that is
	// observably alive is never counted dead (#331).
	var (
		jobSnapshot []batchv1.Job
		jobListErr  error
		jobsFetched bool
	)
	activeJobs := func() []batchv1.Job {
		if !jobsFetched {
			jobSnapshot, jobListErr = k8s.ListActiveJobs(ctx, deps.Client, wf.Namespace, wf.Name)
			if jobListErr != nil {
				log("review-ready: live-job list failed (%v) — deferring to the dispatch-timeout bound", jobListErr)
			}
			jobsFetched = true
		}
		return jobSnapshot
	}
	jobAlive := func(attemptName string) bool {
		for _, j := range activeJobs() {
			if j.Labels[v1alpha1.AttemptLabel] == attemptName {
				return true
			}
		}
		return false
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
			reason := classifyRelease(res.Reason)
			if reason == "dispatch-timeout" && jobAlive(c.Name) {
				// The bound presumes death; the Job is observably still
				// alive (slow deadline enforcement, clock skew). The fact
				// wins: keep the claim live — it holds its slot — and let
				// the job-death pass or the next sweep classify from fact
				// once the Job is terminal. The disagreement is recorded
				// (aggregates + timeline, transition-deduped) so a held
				// slot is visible in the durable history (#331).
				log("review-ready: claim %s (%s) past DispatchTimeout but its Job is still alive — not counting a death", c.Name, r.PR)
				heldReason := res.Reason + " (held: Job still alive)"
				if lastDecision != string(res.Decision) {
					lastDecision, lastReason = string(res.Decision), heldReason
				}
				if deps.TL != nil && (liveAgg == nil || liveAgg.LastReason != heldReason) {
					_ = deps.TL.Emit(ctx, timeline.KindGateStanddown, "", map[string]any{"reason": heldReason, "pr": pr, "repo": repo, "jobAlive": true})
				}
				continue
			}
			if reason == "dispatch-timeout" {
				// A dispatched review presumed dead without a verdict IS a
				// dead dispatch: the breaker counts it and the ledger
				// finalizes the run (#328).
				releaseDeadClaim(ctx, deps, c, reason, log)
			} else {
				releaseClaim(ctx, deps, c, reason, log)
			}
			emitGate(ctx, deps.TL, liveAgg, res, repo, pr)
			liveDispatched--
		}
	}
	// A dispatched claim whose Job is already terminal does not wait out
	// the DispatchTimeout: Job-per-attempt makes the death observable, so
	// recovery is fact-based (ListActiveJobs), not timer-based (#285).
	// Shares the sweep's one job snapshot with the timer pass (#331).
	fetched := false
	for _, c := range claims {
		if r := c.Status.Review; r.DispatchedAt != nil && time.Since(r.DispatchedAt.Time) > jobDeathGrace {
			activeJobs()
			fetched = true
			break
		}
	}
	if fetched && jobListErr == nil {
		for _, c := range claims {
			r := c.Status.Review
			if r.DispatchedAt == nil || time.Since(r.DispatchedAt.Time) <= jobDeathGrace {
				continue
			}
			if !jobAlive(c.Name) {
				log("review-ready: claim %s (%s) has no live job — releasing as dispatch-lost", c.Name, r.PR)
				releaseDeadClaim(ctx, deps, c, "dispatch-lost", log)
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
		// Never dispatched: infrastructure weather, not a dead review —
		// the breaker must NOT count it (only dispatched deaths do).
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
				// Same head — nothing to re-request, UNLESS this is the
				// breaker's human override: an explicit label re-apply on
				// a head that has recorded dead dispatches (#328). The
				// supersede below is uncounted; the re-arm resets the
				// counter and spends a fresh dispatch.
				if !(cand.labeled && claimFor.Status.Review.DeadDispatches > 0) {
					continue
				}
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
			at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, cand.pointer, sha, label, cand.labeled)
			if err != nil {
				if errors.Is(err, attempt.ErrDeadDispatchBreaker) || errors.Is(err, attempt.ErrRecentlyDismissed) {
					// Surface the breaker / churn guard as the decision, not
					// a failure: the system stopped ON PURPOSE and says why
					// (#328, #343).
					log("review-ready: %s: %v", cand.pointer, err)
					lastDecision, lastReason = "standdown", err.Error()
					continue
				}
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
			if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, cand.pointer, sha, label, cand.labeled); err != nil {
				if errors.Is(err, attempt.ErrRecentlyDismissed) {
					// Churn guard: this pointer's era was horizon-dismissed —
					// no re-arm without a human request (#343).
					lastDecision, lastReason = "standdown", err.Error()
					continue
				}
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
		labeled: action == "labeled",
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

// releaseDeadClaim releases a dispatched claim that died without a verdict:
// the breaker counts the death and the ledger finalizes the run (#328).
// Idempotent — when both release passes observe the same death, the second
// is a no-op and says so.
func releaseDeadClaim(ctx context.Context, deps GateDeps, at v1alpha1.Attempt, reason string, log func(string, ...any)) {
	recorded, dead, err := attempt.ReleaseClaimDead(ctx, deps.Client, at.Namespace, at.Name, reason)
	if err != nil {
		log("review-ready: dead release %s failed: %v", at.Name, err)
		return
	}
	if recorded {
		log("review-ready: claim %s released (%s, dead dispatch #%d)", at.Name, reason, dead)
		return
	}
	log("review-ready: claim %s already released — death already recorded (#%d)", at.Name, dead)
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
