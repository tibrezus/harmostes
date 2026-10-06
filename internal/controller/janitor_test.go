package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// The janitor (#651): namespace-wide fleet hygiene. The live-fleet defect:
// the janitor used to ride the review-gate sweep, scoped to the swept
// workflow — 27 fork-maintenance claims sat reconciling 13–15 days because
// no sweep ever visited their workflows, and 2 Completed runner pods
// outlived their deleted Job by 4 days because the pod-GC controller
// missed. These tests pin the ACs:
//
//	AC 1/2 — fork-maintenance-shaped wedges reap with NO gate sweep present
//	AC 3   — Completed runner pods whose Job is gone are deleted;
//	         live pods and live Jobs are never touched
func TestJanitorPass(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Attempt{}).Build()
	old := metav1.NewTime(time.Now().Add(-15 * 24 * time.Hour))
	ancient := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour)) // past the 720h retention horizon
	young := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	const ns = "harmostes"

	mkAttempt := func(name, wf string, ts metav1.Time, phase string, claim *v1alpha1.ReviewClaimStatus) *v1alpha1.Attempt {
		at := &v1alpha1.Attempt{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: ts,
			Labels: map[string]string{v1alpha1.WorkflowLabel: wf},
		}}
		if phase != "" {
			at.Status.Phase = phase
		}
		if claim != nil {
			at.Status.Review = claim
		}
		return at
	}
	mkPod := func(name, jobName string, phase corev1.PodPhase, ts metav1.Time) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: ts,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "attempt-runner",
				"batch.kubernetes.io/job-name": jobName,
			},
		}, Status: corev1.PodStatus{Phase: phase}}
	}

	fixtures := []client.Object{
		// ── the #651 fleet shape: fork-maintenance wedge, unreleased claim ──
		mkAttempt("fork-wedge", "fork-maintenance-signoz", old,
			v1alpha1.AttemptPhaseReconciling, &v1alpha1.ReviewClaimStatus{Released: false}),
		// review-gate wedge: same bound, different template
		mkAttempt("review-wedge", "pr-review-rhesadox", old,
			v1alpha1.AttemptPhaseReconciling, &v1alpha1.ReviewClaimStatus{Released: false}),
		// released era past retention — GC'd
		mkAttempt("old-released", "pr-review-rhesadox", ancient,
			v1alpha1.AttemptPhaseValidated, &v1alpha1.ReviewClaimStatus{Released: true}),
		// live claim young — untouchable
		mkAttempt("young-claim", "pr-review-rhesadox", young,
			v1alpha1.AttemptPhaseReconciling, &v1alpha1.ReviewClaimStatus{Released: false}),
		// terminal young — inside retention, kept
		mkAttempt("young-terminal", "fork-maintenance-signoz", young, v1alpha1.AttemptPhaseFailed, nil),
		// ── pods: orphaned Completed (job gone) → deleted; live job's pod → kept ──
		mkPod("pod-orphan", "attempt-fork-wedge-job", corev1.PodSucceeded, old),
		mkPod("pod-live", "attempt-young-job", corev1.PodRunning, young),
		mkPod("pod-terminal-live-job", "attempt-alive-job", corev1.PodSucceeded, young),
		// no job-name label — unresolvable, skipped, NOT deleted
		func() *corev1.Pod {
			p := mkPod("pod-no-label", "", corev1.PodFailed, old)
			p.Labels = map[string]string{"app.kubernetes.io/component": "attempt-runner"}
			return p
		}(),
	}
	for _, o := range fixtures {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
	// the live Job that pod-terminal-live-job points at
	if err := c.Create(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "attempt-alive-job", Namespace: ns,
	}}); err != nil {
		t.Fatal(err)
	}

	j := &Janitor{Client: c, Namespace: ns}
	j.pass(ctx, logr.Discard())

	// ── wedges reaped (both templates) ──
	for _, name := range []string{"fork-wedge", "review-wedge"} {
		var at v1alpha1.Attempt
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &at); err != nil {
			t.Fatalf("wedge %s must survive the reap (released, failed — not deleted): %v", name, err)
		}
		if at.Status.Phase != v1alpha1.AttemptPhaseFailed || !strings.Contains(at.Status.Message, "reaped") {
			t.Fatalf("wedge %s must be reaped to failed, got phase=%s msg=%q", name, at.Status.Phase, at.Status.Message)
		}
		if at.Status.Review == nil || !at.Status.Review.Released {
			t.Fatalf("wedge %s claim must be released, got %+v", name, at.Status.Review)
		}
	}
	// ── retention GC ──
	var gone v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "old-released"}, &gone); err == nil {
		t.Fatalf("old released era must be GC'd, it survived")
	}
	// ── live work untouched ──
	for _, name := range []string{"young-claim", "young-terminal"} {
		var at v1alpha1.Attempt
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &at); err != nil {
			t.Fatalf("live attempt %s must survive: %v", name, err)
		}
	}
	// ── orphan pod deleted; live pods untouched; unresolvable pod kept ──
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pod-orphan"}, &pod); err == nil {
		t.Fatalf("orphaned Completed pod must be deleted, it survived")
	}
	for _, name := range []string{"pod-live", "pod-terminal-live-job", "pod-no-label"} {
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
			t.Fatalf("pod %s must survive the janitor: %v", name, err)
		}
	}
}
