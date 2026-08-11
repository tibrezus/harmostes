package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
