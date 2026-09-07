package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
)

func TestReconcilePublishesTrigger(t *testing.T) {
	// The controller publishes a trigger event to the Dapr pub/sub topic.
	// No batchv1.Job is created — the worker pool consumes the event.
	var publishedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		publishedBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wf := &v1alpha1.Workflow{}
	wf.Name = "wiki-lint-test"
	wf.Namespace = "harmostes"
	wf.Generation = 2
	wf.Status.ObservedGeneration = 1

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()

	r := &WorkflowReconciler{
		Client:       cl,
		Scheme:       k8s.Scheme(),
		PollInterval: 5 * time.Minute,
		DaprClient:   dapr.New(srv.URL),
		TriggerTopic: "harmostes-triggers",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "wiki-lint-test", Namespace: "harmostes"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A trigger event should have been published.
	if publishedBody == "" {
		t.Fatal("expected trigger event to be published, got empty body")
	}
	var trigger TriggerEvent
	if err := json.Unmarshal([]byte(publishedBody), &trigger); err != nil {
		t.Fatalf("unmarshal trigger event: %v", err)
	}
	if trigger.Workflow != "wiki-lint-test" {
		t.Errorf("trigger workflow = %q, want wiki-lint-test", trigger.Workflow)
	}
}

func TestReconcileSkipsDisabledWorkflow(t *testing.T) {
	published := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wf := &v1alpha1.Workflow{}
	wf.Name = "disabled-wf"
	wf.Namespace = "harmostes"
	wf.Spec.Disabled = true

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithObjects(wf).
		Build()

	r := &WorkflowReconciler{
		Client:       cl,
		Scheme:       k8s.Scheme(),
		PollInterval: 5 * time.Minute,
		DaprClient:   dapr.New(srv.URL),
		TriggerTopic: "harmostes-triggers",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "disabled-wf", Namespace: "harmostes"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("requeue = %v, want 5m", result.RequeueAfter)
	}
	if published {
		t.Error("should not publish trigger for disabled workflow")
	}
}

func TestIsDue_WebhookTrigger(t *testing.T) {
	r := &WorkflowReconciler{PollInterval: 5 * time.Minute}

	// Webhook trigger with changed revision → due
	wf := &v1alpha1.Workflow{}
	wf.Annotations = map[string]string{"harmostes.dev/trigger-revision": "abc123"}
	wf.Status.LastProcessedRevision = "old456"
	due, _ := r.isDue(wf)
	if !due {
		t.Error("expected due=true for webhook with changed revision")
	}

	// Webhook trigger with same revision → not due
	wf2 := &v1alpha1.Workflow{}
	wf2.Annotations = map[string]string{"harmostes.dev/trigger-revision": "abc123"}
	wf2.Status.LastProcessedRevision = "abc123"
	wf2.Spec.Source.Kind = "webhook"
	due, _ = r.isDue(wf2)
	if due {
		t.Error("expected due=false for webhook with same revision")
	}

	// Webhook-only workflow with no trigger-revision → not due (waits for webhook)
	wf4 := &v1alpha1.Workflow{}
	wf4.Spec.Source.Kind = "webhook"
	wf4.Status.ObservedGeneration = 1
	wf4.Generation = 1
	due, _ = r.isDue(wf4)
	if due {
		t.Error("expected due=false for webhook-only workflow without trigger-revision")
	}

	// No webhook, spec changed → due
	wf3 := &v1alpha1.Workflow{}
	wf3.Generation = 2
	wf3.Status.ObservedGeneration = 1
	due, _ = r.isDue(wf3)
	if !due {
		t.Error("expected due=true for spec change")
	}
}

func TestObserveGenerationSetsLastRunAt(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "test-wf"
	wf.Namespace = "harmostes"
	wf.Generation = 3
	wf.Status.ObservedGeneration = 2

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()

	r := &WorkflowReconciler{
		Client: cl,
		Scheme: k8s.Scheme(),
	}

	if err := r.observeGeneration(context.Background(), wf); err != nil {
		t.Fatalf("observeGeneration: %v", err)
	}

	var got v1alpha1.Workflow
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-wf", Namespace: "harmostes"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.ObservedGeneration != 3 {
		t.Errorf("observedGeneration = %d, want 3", got.Status.ObservedGeneration)
	}
	if got.Status.LastRunAt.IsZero() {
		t.Error("LastRunAt should be set at schedule time")
	}
	// Verify the Scheduled condition
	var hasScheduled bool
	for _, c := range got.Status.Conditions {
		if c.Type == "Scheduled" && c.Status == metav1.ConditionTrue {
			hasScheduled = true
		}
	}
	if !hasScheduled {
		t.Error("missing Scheduled=True condition")
	}
}

// TestObserveGenerationRetriesOnConflict (#257): a concurrent writer (the
// one-shot gate patching reviewReady) bumps the resourceVersion between the
// controller's read and write; the optimistic-lock patch must conflict and
// the retry must land ALL of observeGeneration's fields on the fresh state
// — while the concurrent reviewReady write survives untouched.
func TestObserveGenerationRetriesOnConflict(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "test-wf"
	wf.Namespace = "harmostes"
	wf.Generation = 3
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{LiveClaims: 1, LastDecision: "waiting"}

	attempts := 0
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				attempts++
				if attempts == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "harmostes.dev", Resource: "workflows"},
						"test-wf", errors.New("modified"))
				}
				return c.Status().Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &WorkflowReconciler{Client: cl, Scheme: k8s.Scheme()}
	if err := r.observeGeneration(context.Background(), wf); err != nil {
		t.Fatalf("observeGeneration: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected conflict then success, got %d attempts", attempts)
	}

	var got v1alpha1.Workflow
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "test-wf", Namespace: "harmostes"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 3 || got.Status.LastRunAt.IsZero() {
		t.Fatalf("observeGeneration fields not landed after retry: %+v", got.Status)
	}
	if got.Status.ReviewReady == nil || got.Status.ReviewReady.LiveClaims != 1 {
		t.Fatalf("concurrent reviewReady write must survive the retry: %+v", got.Status.ReviewReady)
	}
}

// TestClaimTriggerSlot_DedupsRacingReconciles (#343 fix 2): the FIRST caller
// wins the slot and publishes; an immediate second caller (a racing
// reconcile — the armed-gate carve-out makes every reconcile of an armed
// workflow "due") must LOSE until the cooldown elapses. This is the engine
// of the claim-churn loop: without the slot, every reconcile scheduled
// another sweep.
// ── r7 P2 / r8 P8: the slot's regression surface is INSIDE Reconcile —
// ordering vs the early returns, a new error path returning nil after
// won==true, a wake swallowed by the carve-out. These cases pin publish
// counts at the Reconcile boundary (the TestReconcileSkipsDisabledWorkflow
// httptest-sink pattern). Due-ness comes from the spec-change leg
// (Generation bumped, ObservedGeneration not) so Reconcile reaches the
// slot in every case; the claimTriggerSlot helper tests live BELOW this
// block (they test the primitive, not Reconcile's ordering). ──
func reconcileSlotWorkflow(name string, lastRunAt time.Time) *v1alpha1.Workflow {
	wf := &v1alpha1.Workflow{}
	wf.Name = name
	wf.Namespace = "harmostes"
	wf.Generation = 2
	wf.Status.ObservedGeneration = 1 // spec-change → due
	wf.Status.LastRunAt = metav1.NewTime(lastRunAt)
	return wf
}

func TestReconcile_WinningReconcilePublishesExactlyOne(t *testing.T) {
	published := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wf := reconcileSlotWorkflow("slot-win", time.Time{}) // LastRunAt zero: fresh win
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()
	r := &WorkflowReconciler{
		Client: cl, Scheme: k8s.Scheme(), PollInterval: 5 * time.Minute,
		DaprClient: dapr.New(srv.URL), TriggerTopic: "harmostes-triggers",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace}}

	for i := 1; i <= 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	// The first reconcile wins the slot and publishes; the second must
	// observe the first's LastRunAt stamp and publish NOTHING.
	if published != 1 {
		t.Fatalf("two reconciles must publish exactly one trigger, got %d", published)
	}
}

func TestReconcile_CooldownLoserPublishesNothing(t *testing.T) {
	published := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// LastRunAt 1s ago: inside the 5m schedule cooldown, but the workflow
	// is DUE (spec change). The slot must suppress the publish.
	wf := reconcileSlotWorkflow("slot-cooldown", time.Now().Add(-time.Second))
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()
	r := &WorkflowReconciler{
		Client: cl, Scheme: k8s.Scheme(), PollInterval: 5 * time.Minute,
		DaprClient: dapr.New(srv.URL), TriggerTopic: "harmostes-triggers",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace},
	})
	if err != nil {
		t.Fatalf("cooldown reconcile must not error, got %v", err)
	}
	if published != 0 {
		t.Fatalf("cooldown-hit reconcile must publish zero triggers, got %d", published)
	}
	// The loser requeues at the cooldown floor — not immediately (that is
	// the busy-loop shape the slot exists to remove).
	if result.RequeueAfter != 5*time.Minute {
		t.Fatalf("cooldown loser must requeue at PollInterval, got %v", result.RequeueAfter)
	}
}

func TestReconcile_SlotPatchFailureRequeuesWithoutPublish(t *testing.T) {
	published := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wf := reconcileSlotWorkflow("slot-conflict", time.Time{})
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		WithInterceptorFuncs(interceptor.Funcs{
			// The CAS can never WIN here: every status patch conflicts, so
			// RetryOnConflict exhausts and claimTriggerSlot returns the error —
			// this pins the ERROR branch (requeue, no publish, no crash). The
			// COOLDOWN branch (errTriggerCooldown → lose WITHOUT error) is pinned
			// by TestReconcile_CooldownLoserPublishesNothing above.
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "harmostes.dev", Resource: "workflows"},
					wf.Name, errors.New("modified"))
			},
		}).
		Build()
	r := &WorkflowReconciler{
		Client: cl, Scheme: k8s.Scheme(), PollInterval: 5 * time.Minute,
		DaprClient: dapr.New(srv.URL), TriggerTopic: "harmostes-triggers",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace},
	})
	// A persistently failing CAS suppresses EVERY trigger (#118 discipline):
	// it must requeue (visible, retryable), never error-crash the worker, and
	// never publish on a slot it does not own.
	if err != nil {
		t.Fatalf("slot failure must requeue, not error, got %v", err)
	}
	if published != 0 {
		t.Fatalf("unowned slot must publish zero triggers, got %d", published)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Fatalf("slot failure must requeue at PollInterval, got %v", result.RequeueAfter)
	}
}

func TestClaimTriggerSlot_DedupsRacingReconciles(t *testing.T) {
	wf := &v1alpha1.Workflow{}
	wf.Name = "pr-review-churn"
	wf.Namespace = "harmostes"

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()
	r := &WorkflowReconciler{Client: cl, Scheme: k8s.Scheme(), PollInterval: 5 * time.Minute}
	ctx := context.Background()

	won, err := r.claimTriggerSlot(ctx, wf, r.PollInterval, webhookMinTriggerInterval)
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v — a fresh workflow must win", won, err)
	}
	won, err = r.claimTriggerSlot(ctx, wf, r.PollInterval, webhookMinTriggerInterval)
	if err != nil || won {
		t.Fatalf("second claim within cooldown: won=%v err=%v — must lose", won, err)
	}
	// The webhook window is shorter, but not zero: two racing reconciles
	// must not BOTH publish (the second observe the first's stamp).
	won, err = r.claimTriggerSlot(ctx, wf, r.PollInterval, webhookMinTriggerInterval)
	if err != nil || won {
		t.Fatalf("webhook-window claim inside the race window: won=%v err=%v — the CAS must dedup", won, err)
	}

	// r6 P1: the cooldown is keyed on the AUTHORITATIVE source. A wake
	// whose reconcile copy predates the annotation (informer lag) used to
	// be held to the full PollInterval anchor; now the fresh copy inside
	// the CAS decides. LastRunAt 30s old + annotation present → the wake
	// wins on the 10s window although the 5m schedule cooldown has not
	// elapsed. Without the annotation it must still lose (schedule key).
	var fresh2 v1alpha1.Workflow
	if err := cl.Get(ctx, client.ObjectKeyFromObject(wf), &fresh2); err != nil {
		t.Fatal(err)
	}
	stale := fresh2.DeepCopy()
	stale.Annotations = nil // simulate the pre-annotation cache copy
	fresh2.Annotations = map[string]string{"harmostes.dev/trigger-revision": "rev-next"}
	if err := cl.Update(ctx, &fresh2); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(wf), &fresh2); err != nil {
		t.Fatal(err)
	}
	fresh2.Status.LastRunAt = metav1.NewTime(metav1.Now().Add(-30 * time.Second))
	if err := cl.Status().Update(ctx, &fresh2); err != nil {
		t.Fatal(err)
	}
	won, err = r.claimTriggerSlot(ctx, stale, r.PollInterval, webhookMinTriggerInterval)
	if err != nil || !won {
		t.Fatalf("webhook wake behind a stale cache copy: won=%v err=%v — the fresh annotation must key the 10s window", won, err)
	}

	// After the cooldown the slot frees.
	wf2 := &v1alpha1.Workflow{}
	wf2.Name = "pr-review-churn-2"
	wf2.Namespace = "harmostes"
	_ = cl.Create(ctx, wf2)
	if _, err := r.claimTriggerSlot(ctx, wf2, 0, 0); err != nil {
		t.Fatalf("zero-cooldown claim: %v", err)
	}
	var fresh v1alpha1.Workflow
	if err := cl.Get(ctx, client.ObjectKeyFromObject(wf2), &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.LastRunAt = metav1.NewTime(metav1.Now().Add(-2 * r.PollInterval))
	if err := cl.Status().Update(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	won, err = r.claimTriggerSlot(ctx, wf2, r.PollInterval, webhookMinTriggerInterval)
	if err != nil || !won {
		t.Fatalf("claim after cooldown: won=%v err=%v — must win", won, err)
	}
}

// TestReconcile_WebhookLoserRequeuesAtFloor (r8 F3): the busy-loop guard —
// a webhook-due LOSER (cooldown hit) must not requeue at 0; the
// `requeueAfter == 0 → webhookMinTriggerInterval` leg is the subtlest line
// in the slot logic and had no test. The sibling test
// TestReconcile_CooldownLoserPublishesNothing pins the PollInterval leg.
func TestReconcile_WebhookLoserRequeuesAtFloor(t *testing.T) {
	published := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		published++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wf := reconcileSlotWorkflow("webhook-loser", time.Now().Add(-time.Second)) // inside the 10s webhook window
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-revision": "abc123", // != LastProcessedRevision (empty) → webhook-due
	}
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf).
		Build()
	r := &WorkflowReconciler{
		Client: cl, Scheme: k8s.Scheme(), PollInterval: 5 * time.Minute,
		DaprClient: dapr.New(srv.URL), TriggerTopic: "harmostes-triggers",
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wf.Name, Namespace: wf.Namespace},
	})
	if err != nil {
		t.Fatalf("webhook loser must not error, got %v", err)
	}
	if published != 0 {
		t.Fatalf("cooldown-hit webhook loser must publish zero triggers, got %d", published)
	}
	if result.RequeueAfter != webhookMinTriggerInterval {
		t.Fatalf("webhook-due loser must requeue at the 10s floor (never 0 — hot-loop guard), got %v", result.RequeueAfter)
	}
}
