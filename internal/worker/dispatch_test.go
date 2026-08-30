package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func dispatchScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("batchv1 scheme: %v", err)
	}
	return s
}

func newTestDispatcher(t *testing.T, objects ...runtime.Object) (*Dispatcher, context.Context) {
	t.Helper()
	scheme := dispatchScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}, &v1alpha1.Workflow{}).
		WithRuntimeObjects(objects...).Build()
	d := &Dispatcher{
		cl:                 cl,
		scheme:             scheme,
		namespace:          "default",
		logf:               t.Logf,
		fleetMaxConcurrent: 3,
		jobImage:           "harmostes-worker:test",
		jobTTLSeconds:      nil,
	}
	return d, context.Background()
}

// gatedDispatchWorkflow: the gate fixture with the durable wake on the CR —
// the webhook annotates before publishing, so the in-process gate reads the
// annotation (env is per-process and the dispatcher is shared).
func gatedDispatchWorkflow() *v1alpha1.Workflow {
	wf := gateWorkflow()
	wf.Name = "pr-review-harmostes"
	wf.Namespace = "default"
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":       "git.rezus.cloud/tibrez/rhesadox#99",
		"harmostes.dev/trigger-action":   "labeled",
		"harmostes.dev/trigger-revision": "headabc123",
	}
	return wf
}

func dispatchRequest() RunRequest {
	return RunRequest{
		Workflow: "pr-review-harmostes", Namespace: "default",
		Pr: "github.com/tibrezus/harmostes#99", Action: "labeled",
		Revision: "headabc123", PrTitle: "t",
	}
}

// #272: proceed → Attempt resolved + Job created with the claim env — the
// graph runs in the Job pod, never in the dispatcher.
func TestDispatchProceedsToJobAndAttempt(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("exactly one Job must be dispatched, got %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Labels["harmostes.dev/workflow"] != "pr-review-harmostes" {
		t.Fatalf("job must carry the workflow label: %+v", job.Labels)
	}
	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["HARMOSTES_DISPATCHED_ATTEMPT"] == "" {
		t.Fatalf("dispatched-claim marker missing: %v", env)
	}
	if env["HARMOSTES_TRIGGER_REPO"] != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("gate envelope not bridged to job env: %v", env)
	}

	var attempts v1alpha1.AttemptList
	if err := d.cl.List(ctx, &attempts); err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts.Items) != 1 {
		t.Fatalf("exactly one Attempt must exist, got %d", len(attempts.Items))
	}
}

// #272/#274: at capacity the dispatcher ACKs without dispatching — capacity
// counts live DISPATCHED CLAIMS (Attempts); the armed marker is the durable
// queue and sweeps retry as slots free.
func TestDispatchCapacityRefusalStaysQueued(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady.MaxConcurrent = 1
	now := time.Now()
	inFlight := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", now.Add(-time.Minute), &now)
	other := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#100", "otherhead00", now.Add(-time.Minute), &now)
	d, ctx := newTestDispatcher(t, wf, inFlight, other)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("capacity refusal must ACK, got error: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("no Job may be created at capacity, got %d", len(jobs.Items))
	}
}

// #272: a gate waiting decision dispatches nothing (label absent on the
// reviewed PR — the gate stays armed and the sweep retries).
func TestDispatchWaitingCreatesNothing(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "headabc123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{}, // label absent
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("waiting dispatch must ACK: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("waiting must not dispatch, got %d jobs", len(jobs.Items))
	}
}

// #272: every class is Job-per-run — a workflow without reviewReady
// dispatches straight through (no gate).
func TestDispatchNonGatedWorkflowDispatches(t *testing.T) {
	clearTriggerEnv(t)
	wf := gatedDispatchWorkflow()
	wf.Spec.ReviewReady = nil
	d, ctx := newTestDispatcher(t, wf)

	req := dispatchRequest()
	req.Action = "" // schedule-shaped: no PR, no gate — plain run
	req.Pr = ""
	if err := d.Dispatch(ctx, req); err != nil {
		t.Fatalf("non-gated dispatch: %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("non-gated workflow must dispatch on trigger, got %d jobs", len(jobs.Items))
	}
}

// #272: two racing wakes for the same PR dedupe on the live attempt Job.
func TestDispatchDedupesLiveAttemptJob(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gatedDispatchWorkflow()
	d, ctx := newTestDispatcher(t, wf)

	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := d.Dispatch(ctx, dispatchRequest()); err != nil {
		t.Fatalf("second dispatch (redelivery): %v", err)
	}
	var jobs batchv1.JobList
	if err := d.cl.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("racing wakes must dedupe to one Job, got %d", len(jobs.Items))
	}
}

// #272: effective capacity — spec override wins, nil/0 takes the fleet
// default.
func TestEffectiveMaxConcurrent(t *testing.T) {
	if got := (*v1alpha1.ReviewReadySpec)(nil).EffectiveMaxConcurrent(3); got != 3 {
		t.Fatalf("nil spec must take fleet default, got %d", got)
	}
	if got := (&v1alpha1.ReviewReadySpec{}).EffectiveMaxConcurrent(3); got != 3 {
		t.Fatalf("zero override must take fleet default, got %d", got)
	}
	if got := (&v1alpha1.ReviewReadySpec{MaxConcurrent: 5}).EffectiveMaxConcurrent(3); got != 5 {
		t.Fatalf("spec override must win, got %d", got)
	}
}

// #287: the Job boundary must forward deployment-level credentials —
// pre-ADR runs inherited the pool env via buildChildEnv; without the
// forward the workspace plugin hits the private Forgejo anonymously (404)
// and the agent node lacks its LLM endpoint.
func TestJobCredentialEnv(t *testing.T) {
	t.Setenv("HARMOSTES_FORGEJO_TOKEN", "fj-token")
	t.Setenv("LITELLM_API_KEY", "llm-key")
	t.Setenv("DAPR_HTTP_PORT", "3500")
	t.Setenv("POD_NAME", "pool-pod")
	t.Setenv("HARMOSTES_VALKEY_PORT_6379_TCP_ADDR", "noise")

	got := jobCredentialEnv()
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "HARMOSTES_FORGEJO_TOKEN=fj-token") || !strings.Contains(joined, "LITELLM_API_KEY=llm-key") {
		t.Fatalf("credential env missing: %q", joined)
	}
	if strings.Contains(joined, "DAPR_") || strings.Contains(joined, "POD_NAME") || strings.Contains(joined, "VALKEY") {
		t.Fatalf("pod-scoped noise must not cross the Job boundary: %q", joined)
	}
}
