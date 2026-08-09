package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
)

func TestPublishTrigger_BuildsCloudEvent(t *testing.T) {
	// Capture the published event via a mock Dapr HTTP server.
	var publishedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		publishedBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := &WorkflowReconciler{
		DaprClient:     dapr.New(srv.URL),
		PubSubTriggers: true,
		TriggerTopic:   "harmostes-triggers",
	}

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-lint-harmostes", Namespace: "harmostes"},
	}

	err := r.publishTrigger(context.Background(), wf, "webhook", "abc123", "00-trace", "attempt-1")
	if err != nil {
		t.Fatalf("publishTrigger: %v", err)
	}

	// Verify the published CloudEvent
	var event cloudevents.Event
	if err := json.Unmarshal([]byte(publishedBody), &event); err != nil {
		t.Fatalf("unmarshal cloud event: %v", err)
	}

	if event.Type() != "harmostes.trigger" {
		t.Errorf("event type = %q, want harmostes.trigger", event.Type())
	}
	if event.Source() != "harmostes-controller" {
		t.Errorf("event source = %q", event.Source())
	}
	if event.Subject() != "wiki-lint-harmostes" {
		t.Errorf("event subject = %q", event.Subject())
	}

	var trigger TriggerEvent
	if err := event.DataAs(&trigger); err != nil {
		t.Fatalf("unmarshal trigger data: %v", err)
	}
	if trigger.Workflow != "wiki-lint-harmostes" {
		t.Errorf("workflow = %q", trigger.Workflow)
	}
	if trigger.Namespace != "harmostes" {
		t.Errorf("namespace = %q", trigger.Namespace)
	}
	if trigger.TriggerType != "webhook" {
		t.Errorf("trigger type = %q", trigger.TriggerType)
	}
	if trigger.Revision != "abc123" {
		t.Errorf("revision = %q", trigger.Revision)
	}
	if trigger.Traceparent != "00-trace" {
		t.Errorf("traceparent = %q", trigger.Traceparent)
	}
	if trigger.AttemptName != "attempt-1" {
		t.Errorf("attempt = %q", trigger.AttemptName)
	}
}

func TestPublishTrigger_NilDaprClient(t *testing.T) {
	// When DaprClient is nil, publishTrigger should be a no-op (not an error).
	r := &WorkflowReconciler{
		DaprClient: nil,
	}
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "harmostes"},
	}
	err := r.publishTrigger(context.Background(), wf, "schedule", "", "", "")
	if err != nil {
		t.Errorf("expected nil error with nil DaprClient, got %v", err)
	}
}

func TestTriggerReason(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		genObs      int64
		genCur      int64
		lastProc    string
		want        string
	}{
		{
			name:        "webhook trigger",
			annotations: map[string]string{"harmostes.dev/trigger-revision": "new-rev"},
			lastProc:    "old-rev",
			want:        "webhook",
		},
		{
			name:   "spec change",
			genObs: 1,
			genCur: 2,
			want:   "spec-change",
		},
		{
			name: "schedule",
			want: "schedule",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			wf.Generation = tt.genCur
			wf.Status.ObservedGeneration = tt.genObs
			wf.Status.LastProcessedRevision = tt.lastProc

			got := triggerReason(wf)
			if got != tt.want {
				t.Errorf("triggerReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Ensure the fake client compiles (verifies the reconciler struct is usable).
func TestReconcilerStructCompiles(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &WorkflowReconciler{
		Client: cl,
		Scheme: scheme,
	}
	_ = r // just verify it compiles + links
}
