package attempt

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// ArmClaim arms (or refreshes) this workflow's claim on the PR: resolving
// the deterministic Attempt for (workflow, head SHA) and stamping its review
// state. Any OTHER live claim on the same PR releases as superseded — one
// live claim per PR is the invariant that makes parallel reviews safe.
//
// humanRequest marks an explicit human re-request (label wake) on an
// already-armed head: it is the breaker's override — a human saying "retry
// now" resets the dead-dispatch count and re-arms. Automatic sweeps pass
// false and are refused once the breaker is open.
func ArmClaim(ctx context.Context, c client.Client, scheme *runtime.Scheme, wf *v1alpha1.Workflow, pr, headSHA, label string, humanRequest bool) (*v1alpha1.Attempt, error) {
	obj := DeriveObjective(wf, TriggerContext{Revision: headSHA, Source: "webhook"})
	at, _, err := ResolveOrCreate(ctx, c, obj, ResolveOptions{
		Namespace:   wf.Namespace,
		WorkflowRef: wf.Namespace + "/" + wf.Name,
		Owner:       wf,
		Scheme:      scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve claim attempt: %w", err)
	}

	// Supersede other live claims on this PR (head moved, or an older arm).
	others, err := LiveReviewClaims(ctx, c, wf.Namespace, wf.Name)
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Name == at.Name || o.Status.Review.PR != pr {
			continue
		}
		if err := ReleaseClaim(ctx, c, wf.Namespace, o.Name, "superseded"); err != nil {
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
		r.PR, r.HeadSHA, r.Label = pr, headSHA, label
		// The horizon clock persists across refreshes of the SAME claim;
		// a new claim (new SHA/PR) arms fresh.
		if !sameClaim || r.ArmedSince == nil {
			t := now
			r.ArmedSince = &t
		}
		// The breaker counts deaths of THIS head's dispatches; a new head
		// or an explicit human re-request starts from zero.
		if !sameClaim || humanRequest {
			r.DeadDispatches = 0
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
	})
}

// ReleaseClaim frees the claim's capacity slot.
func ReleaseClaim(ctx context.Context, c client.Client, namespace, attemptName, reason string) error {
	return patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		s.Review.Released = true
		s.Review.ReleaseReason = reason
	})
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
func ReleaseClaimDead(ctx context.Context, c client.Client, namespace, attemptName, reason string) error {
	return patchAttemptStatus(ctx, c, namespace, attemptName, func(s *v1alpha1.AttemptStatus) {
		if s.Review == nil {
			s.Review = &v1alpha1.ReviewClaimStatus{}
		}
		s.Review.Released = true
		s.Review.ReleaseReason = reason
		if s.Review.DispatchedAt != nil {
			s.Review.DeadDispatches++
		}
		now := metav1.NewTime(time.Now())
		for i := range s.Runs {
			if s.Runs[i].Phase == "" || s.Runs[i].Phase == "running" {
				s.Runs[i].Phase = "failed"
				s.Runs[i].EndedAt = now
			}
		}
		if s.Phase == "" || s.Phase == v1alpha1.AttemptPhaseReconciling {
			s.Phase = v1alpha1.AttemptPhaseFailed
			s.Message = fmt.Sprintf("dispatch lost: run ended without a verdict (%s)", reason)
		}
	})
}

// LiveReviewClaims returns the workflow's unreleased review claims, oldest
// attempt first (stable arm order for sweeps).
func LiveReviewClaims(ctx context.Context, c client.Client, namespace, workflowName string) ([]v1alpha1.Attempt, error) {
	var list v1alpha1.AttemptList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	var out []v1alpha1.Attempt
	for _, a := range list.Items {
		if a.Status.Review == nil || a.Status.Review.Released {
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
