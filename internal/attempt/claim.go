package attempt

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// Review-Ready Gate claims (ADR-0007 phase 4): the Attempt IS the claim.
// Arming creates/refreshes the claim (created at ARM time so waiting state
// persists across sweeps); dispatch marks it; releases free its capacity
// slot. All writes are optimistic-locked status patches — one owner (the
// gate), so the #257 lost-update class cannot recur on claims.

// ErrDeadDispatchBreaker is returned by ArmClaim when the head's dead
// dispatches hit MaxDeadDispatchesPerHead (#328): dispatched reviews of
// this exact head died without a verdict repeatedly, so automatic re-arm
// is refused — the honest duration of this review exceeds the run bound,
// and re-arming would only burn another cycle. Reset by a new head push or
// an explicit label wake (human override). Wrap, never replace: callers
// match with errors.Is.
var ErrDeadDispatchBreaker = errors.New("dead-dispatch breaker open")

// ErrRecentlyDismissed is returned by ArmClaim when the SAME head's latest
// claim was dismissed by the horizon: automatic re-arm would restart the
// identical ambiguity with no new information (#343). A human request
// (label re-apply, or a push followed by one) overrides.
var ErrRecentlyDismissed = errors.New("recently dismissed by horizon")

// ErrChurnBudgetExhausted is returned by ArmClaim when the SAME head burned
// through MaxDispatchLostReleases consecutive never-dispatched releases: the
// release/revive cycle must stop, not flap (#343 fix 3). Deliberately a
// SENTINEL DISTINCT from ErrRecentlyDismissed (r12 must-fix 2): "we stopped
// asking" (horizon) and "we could not dispatch" (budget) are different
// failures to an operator — the post-deploy review of #343 must be able to
// tell a converged churn loop from labeled work dropped on broken
// dispatching, and the metric reason follows the sentinel.
var ErrChurnBudgetExhausted = errors.New("dispatch-lost churn budget exhausted")

// ArmClaim arms (or refreshes) this workflow's claim on the PR: resolving
// the deterministic Attempt for (workflow, head SHA) and stamping its review
// state. The returned Attempt is the PRE-PATCH resolve snapshot: its
// Status.Review does NOT reflect the arm just written — callers must use
// .Name (and re-Get if they need post-arm state), never the returned status.
// Any OTHER live claim on the same PR releases as superseded — one
// live claim per PR is the invariant that makes parallel reviews safe.
//
// humanRequest marks an explicit human re-request (label wake) on an
// already-armed head: it is the breaker's override — a human saying "retry
// now" resets the dead-dispatch count and re-arms. Automatic sweeps pass
// false and are refused once the breaker is open.
// DispatchLostWindowOpen reports whether the churn-budget evidence is still
// FRESH: the newest dispatch-lost strike sits inside the horizon window.
// When the strike carries no stamp (claims released by pre-142 workers,
// #391) the claim's own age is the fallback clock — an unstamped budget
// must age out on SOME clock, or the "wait out the horizon" remedy in the
// refusal message is a lie that stands forever. This is THE predicate for
// budget recency: ArmClaim's refusal/reset here and the gate's section-A
// standdown mirror (review_gate.go) both consult it — they used to
// disagree on expiry semantics (r31 finding 4, #390), and a future reader
// "unifying" one to match the other wrongly is the exact failure the
// shared helper prevents.
func DispatchLostWindowOpen(r *v1alpha1.ReviewClaimStatus, claimAge, horizon time.Duration) bool {
	if r == nil {
		return false
	}
	sinceNewest := claimAge // fallback: no stamp → the claim's own age speaks
	if r.LastDispatchLostAt != nil {
		sinceNewest = time.Since(r.LastDispatchLostAt.Time)
	}
	return sinceNewest < horizon
}

func ArmClaim(ctx context.Context, c client.Client, scheme *runtime.Scheme, wf *v1alpha1.Workflow, pr, headSHA, label string, humanRequest bool) (*v1alpha1.Attempt, error) {
	// Pointer-local era read (r7 P1): attempt identity is (source repo,
	// head SHA) — every era of this pointer IS this one object. The r6
	// full-history List read the workflow's ENTIRE retained attempt set
	// (released eras included, full status with run ledgers) once per
	// candidate per sweep on the gate's uncached client, to answer a
	// question about ONE object; released eras are never deleted, so the
	// read grew forever and — with no sweep deadline — degraded under load
	// into exactly the never-dispatched releases the churn guard refuses.
	// Resolve the object by name instead: O(1), no history on the wire.
	obj := DeriveObjective(wf, TriggerContext{Revision: headSHA, Source: "webhook"})
	at, created, err := ResolveOrCreate(ctx, c, obj, ResolveOptions{
		Namespace:   wf.Namespace,
		WorkflowRef: wf.Namespace + "/" + wf.Name,
		Owner:       wf,
		Scheme:      scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve claim attempt: %w", err)
	}

	// Era stickiness + churn guard (#343), read off the SAME object.
	// sameClaim gates every guard: attempt identity carries no PR number,
	// so two pointers (PR re-opened under a new number, head force-pushed
	// back) resolve to one object — a guard may only fire on evidence the
	// ASKING pointer produced (r7 P2).
	r := at.Status.Review
	sameClaim := !created && r != nil && r.PR == pr && r.HeadSHA == headSHA
	// Churn guard (r4: BOTH legs of #343 fix 3's contract): the head's
	// latest era was dismissed by the horizon (born expired — revival would
	// restart the identical ambiguity), OR the head burned through
	// MaxDispatchLostReleases consecutive never-dispatched releases (the
	// release/revive cycle must stop, not flap). Human request overrides
	// (re-apply the label / push + label).
	var budgetExpiry bool
	if !humanRequest && sameClaim {
		// Horizon leg reads the PERSISTED dismissal (r11 must-fix 1), not
		// era state: Released/ReleaseReason are cleared by the revival that
		// answers them, so the old leg fired exactly once and the criterion
		// rode the counter alone. DismissedAt survives revival AND the
		// human override (the override is the human's own arm), expiring
		// with the horizon — the dismissal is only "recent" for the window
		// the horizon names.
		// The horizon window reads the OPERATOR'S number, not the default
		// (r12 P1): a workflow with no reviewReady block must not silently
		// grow a 6h dismissal window — skip the leg instead.
		if wf.Spec.ReviewReady != nil && r.DismissedAt != nil &&
			time.Since(r.DismissedAt.Time) < wf.Spec.ReviewReady.HorizonDuration() {
			return nil, fmt.Errorf("%w: %s — automatic re-arm refused; re-apply the label to request a fresh review",
				ErrRecentlyDismissed, shortSHA(headSHA))
		}
		// The budget is a WINDOW, not a life sentence (#376 defect 2):
		// three strikes within the horizon window refuse; strikes older
		// than the horizon are stale evidence — self-clear below and let
		// the automatic arm proceed. Without this a webhook-less forge
		// (the label event never reaches the gate) had NO operator exit:
		// the prescribed "re-apply the label" never arrived as a
		// humanRequest arm, so the refusal stood forever.
		// Zero CreationTimestamp (never server-stamped — fake clients, or
		// an API-server anomaly) reads as age 0: UNKNOWN age keeps the
		// window OPEN (refuse) — the conservative reading.
		claimAge := time.Since(at.CreationTimestamp.Time)
		if at.CreationTimestamp.IsZero() {
			claimAge = 0
		}
		budgetActive := r.DispatchLostReleases >= v1alpha1.MaxDispatchLostReleases &&
			DispatchLostWindowOpen(r, claimAge, wf.Spec.ReviewReady.HorizonDuration())
		if budgetActive {
			return nil, fmt.Errorf("%w: %s — %d consecutive never-dispatched releases within the horizon window (max %d); re-apply the label for a fresh review now, or wait out the horizon and the budget self-clears",
				ErrChurnBudgetExhausted, shortSHA(headSHA), r.DispatchLostReleases, v1alpha1.MaxDispatchLostReleases)
		}
		budgetExpiry = r.DispatchLostReleases > 0 &&
			!DispatchLostWindowOpen(r, claimAge, wf.Spec.ReviewReady.HorizonDuration())
	}
	// Reusable era: pointer-local now — the era IS this object, so "reuse"
	// just means "the revival rules below decide what an arm of a released
	// era means". The reuse BUDGET lives in the guard above (r4 P1: a
	// dispatch-lost era is only revivable under MaxDispatchLostReleases;
	// the cycle converges into a refusal instead of flapping).

	// Supersede other live claims on this PR (head moved, or an older arm).
	// One full live list per arm is UNAVOIDABLE here: attempt identity is
	// (kind, source repo, head SHA) and carries no PR number, so "is there
	// another live claim on this pointer?" cannot resolve by name. Cost is
	// bounded by live claims per workflow — fine at fleet width ≤ dozens;
	// revisit if that approaches ~100 (NOT the r6 shape: the released
	// history never crosses the wire).
	others, err := LiveReviewClaims(ctx, c, wf)
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Name == at.Name || o.Status.Review.PR != pr {
			continue
		}
		if err := ReleaseClaim(ctx, c, wf.Namespace, o.Name, v1alpha1.ReleaseReasonSuperseded); err != nil {
			return nil, fmt.Errorf("supersede %s: %w", o.Name, err)
		}
	}

	// Breaker check against the CURRENT persisted state, before any write:
	// a refused arm must not clobber the claim (the count is the evidence).
	if cur := at.Status.Review; cur != nil &&
		cur.PR == pr && cur.HeadSHA == headSHA &&
		cur.DeadDispatches >= v1alpha1.MaxDeadDispatchesPerHead && !humanRequest {
		return nil, fmt.Errorf("%w: %d dispatched reviews of %s died without a verdict — automatic re-arm refused; push a new commit or re-apply the label to override",
			ErrDeadDispatchBreaker, cur.DeadDispatches, shortSHA(headSHA))
	}

	// Live-marker removal BEFORE the status patch (r8 P1): absence means
	// live, so the arm's visibility commit is the label removal. If it
	// fails, abort BEFORE the status write — the claim stays released
	// (invisible, harmless) and the next sweep's arm retries the removal;
	// committing Released=false first would strand a live claim outside the
	// gate's list — the committed-but-invisible arm. A refused arm never
	// reaches this: the claim keeps its marker and its evidence.
	if err := markClaimLive(ctx, c, wf.Namespace, at.Name); err != nil {
		return nil, fmt.Errorf("unmark released claim: %w", err)
	}

	now := metav1.NewTime(time.Now())
	err = patchAttemptStatus(ctx, c, wf.Namespace, at.Name, func(s *v1alpha1.AttemptStatus) {
		// Stamp the phase here, not only at create: the real API server's
		// status subresource drops create-time status (the fake kept it —
		// a fake-vs-real divergence that left claims phaseless, #277).
		if s.Phase == "" {
			s.Phase = v1alpha1.AttemptPhaseReconciling
		}
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		r := s.Review
		sameClaim := r.PR == pr && r.HeadSHA == headSHA
		// Reachability note (#352 finding 3): a RELEASED claim never
		// re-enters LiveReviewClaims — the marker is the list bound — so
		// every gate-driven reset below refreshes a LIVE claim. The
		// released-claim reset path is reachable only via ArmClaim itself
		// (a labeled wake or scan arm); the tests exercise it directly for
		// exactly that reason. A stale DispatchedAt can only originate from
		// a failed markClaimLive, which aborts before the status commit.
		//
		// Revival rules SPLIT BY RELEASE REASON (r3 P4 — the r2 blanket reset
		// re-opened the #343 churn): a dispatch-lost release is a
		// never-consummated era — revival KEEPS the era clock, so the verdict
		// window still reaches verdicts posted mid-era and the never-dispatched
		// pass's own horizon check (r4) can expire the era. A horizon release
		// is born expired — revival RESETS the clock (r2 F1: the human
		// override must produce a working era). A live claim refresh always
		// keeps its clock (anchoring, #343).
		wasReleased := r.Released
		wasHorizon := r.ReleaseReason == v1alpha1.ReleaseReasonHorizon
		wasDispatchLost := r.ReleaseReason == v1alpha1.ReleaseReasonDispatchLost
		r.PR, r.HeadSHA, r.Label = pr, headSHA, label
		// r6 P2: a HUMAN re-request on a dispatch-lost era anchors a fresh
		// clock — the old one is already deep in the past (the era only
		// survives because anchoring preserves it), so the next sweep's
		// horizon check would instantly re-release and the guard would
		// refuse again: the human asks, the machine stands down. The
		// AUTOMATIC path keeps its anchoring (#343).
		if !sameClaim || r.ArmedSince == nil || wasHorizon || (humanRequest && wasDispatchLost) {
			t := now
			r.ArmedSince = &t
		}
		if wasReleased {
			// REVIVAL ONLY (not live refresh): the era's dispatch, if any,
			// ended with the release — carrying DispatchedAt across a
			// revival is a phantom dispatch: pass A counts it against
			// capacity with no Job behind it, and the timer pass strikes
			// the breaker for a dispatch never made (#344 F2 — the mirror
			// of #331). A LIVE refresh keeps DispatchedAt: pass A's
			// capacity accounting reads "in flight" from its presence.
			r.DispatchedAt = nil
		}
		// The breaker counts deaths of THIS head's dispatches; a new head
		// or an explicit human re-request starts from zero. The release
		// counter resets under the same predicate (a human wake said
		// "review this" — #343 fix 3; a pointer change must not inherit the
		// first pointer's churn evidence — r7 P2: two pointers sharing one
		// head resolve to this ONE attempt object).
		if !sameClaim || humanRequest || budgetExpiry {
			r.DeadDispatches = 0
			r.DispatchLostReleases = 0
			// DismissedAt is deliberately NOT cleared here — not even by a
			// human request (r11 must-fix 1): the override is the human's
			// OWN arm (the guard reads !humanRequest); the NEXT automatic
			// arm within the horizon window is still refused, and time is
			// the dismissal's only eraser. "Horizon-dismiss → human revival
			// → automatic arm refused" is the named contract.
		}
		r.Released = false
		r.ReleaseReason = ""
		// Re-arm honesty (#328): an attempt being re-armed after a failure
		// is in flight again — reflect it, and drop the stale failure
		// message (the death, if the breaker later opens, is re-recorded
		// by ReleaseClaimDead).
		if s.Phase == v1alpha1.AttemptPhaseFailed {
			s.Phase = v1alpha1.AttemptPhaseReconciling
			s.Message = ""
		}
	})
	if err != nil {
		return nil, err
	}
	return at, nil
}

// MarkClaimDispatched stamps the dispatch liveness marker (#248): the
// DispatchTimeout bound runs from this instant.
func MarkClaimDispatched(ctx context.Context, c client.Client, namespace, attemptName string) error {
	return patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		t := metav1.NewTime(time.Now())
		s.Review.DispatchedAt = &t
		// r6 P1: a successful dispatch breaks the "consecutive
		// never-dispatched releases" chain — without this the counter is
		// monotonic-since-last-human and the field's own contract is false.
		s.Review.DispatchLostReleases = 0
	})
}

// ReleaseClaim frees the claim's capacity slot.
func ReleaseClaim(ctx context.Context, c client.Client, namespace, attemptName, reason string) error {
	if err := patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		s.Review.Released = true
		s.Review.ReleaseReason = reason
		// Consecutive never-dispatched releases for this pointer (#343 fix
		// 3): feeds the reuse bound and the auto re-arm refusal. Same-pointer
		// revivals keep the count — it is the era chain's churn evidence —
		// while a pointer CHANGE resets it (ArmClaim; identity carries no PR
		// number, so two pointers share one attempt object, r7 P2).
		if reason == v1alpha1.ReleaseReasonDispatchLost {
			s.Review.DispatchLostReleases++
			t := metav1.Now()
			s.Review.LastDispatchLostAt = &t
		}
		if reason == v1alpha1.ReleaseReasonHorizon {
			t := metav1.NewTime(time.Now())
			s.Review.DismissedAt = &t
		}
	}); err != nil {
		return err
	}
	// Off the gate's live list with the era (r7 P1, r8 rework): the marker
	// IS the release's visibility commit — additive, RV-preconditioned.
	return markClaimReleased(ctx, c, namespace, attemptName)
}

// ReleaseClaimDead releases a dispatched claim that provably died without a
// verdict (job death or dispatch timeout — the two reasons passed here), and
// records the death in one atomic patch (#328):
//
//   - the breaker counter increments (dispatched deaths only — the guard is
//     load-bearing, callers must not use this for infra releases);
//   - the ledger tells the truth: run records still "running" are failed
//     (the worker is SIGKILLed at DeadlineExceeded and can never write its
//     own outcome — the gate is the death observer), and a phase the worker
//     never finalized becomes failed with an honest message.
//
// A worker-written failure (graceful agent exit, e.g. "agent failed after 4
// attempt(s), …") is preserved verbatim — it carries the specific signal;
// only the stale running records are finalized.
//
// IDEMPOTENT: an already-released claim is never re-counted. The gate's
// sweep runs two release passes (timer-based and job-based) over the same
// claim snapshot; a death discovered by both must count once — the first
// observer records it, the second is a no-op. It returns whether this call
// recorded the death and the post-patch count (the fetch-time snapshot
// callers hold is stale by one death by construction).
func ReleaseClaimDead(ctx context.Context, c client.Client, namespace, attemptName, reason string) (recorded bool, deadDispatches int, err error) {
	err = patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		if s.Review.Released {
			// First observer already recorded this death (or the claim was
			// released for an unrelated reason) — never count twice.
			deadDispatches = s.Review.DeadDispatches
			return
		}
		s.Review.Released = true
		s.Review.ReleaseReason = reason
		if s.Review.DispatchedAt != nil {
			s.Review.DeadDispatches++
			recorded = true
		}
		deadDispatches = s.Review.DeadDispatches
		now := metav1.NewTime(time.Now())
		for i := range s.Runs {
			if s.Runs[i].Phase == "" || s.Runs[i].Phase == "running" {
				s.Runs[i].Phase = "failed"
				s.Runs[i].EndedAt = now
			}
		}
		if s.Phase == "" || s.Phase == v1alpha1.AttemptPhaseReconciling {
			s.Phase = v1alpha1.AttemptPhaseFailed
			s.Message = fmt.Sprintf("run ended without a verdict (%s)", reason)
		}
	})
	if err != nil {
		return recorded, deadDispatches, err
	}
	if recorded {
		// Off the gate's live list with the era — only when this observer
		// recorded the release (an idempotent re-observe must not relabel a
		// claim a revival already re-armed).
		err = markClaimReleased(ctx, c, namespace, attemptName)
	}
	return recorded, deadDispatches, err
}

// FinalizeCancelledClaim finalizes a claim the gate released as superseded or
// closed while its review Job was still running (#402): the cancel-on-supersede
// pass deleted the Job, and this records the cancellation in the ledger —
// run records still "running" are failed (the worker may have been SIGTERMed
// before it could write its own outcome — the gate is the death observer, the
// same convention ReleaseClaimDead established), and the phase reaches a
// terminal state instead of a forever-reconciling husk.
//
// Unlike ReleaseClaimDead, NO breaker counter moves: the dispatch did not die
// unluckily — the gate decided the work is obsolete. Cancelling must never
// spend the churn budget or the dead-dispatch budget (the released claim is
// off the live list either way; this is pure ledger hygiene).
//
// IDEMPOTENT by construction: the patch only touches non-terminal phases and
// run records, and a worker-written terminal outcome is preserved verbatim —
// the cancellation message is stamped ONLY when this call actually finalized
// a running/empty run record (i.e. when the gate really is the death
// observer); a run the worker already recorded honestly is never
// second-guessed with an over-claiming message.
func FinalizeCancelledClaim(ctx context.Context, c client.Client, namespace, attemptName, reason string) error {
	if !v1alpha1.IsCancellationRelease(reason) {
		return nil // not a cancellation — the ledger is not this call's business
	}
	return patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		// The write must re-agree with the state the read saw — the
		// markClaimReleased discipline (r12 must-fix 1). The DeleteJob→
		// finalize gap can see the era REVIVED (label re-applied, or an old
		// head force-pushed back into the same attempt identity): patching
		// then would stamp a terminal ledger over a live in-flight review.
		// A revived claim is not dead; nothing here may tell it otherwise.
		if s.Review == nil || !s.Review.Released || !v1alpha1.IsCancellationRelease(s.Review.ReleaseReason) {
			return
		}
		now := metav1.NewTime(time.Now())
		finalizedRun := false
		for i := range s.Runs {
			if s.Runs[i].Phase == "" || s.Runs[i].Phase == "running" {
				s.Runs[i].Phase = "failed"
				s.Runs[i].EndedAt = now
				finalizedRun = true
			}
		}
		if s.Phase == "" || s.Phase == v1alpha1.AttemptPhaseReconciling {
			if reason == v1alpha1.ReleaseReasonSuperseded {
				s.Phase = v1alpha1.AttemptPhaseSuperseded
			} else {
				s.Phase = v1alpha1.AttemptPhaseFailed
			}
		}
		// The cancellation message is the death observer's statement: stamp it
		// only when this call actually finalized a running/empty run record. A
		// run the worker already recorded honestly (e.g. finished naturally
		// between the job snapshot and this patch) keeps its own story.
		if finalizedRun {
			s.Message = fmt.Sprintf("review cancelled (%s) — Job deleted before the run bound; the gate finalized this ledger as the death observer", reason)
		}
	})
}

// markClaimReleased / markClaimLive maintain the release marker (see
// ReviewClaimLabel) under the SAME write discipline as the status ledger
// (#257): Get → RV-preconditioned MergeFromWithOptimisticLock inside
// RetryOnConflict. Two writers (arm's removal, release's set) race on one
// key; the RV precondition alone does not close the cross-resource race
// (RetryOnConflict re-Gets), so markClaimReleased is additionally
// CONDITIONAL on the status ledger: it stamps only when the freshly-read
// status still says Released (r12 must-fix 1). Both divergence directions
// are pinned by test — see TestMarkClaimReleased_DoesNotStompRevival and
// TestMarkClaimReleased_StatusReleasedMarkerAbsent.
//
// markClaimLive is SUBTRACTIVE (removes the key): absence means live, so
// the pre-upgrade truth is the live truth and a legacy claim holding a
// slot is visible to the first post-deploy sweep with zero backfill.
func markClaimReleased(ctx context.Context, c client.Client, namespace, name string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var at v1alpha1.Attempt
		key := client.ObjectKey{Namespace: namespace, Name: name}
		if err := c.Get(ctx, key, &at); err != nil {
			return err
		}
		if at.Labels[v1alpha1.ReviewClaimLabel] == v1alpha1.ReviewClaimReleased {
			return nil
		}
		// The marker may only commit when the STATUS ledger agrees the
		// claim is released (r12 must-fix 1, probe-verified): a concurrent
		// ArmClaim revival can commit Released=false in the gap between
		// ReleaseClaim's status patch and THIS marker write — stamping the
		// marker then makes the live claim invisible to the gate's list
		// forever (liveDispatched undercounts, the sweep over-dispatches).
		// The revival wins; the next release re-stamps.
		if at.Status.Review == nil || !at.Status.Review.Released {
			return nil
		}
		base := at.DeepCopy()
		if at.Labels == nil {
			at.Labels = map[string]string{}
		}
		at.Labels[v1alpha1.ReviewClaimLabel] = v1alpha1.ReviewClaimReleased
		return c.Patch(ctx, &at, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func markClaimLive(ctx context.Context, c client.Client, namespace, name string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var at v1alpha1.Attempt
		key := client.ObjectKey{Namespace: namespace, Name: name}
		if err := c.Get(ctx, key, &at); err != nil {
			return err
		}
		if _, marked := at.Labels[v1alpha1.ReviewClaimLabel]; !marked {
			return nil
		}
		base := at.DeepCopy()
		delete(at.Labels, v1alpha1.ReviewClaimLabel)
		return c.Patch(ctx, &at, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// LiveReviewClaims returns the workflow's unreleased review claims, oldest
// attempt first (stable arm order for sweeps).
func LiveReviewClaims(ctx context.Context, c client.Client, wf *v1alpha1.Workflow) ([]v1alpha1.Attempt, error) {
	namespace, workflowName := wf.Namespace, wf.Name
	var list v1alpha1.AttemptList
	// Server-side bounded (r7 P1, r8 rework): (workflow=X, review-claim
	// DoesNotExist) — the released era history (one attempt per reviewed
	// head, retained until the #385 GC horizon, full status with run
	// ledgers) never crosses
	// the wire. ABSENCE of the marker means live, so pre-upgrade claims
	// (unlabeled, holding real slots) are visible to the first sweep — the
	// rollout cannot over-dispatch past them. The Released re-check is
	// belt-and-braces for label/status drift, not the bound.
	wfReq, err := labels.NewRequirement(v1alpha1.WorkflowLabel, selection.Equals, []string{workflowName})
	if err != nil {
		return nil, fmt.Errorf("live-claim selector: %w", err)
	}
	unmarked, err := labels.NewRequirement(v1alpha1.ReviewClaimLabel, selection.DoesNotExist, nil)
	if err != nil {
		return nil, fmt.Errorf("live-claim selector: %w", err)
	}
	// The objective-kind leg is deliberately NOT server-side (r11 must-fix
	// 2): the label is written at create by the worker image, a SEPARATELY
	// deployable binary — a gate that rolled first would drop every
	// label-less live claim from this list, undercount capacity, and
	// over-dispatch past maxConcurrent. The marker's ABSENCE is the
	// correctness leg (unlabeled pre-upgrade claims stay visible); the kind
	// filter below is HYGIENE (wire bytes only — correctness leans on the
	// client-side kind + Review checks). Keep the two roles distinct: the
	// next reader will otherwise "restore" the server-side Equals leg and
	// reintroduce the rollout coupling.
	sel := labels.NewSelector().Add(*wfReq).Add(*unmarked)
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	var out []v1alpha1.Attempt
	for _, a := range list.Items {
		if a.Status.Review == nil || a.Status.Review.Released {
			continue
		}
		// Kind check client-side (see selector comment — hygiene, not
		// correctness; but it keeps non-review Attempts out of supersede
		// and capacity arithmetic).
		if a.Labels[v1alpha1.ObjectiveKindLabel] != DeriveKind(wf) {
			continue
		}
		if a.Spec.WorkflowRef != namespace+"/"+workflowName {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// All claim writes go through patchAttemptStatus (lifecycle.go) — the
// ledger's single write primitive, carrying the #257 lost-update discipline
// (optimistic lock + retry) and the #289 structural bounds.

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
