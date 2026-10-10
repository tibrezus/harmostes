// Package k8sconform is the fake-vs-real conformance suite (#669,
// pi-durable lesson 4): every storage-backed wrapper behavior is written
// ONCE here and runs against BOTH the fake client (unit tier, every
// `go test`) and a real API server (envtest, `make test-integration`
// and CI's integration tier). #277 was a fake-vs-real divergence found
// by luck; this table makes the class of bug systematic — a wrapper
// that works only on the fake client fails the envtest leg by
// construction.
//
// Known divergences (fake ≠ real) are not hidden: each case asserts the
// REAL-server semantic on the Envtest leg and the documented fake
// semantic on the Fake leg. A wrapper that depends on a fake-only
// behavior therefore fails on Envtest, never silently on Fake.
package k8sconform

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// Backend names the client under test.
type Backend string

const (
	Fake    Backend = "fake"    // controller-runtime fake client (unit tier)
	Envtest Backend = "envtest" // real API server (integration tier)
)

// Run executes the full wrapper conformance table. Both tiers call this
// with their client; failures name the backend so a red envtest leg
// reads as "wrapper relies on fake-only behavior".
func Run(t *testing.T, cl client.Client, backend Backend) {
	t.Helper()
	ctx := context.Background()
	ns := "conform-" + string(backend)
	// A real server requires the namespace to exist; the fake client
	// ignores it — create unconditionally (part of the contract table).
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("[%s] create namespace: %v", backend, err)
	}

	// Fixture: a Workflow (StatusPatcher target) and an Attempt (the
	// claim-status wrappers' target).
	wf := &v1alpha1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: ns},
		Spec: v1alpha1.WorkflowSpec{Source: v1alpha1.SourceSpec{Kind: "schedule"}}}
	mustCreate(t, cl, wf)

	at := &v1alpha1.Attempt{ObjectMeta: metav1.ObjectMeta{Name: "at", Namespace: ns},
		Spec: v1alpha1.AttemptSpec{Objective: v1alpha1.ObjectiveSpec{Kind: "pr-review"}}}
	mustCreate(t, cl, at)

	t.Run("StatusPatcherPatchAppliesAndRoundtrips", func(t *testing.T) {
		p := k8s.StatusPatcher{Client: cl, Namespace: ns}
		if err := p.PatchStatus(ctx, "wf", func(s *v1alpha1.WorkflowStatus) {
			s.GateStatus = "green"
		}); err != nil {
			t.Fatalf("[%s] PatchStatus: %v", backend, err)
		}
		got, err := p.GetStatus(ctx, "wf")
		if err != nil {
			t.Fatalf("[%s] GetStatus: %v", backend, err)
		}
		if got.GateStatus != "green" {
			t.Fatalf("[%s] gateStatus must roundtrip, got %q", backend, got.GateStatus)
		}
	})

	t.Run("AttemptStatusSubresourcePatch", func(t *testing.T) {
		// The claim wrappers' substrate (#666/#667): intent + edge land
		// through status subresource patches.
		if err := attempt.MarkDispatchIntent(ctx, cl, ns, "at"); err != nil {
			t.Fatalf("[%s] MarkDispatchIntent: %v", backend, err)
		}
		if err := attempt.MarkClaimDispatched(ctx, cl, ns, "at", "job-conform"); err != nil {
			t.Fatalf("[%s] MarkClaimDispatched: %v", backend, err)
		}
		var after v1alpha1.Attempt
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "at"}, &after); err != nil {
			t.Fatalf("[%s] get: %v", backend, err)
		}
		if after.Status.Review == nil || after.Status.Review.DispatchedJob != "job-conform" ||
			after.Status.Review.DispatchedAt == nil {
			t.Fatalf("[%s] intent+edge must persist: %+v", backend, after.Status.Review)
		}
	})

	t.Run("StatusPatchMustNotTouchSpec", func(t *testing.T) {
		// The subresource contract: status writes may never leak into spec.
		var before v1alpha1.Workflow
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wf"}, &before); err != nil {
			t.Fatal(err)
		}
		p := k8s.StatusPatcher{Client: cl, Namespace: ns}
		if err := p.PatchStatus(ctx, "wf", func(s *v1alpha1.WorkflowStatus) {
			s.ObservedGeneration = 42
		}); err != nil {
			t.Fatalf("[%s] PatchStatus: %v", backend, err)
		}
		var after v1alpha1.Workflow
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wf"}, &after); err != nil {
			t.Fatal(err)
		}
		if string(after.Spec.Config) != string(before.Spec.Config) {
			t.Fatalf("[%s] spec drifted under a status patch: %q", backend, after.Spec.Config)
		}
	})

	t.Run("MatchingLabelsSelectsOnlyMatching", func(t *testing.T) {
		mk := func(name, label string) *v1alpha1.Attempt {
			return &v1alpha1.Attempt{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: ns, Labels: map[string]string{"tier": label}},
				Spec: v1alpha1.AttemptSpec{Objective: v1alpha1.ObjectiveSpec{Kind: "pr-review"}}}
		}
		mustCreate(t, cl, mk("at-a", "gold"))
		mustCreate(t, cl, mk("at-b", "dust"))
		var list v1alpha1.AttemptList
		if err := cl.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{"tier": "gold"}); err != nil {
			t.Fatalf("[%s] list: %v", backend, err)
		}
		if len(list.Items) != 1 || list.Items[0].Name != "at-a" {
			t.Fatalf("[%s] label selector must match exactly at-a, got %d", backend, len(list.Items))
		}
	})

	t.Run("CreateTimeStatusIsDropped", func(t *testing.T) {
		// #277, the founding divergence: the real API server IGNORES status
		// set at Create time (status is a subresource); the fake client
		// keeps it. Wrappers must never rely on create-time status.
		obj := &v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{Name: "at-ct", Namespace: ns},
			Spec:       v1alpha1.AttemptSpec{Objective: v1alpha1.ObjectiveSpec{Kind: "pr-review"}},
			Status:     v1alpha1.AttemptStatus{Phase: "validated"}}
		if err := cl.Create(ctx, obj); err != nil {
			t.Fatalf("[%s] create: %v", backend, err)
		}
		var got v1alpha1.Attempt
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "at-ct"}, &got); err != nil {
			t.Fatal(err)
		}
		if backend == Envtest {
			if got.Status.Phase != "" {
				t.Fatalf("[%s] real server must drop create-time status, got %q", backend, got.Status.Phase)
			}
		} else if got.Status.Phase != "validated" {
			t.Fatalf("[%s] documented fake divergence: fake KEEPS create-time status, got %q", backend, got.Status.Phase)
		}
	})

	t.Run("ForegroundJobDelete", func(t *testing.T) {
		// The janitor's stranded-job leg (#667) deletes with foreground
		// propagation. Fake deletes immediately; a real server marks the
		// deletion timestamp (foreground finalizer until dependents die)
		// — wrappers must treat NotFound AND still-terminating as deleted.
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job-fg", Namespace: ns},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:    []corev1.Container{{Name: "c", Image: "busybox"}},
				RestartPolicy: corev1.RestartPolicyNever,
			}}}}
		if err := cl.Create(ctx, job); err != nil {
			t.Fatalf("[%s] create job: %v", backend, err)
		}
		err := cl.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground))
		if err != nil {
			t.Fatalf("[%s] delete: %v", backend, err)
		}
		var after batchv1.Job
		getErr := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "job-fg"}, &after)
		switch backend {
		case Envtest:
			// Real server: either gone already or terminating — both are
			// "deleted" for wrapper purposes. Anything else is a bug.
			if getErr == nil && after.DeletionTimestamp.IsZero() {
				t.Fatalf("[%s] foreground delete must at least mark deletion", backend)
			}
		case Fake:
			if getErr == nil {
				t.Fatalf("[%s] documented fake divergence: fake deletes immediately, job still readable", backend)
			}
		}
	})
}

func mustCreate(t *testing.T, cl client.Client, obj client.Object) {
	t.Helper()
	if err := cl.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %s: %v", obj.GetName(), err)
	}
}
