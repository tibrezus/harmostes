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
//
// INVARIANT (r8 review, F1): gateSweepDeadline + arm-worst-case must stay
// comfortably inside reDispatchGrace. The sweep arms claims; the CALLER
// creates the Jobs and marks them dispatched. A sweep aborted by its own
// deadline strands armed-undispatched claims; reDispatchGrace is what
// keeps the NEXT sweep from eating them before their dispatch loop (or a
// jobAlive re-check) can speak. Retuning either constant alone re-opens
// the #343 churn loop.
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
	// Wake carries the TRIGGER EVENT that scheduled this run (#349): the
	// controller publishes it, the consumer hands it down with the run
	// request, and the gate turns it into the labeled-scan's leading
	// candidate. It must ride the event — the old parseWake scraped env
	// vars set on dispatched JOB pods (which skip the gate) and workflow
	// annotations the controller CLEARS at schedule time (anti-rapid-fire),
	// so in the worker-pool topology the wake never arrived: every labeled
	// re-apply armed as automatic, and the breaker's documented override
	// ("re-apply the label") was structurally dead.
	WakePR       string
	WakeAction   string
	WakeRevision string
}

// wake converts the threaded trigger event into the scan's leading
// candidate. nil when this run has no wake (poll-triggered sweeps and
// empty events) or the pointer is unparseable / out of scope.
func (d GateDeps) wake(wf *v1alpha1.Workflow) *candidate {
	if d.WakePR == "" {
		return nil
	}
	repo, pr, err := parsePRPointer(d.WakePR)
	if err != nil {
		return nil
	}
	repo = normalizeRepoPointer(repo, wf)
	if !repoInScope(wf, repo) {
		return nil // out-of-scope wake: arm nothing (defense-in-depth)
	}
	return &candidate{
		repo: repo, pr: pr, pointer: fmt.Sprintf("%s#%d", repo, pr), sha: d.WakeRevision,
		isWake:  true,
		request: d.WakeAction == "labeled" || d.WakeAction == "unlabeled" || d.WakeAction == "label_updated",
		labeled: d.WakeAction == "labeled",
	}
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

// gateSweepDeadline bounds the sweep itself (r7 P1): a healthy sweep is
// sub-second (label-filtered live lists, pointer-local arm reads), but the
// deadline is what makes that true under failure — without it, a slow API
// server pushes the sweep into the Job deadline, the run dies
// never-dispatched, and the churn guard converts infrastructure pressure
// into silently dropped reviews. A mid-sweep abort between arm and
// dispatch leaves an armed-but-undispatched claim whose rescue is NOT this
// timer's expiry but the NEXT sweep's liveness re-check: jobAlive finds no
// Job for the claim and — after the reDispatchGrace window protects a
// genuinely in-flight dispatch — releases it dispatch-lost (r8 P4.4: do
// not retune this deadline expecting it to rescue claims; it only stops
// the sweep from running unbounded).
const gateSweepDeadline = 2 * time.Minute

func runGate(ctx context.Context, deps GateDeps, wf *v1alpha1.Workflow, wakeOnly bool) ([]GateDispatch, error) {
	rr := wf.Spec.ReviewReady
	if rr == nil {
		return nil, nil // gate not configured for this workflow
	}
	ctx, cancel := context.WithTimeout(ctx, gateSweepDeadline)
	defer cancel()
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
	heldRecorded := false // pass A preserved a live Job — its reason wins the sweep (#331, r8 F2)

	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
			if reason == v1alpha1.ReleaseReasonDispatchTimeout && jobAlive(c.Name) {
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
				heldRecorded = true
				if deps.TL != nil && (liveAgg == nil || liveAgg.LastReason != heldReason) {
					_ = deps.TL.Emit(ctx, timeline.KindGateStanddown, "", map[string]any{"reason": heldReason, "pr": pr, "repo": repo, "jobAlive": true})
				}
				continue
			}
			if reason == v1alpha1.ReleaseReasonDispatchTimeout {
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
				releaseDeadClaim(ctx, deps, c, v1alpha1.ReleaseReasonDispatchLost, log)
			}
		}
	}

	// Armed but never dispatched past the grace window: the sweep that
	// armed the claim died before its Job landed (crash, API blip, or
	// the #277 scheme-bug class). Release as dispatch-lost so this same
	// sweep's drain re-evaluates and re-fills the slot — the createMu
	// and live-Job dedupe make the refill safe (#279).
	//
	// ABORT-AWARE (r8 F1): never release on an aborted sweep. A sweep
	// that hit gateSweepDeadline is an unreliable observer — it may have
	// armed claims whose dispatch loop never ran; releasing them here
	// burns their churn budget for a dispatch that was never attempted,
	// and three such sweeps refuse a labeled, green PR. The claims are
	// not lost: the next HEALTHY sweep re-checks them.
	// FAIL-CLOSED on unknown liveness (r11 must-fix 3): an empty snapshot
	// on jobListErr means "we could not tell" — and on the
	// Create-succeeded-mark-failed case the claim's Job is RUNNING, so
	// releasing it there is the r9 (b) bug class on the error path. Same
	// polarity as passes A/B ("a failed list latches for the whole sweep").
	jobsKnown := func() bool { activeJobs(); return jobListErr == nil }
	if ctx.Err() != nil {
		log("review-ready: sweep aborted (ctx: %v) — skipping the never-dispatched release pass (unreliable observer)", ctx.Err())
	} else if !jobsKnown() {
		log("review-ready: live-job list failed (%v) — skipping the never-dispatched release pass (release is destructive; unknown must fail closed)", jobListErr)
	} else {
		for _, c := range claims {
			r := c.Status.Review
			// DispatchedAt == nil means "nobody ran MarkClaimDispatched",
			// NOT "no Job": the dispatcher can Create the Job and fail the
			// mark, and the live-Job dedupe continues before the mark. Both
			// leave a RUNNING review with a nil marker — releasing it here
			// spends the churn budget on a live run (r9 (b)). The jobAlive
			// snapshot is already memoised; the other two passes consult it
			// and so does this one now.
			if r.DispatchedAt != nil || jobAlive(c.Name) {
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
			// r6 P1: the age bound is now UNIFORM — aged or not, a
			// never-dispatched release is dispatch-lost (we never dispatched:
			// that is what the reason says), bumping DispatchLostReleases.
			// r4 released aged claims as HORIZON, which was the ambiguity
			// dismissal — the churn guard then refused any re-arm of a
			// labeled PR whose dispatch kept failing for weather: "we stopped
			// asking" and "we could not dispatch" collapsed into one clock.
			// The counter bounds the cycle instead (reuse < Max, guard at
			// Max), and ReleaseReasonHorizon stays reserved for genuine
			// ambiguity (verdict-window expiry on dispatched claims).
			log("review-ready: claim %s (%s) never dispatched — releasing as dispatch-lost (release #%d, aged=%t)", c.Name, r.PR, r.DispatchLostReleases+1, !arm.IsZero() && time.Since(arm) > rr.HorizonDuration())
			releaseClaim(ctx, deps, c, v1alpha1.ReleaseReasonDispatchLost, log)
		}
	}

	// Re-list: releases in the loop above must be visible to the drain
	// (liveOn/claims snapshots are stale the moment a claim releases).
	claims, err = attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	capacityFull := false

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
	if wake := deps.wake(wf); wake != nil {
		addCand(*wake)
	}
	// (c) A saturated fleet skips the scan: the labeled List is the one
	// unbounded-per-repo call in the sweep, and when no slot is free its
	// candidates cannot dispatch anyway — one bounded live-list plus the
	// wake is the whole sweep (r9 P6(c)). The saturation is RECORDED here
	// (r11 pillar 7): with the scan skipped, the queue break below never
	// fires on a saturated sweep, so this is the site that must speak.
	if !wakeOnly && free <= 0 {
		capacityFull = true
	}
	if !wakeOnly && free > 0 {
		for _, repo := range scopeRepos(wf) {
			norm := normalizeRepoPointer(repo, wf)
			pulls, err := api.ListLabeledOpenPulls(ctx, norm, label)
			if err != nil {
				log("review-ready: labeled scan %s failed: %v", norm, err)
				// "We could not check CI" must not be identical to
				// "nothing was labeled" (r9 (d)).
				recordReviewGateReason(ctx, wf.Name, norm, "scan-error")
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
			// Saturation is a REASON, not "nothing to evaluate" (r11):
			// "why is this labeled PR not being reviewed?" must answer
			// "capacity full", and section D writes what we record here.
			capacityFull = true
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
				if isIntentionalStop(err) {
					standDown(ctx, deps, liveAgg, wf.Name, cand, err, log, &lastDecision, &lastReason, &heldRecorded)
					continue // before free--: a refusal must not consume a slot
				}
				log("review-ready: arm claim %s failed: %v", cand.pointer, err)
				// A ctx-bound arm failure is the pressure signal (r8 F1):
				// count it or the fleet stops reviewing at zero on the
				// only series the post-deploy review reads.
				recordReviewGate(ctx, wf.Name, cand.repo, err)
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
				if isIntentionalStop(err) {
					standDown(ctx, deps, liveAgg, wf.Name, cand, err, log, &lastDecision, &lastReason, &heldRecorded)
					continue
				}
				log("review-ready: arm claim %s failed: %v", cand.pointer, err)
				recordReviewGate(ctx, wf.Name, cand.repo, err)
			}
			lastDecision, lastReason = string(res.Decision), res.Reason
			emitGate(ctx, deps.TL, liveAgg, res, cand.repo, cand.pr)
		case review.DecisionStanddown:
			lastDecision, lastReason = string(res.Decision), res.Reason
			emitGate(ctx, deps.TL, liveAgg, res, cand.repo, cand.pr)
		}
	}

	// ── D. Aggregates (the Workflow status stops being a hot field). ──
	// Durable records speak on a ctx the deadline CANNOT cancel (r8 (e)):
	// an aborted sweep must still write its summary and its counters, or
	// the failure mode this deadline exists for is invisible in it.
	recordCtx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		// The abort itself is countable — the effect of this safeguard is
		// falsifiable from telemetry, not only from a log grep. repo stays
		// "" (r11 nit): "sweep" in a pointer-typed label breaks group-by-repo.
		recordReviewGateReason(recordCtx, wf.Name, "", "sweep-abort")
	}
	if !heldRecorded {
		// The status a human reads first must carry the real cause (r11
		// pillar 7): "nothing to evaluate" on an aborted or saturated sweep
		// is the misleading signal the aggregates exist to prevent. A held
		// headline (a live run outranks a refusal as news) still wins.
		if ctx.Err() != nil {
			lastDecision, lastReason = "waiting", fmt.Sprintf("sweep aborted before completion: %v", ctx.Err())
		} else if capacityFull {
			lastDecision, lastReason = "waiting", fmt.Sprintf("capacity full (live=%d/cap=%d) — labeled PRs queue for the next sweep", liveDispatched, capacity)
		}
	}
	if err := deps.Status.PatchStatus(recordCtx, wf.Name, func(s *v1alpha1.WorkflowStatus) {
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

func candSha(c candidate) string { return c.sha }

func findClaim(claims []v1alpha1.Attempt, pointer string) *v1alpha1.Attempt {
	for i := range claims {
		if claims[i].Status.Review != nil && claims[i].Status.Review.PR == pointer && !claims[i].Status.Review.Released {
			return &claims[i]
		}
	}
	return nil
}

// standDown records an intentional stop (breaker, churn guard) ONE way:
// log + Workflow status + a durable gate timeline row. "Why is this labeled
// PR not being reviewed?" must be answerable from pod logs or the durable
// history alone — and the proceed branch is where the guard is MOST likely
// to fire (labeled PR, green CI), so both arm call sites share this (r4 P7).
func standDown(ctx context.Context, deps GateDeps, liveAgg *v1alpha1.ReviewReadyStatus, wfName string, cand candidate, err error, log func(string, ...any), lastDecision, lastReason *string, heldRecorded *bool) {
	// A refusal must not clobber a stronger reason recorded earlier in the
	// same sweep: pass A's "(held: Job still alive)" (#331) is a durability
	// promise about a live run — a later candidate's refusal is metadata,
	// not a contradiction. Tracked as a FLAG, not by sniffing the prose:
	// the reason sentence belongs to the review package and may reword.
	// (r8 F2: the HasPrefix form here was dead code — the marker is a
	// suffix — and no test noticed.)
	// PREFERENCE, not a mute (r10): the held reason stays as the sweep's
	// headline (a live run outranks a refusal as news), but the refusal is
	// still fully recorded — counted on the counter and emitted to the
	// timeline for ITS candidate. Muting it made the turned-away candidate
	// invisible in exactly the round the counter exists for.
	log("review-ready: %s: %v", cand.pointer, err)
	recordReviewGate(ctx, wfName, cand.repo, err)
	if !*heldRecorded {
		*lastDecision, *lastReason = "standdown", err.Error()
	}
	emitGate(ctx, deps.TL, liveAgg, review.Result{
		Evaluation:  review.Evaluation{Decision: review.DecisionStanddown, Reason: err.Error()},
		NewArmedSha: "",
	}, cand.repo, cand.pr)
}

// isIntentionalStop reports arm refusals that are the system stopping ON
// PURPOSE (#328 breaker, #343 churn guard) — surfaced as standdowns, never
// as failures. One predicate so a third sentinel cannot be swallowed by a
// call site forgetting to extend its list (r2 P3).
func isIntentionalStop(err error) bool {
	return errors.Is(err, attempt.ErrDeadDispatchBreaker) ||
		errors.Is(err, attempt.ErrRecentlyDismissed) ||
		errors.Is(err, attempt.ErrChurnBudgetExhausted)
}

// classifyRelease maps a standdown reason onto the claim's release-reason
// vocabulary. "consumed"/"closed"/"superseded"/"standdown" stay bare
// literals on purpose — terminal classes nothing branches on (the open-
// string note lives on the ReleaseReason const block in api/v1alpha1).
func classifyRelease(reason string) string {
	switch {
	case strings.Contains(reason, "consumed"):
		return "consumed"
	case strings.Contains(reason, "presumed dead"):
		return v1alpha1.ReleaseReasonDispatchTimeout
	case strings.Contains(reason, "horizon exceeded"):
		return v1alpha1.ReleaseReasonHorizon
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
