package attempt

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// Review-Ready Gate claims (ADR-0007 phase 4): the Attempt IS the claim.
// Arming creates/refreshes the claim (created at ARM time so waiting state
// persists across sweeps); dispatch marks it; releases free its capacity
// slot. All writes are optimistic-locked status patches — one owner (the
// gate), so the #257 lost-update class cannot recur on claims.

// ArmClaim arms (or refreshes) this workflow's claim on the PR: resolving
// the deterministic Attempt for (workflow, head SHA) and stamping its review
// state. Any OTHER live claim on the same PR releases as superseded — one
// live claim per PR is the invariant that makes parallel reviews safe.
func ArmClaim(ctx context.Context, c client.Client, scheme *runtime.Scheme, wf *v1alpha1.Workflow, pr, headSHA, label string) (*v1alpha1.Attempt, error) {
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
		r.Released = false
		r.ReleaseReason = ""
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

// patchAttemptStatus applies mutate to the attempt's status with lost-update
// discipline (#257): fresh Get → mutate → resourceVersion-preconditioned
// patch, retried on conflict.
func patchAttemptStatus(ctx context.Context, c client.Client, namespace, attemptName string, mutate func(*v1alpha1.AttemptStatus)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var at v1alpha1.Attempt
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: attemptName}, &at); err != nil {
			return err
		}
		base := at.DeepCopy()
		mutate(&at.Status)
		// Structural compaction (#289) — same rationale as mutateStatus:
		// the bound is a property of the write path.
		compactStatus(&at.Status)
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		return c.Status().Patch(ctx, &at, patch)
	})
}
