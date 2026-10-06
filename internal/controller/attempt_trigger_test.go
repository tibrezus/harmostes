package controller

// Regression + acceptance tests for the consume-on-completion trigger
// (#647): an attempt's run reached terminal → its workflow is due now, at
// event time, keyed to the webhook floor in the trigger-slot CAS — never to
// the 5m poll anchor an armed sweep stamped seconds ago (harmostes#646
// evidence 4: a review Job Completed ~00:20 with the verdict already
// posted; every trigger in between logged "already claimed" until a sweep
// noticed at ~00:32).
//
// The kernel vocabulary here is deliberately gate-free: Runs, claims,
// WorkflowRef. The sweep stays the consumer; these tests pin only WHEN it
// is asked to look.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/dapr"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// triggerFixture647 builds a workflow armed by the review-ready gate with
// one live-claim attempt. runPhase/runEnded control the run ledger state
// the trigger reads. released flips the claim's liveness. LastRunAt is
// stamped 15s ago — INSIDE the 5m poll anchor, OUTSIDE the 10s webhook
// floor — so a publish proves the terminal wake was short-keyed, and its
// absence proves the anchor held.
func triggerFixture647(t *testing.T, runPhase string, runEnded *time.Time, released bool) (*WorkflowReconciler, *atomic.Int64, *v1alpha1.Workflow) {
	t.Helper()
	now := metav1.Now()
	lastRun := metav1.NewTime(now.Add(-15 * time.Second))

	wf := &v1alpha1.Workflow{}
	wf.Name = "wiki-lint-test"
	wf.Namespace = "harmostes"
	wf.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{}
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{LiveClaims: 1}
	wf.Status.LastRunAt = lastRun

	run := v1alpha1.RunRecord{Name: "run-1", StartedAt: metav1.NewTime(now.Add(-time.Minute)), Phase: runPhase}
	if runEnded != nil {
		run.EndedAt = metav1.NewTime(*runEnded)
	}
	at := &v1alpha1.Attempt{}
	at.Name = "wiki-lint-test-5037cf50"
	at.Namespace = "harmostes"
	at.Labels = map[string]string{
		v1alpha1.WorkflowLabel:     "wiki-lint-test",
		v1alpha1.ObjectiveKindLabel: v1alpha1.ObjectiveKindPRReview,
		// v1alpha1.ReviewClaimLabel deliberately ABSENT: absence means live.
	}
	at.Spec.WorkflowRef = "harmostes/wiki-lint-test"
	at.Status.Review = &v1alpha1.ReviewClaimStatus{PR: "git.rezus.cloud/tibrez/rhesadox#2621", HeadSHA: "5037cf50", Released: released}
	at.Status.Runs = []v1alpha1.RunRecord{run}

	var publishes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publishes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(wf, at).
		Build()

	r := &WorkflowReconciler{
		Client:       cl,
		Scheme:       k8s.Scheme(),
		PollInterval: 5 * time.Minute,
		DaprClient:   dapr.New(srv.URL),
		TriggerTopic: "harmostes-triggers",
	}
	return r, &publishes, wf
}

func reconcile647(t *testing.T, r *WorkflowReconciler, wf *v1alpha1.Workflow) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// T1 — acceptance: a terminal run on a live claim publishes NOW (the
// webhook floor, not the poll anchor). 15s since LastRunAt proves the
// short keying: under the old keying this reconcile lost the slot.
func Test647TerminalRunTriggersNow(t *testing.T) {
	ended := time.Now().Add(-time.Second)
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, false)
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 1 {
		t.Fatalf("issue #647: a terminal run on a live claim must publish immediately, got %d publish(es)", n)
	}
}

// T2 — the #646 evidence-4 gap shape, asserted as desired: the run is still
// RUNNING (nothing to consume) → no event-time publish; the armed poll
// anchor holds the cadence.
func Test647RunningRunStaysOnPollAnchor(t *testing.T) {
	r, publishes, wf := triggerFixture647(t, "running", nil, false)
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 0 {
		t.Fatalf("a running run must not consume the event-time wake, got %d publish(es)", n)
	}
}

// T3 — the watermark de-arms: a terminal run that ended BEFORE LastRunAt
// was already on the dispatch the stamp anchors — no second publish.
func Test647WatermarkDeArmsConsumedCompletion(t *testing.T) {
	ended := time.Now().Add(-20 * time.Second) // ended before the 15s-ago stamp
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, false)
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 0 {
		t.Fatalf("a terminal run older than LastRunAt is already consumed, got %d publish(es)", n)
	}
}

// T4 — no live claim, no wake: released eras are consumed history.
func Test647ReleasedClaimDoesNotWake(t *testing.T) {
	ended := time.Now().Add(-time.Second)
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, true)
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 0 {
		t.Fatalf("a released claim must not consume the wake, got %d publish(es)", n)
	}
}

// T5 — at-least-once safe: the SAME completion reconciled twice publishes
// exactly once (the stamp advances past EndedAt; the CAS holds the rest).
func Test647OneCompletionPublishesOnce(t *testing.T) {
	ended := time.Now().Add(-time.Second)
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, false)
	reconcile647(t, r, wf)
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 1 {
		t.Fatalf("one completion must publish exactly once (no storms), got %d", n)
	}
}

// T6 — workflows the gate never armed pay no wake: the terminal condition
// is gated on ReviewReady state, so first dispatch stays owned by the
// schedule/webhook paths.
func Test647NonGateWorkflowPaysNoWake(t *testing.T) {
	ended := time.Now().Add(-time.Second)
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, false)
	r.Client = fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		WithObjects(func() client.Object {
			c := wf.DeepCopy()
			c.Status.ReviewReady = nil // the gate never armed this workflow
			return c
		}()).
		Build()
	reconcile647(t, r, wf)
	if n := publishes.Load(); n != 0 {
		t.Fatalf("a never-armed workflow must not consume the terminal wake, got %d publish(es)", n)
	}
}

// T7 — the wake predicate admits ONLY run-terminal transitions: double
// terminal patches, hold-note patches, and non-update events stay silent.
func Test647PredicateOnlyTransitions(t *testing.T) {
	p := runTerminalPredicate{}
	run := func(phase string, ended metav1.Time) v1alpha1.RunRecord {
		return v1alpha1.RunRecord{Name: "run-1", Phase: phase, EndedAt: ended}
	}
	now := metav1.Now()
	attemptWith := func(runs ...v1alpha1.RunRecord) *v1alpha1.Attempt {
		at := &v1alpha1.Attempt{}
		at.Status.Runs = runs
		return at
	}

	// running → succeeded: the completion event.
	if !p.Update(event.UpdateEvent{ObjectOld: attemptWith(run("running", metav1.Time{})), ObjectNew: attemptWith(run("succeeded", now))}) {
		t.Fatal("running→succeeded must pass the predicate")
	}
	// already-terminal → already-terminal: re-recorded outcome (worker
	// retry patch) — not a completion.
	if p.Update(event.UpdateEvent{ObjectOld: attemptWith(run("succeeded", now)), ObjectNew: attemptWith(run("succeeded", now))}) {
		t.Fatal("terminal→terminal must not pass the predicate")
	}
	// unrelated status patch (hold note): no run transition at all.
	if p.Update(event.UpdateEvent{ObjectOld: attemptWith(run("running", metav1.Time{})), ObjectNew: attemptWith(run("running", metav1.Time{}))}) {
		t.Fatal("a patch without a run transition must not pass the predicate")
	}
	// ledger finalize (running → failed): the dead-run completion.
	if !p.Update(event.UpdateEvent{ObjectOld: attemptWith(run("running", metav1.Time{})), ObjectNew: attemptWith(run("failed", now))}) {
		t.Fatal("running→failed must pass the predicate")
	}
	if p.Create(event.CreateEvent{Object: attemptWith(run("succeeded", now))}) {
		t.Fatal("creates must not pass (runs start running)")
	}
	if p.Delete(event.DeleteEvent{Object: attemptWith(run("succeeded", now))}) {
		t.Fatal("deletes must not pass")
	}
	if p.Generic(event.GenericEvent{Object: attemptWith(run("succeeded", now))}) {
		t.Fatal("generic events must not pass")
	}
	// non-Attempt objects: defensively silent.
	if p.Update(event.UpdateEvent{ObjectOld: &v1alpha1.Workflow{}, ObjectNew: &v1alpha1.Workflow{}}) {
		t.Fatal("non-Attempt updates must not pass")
	}
}

// T8 — the map resolves spec.workflowRef ("namespace/name") to the owning
// workflow's reconcile request; malformed/empty refs yield nothing (the
// ledger is best-effort history, never a trigger authority).
func Test647MapResolvesWorkflowRef(t *testing.T) {
	r := &WorkflowReconciler{}
	at := &v1alpha1.Attempt{}
	at.Namespace = "harmostes"
	at.Spec.WorkflowRef = "harmostes/wiki-lint-test"
	reqs := r.attemptTerminalToWorkflow(context.Background(), at)
	if len(reqs) != 1 || reqs[0].Namespace != "harmostes" || reqs[0].Name != "wiki-lint-test" {
		t.Fatalf("map must resolve the owning workflow, got %v", reqs)
	}
	for _, bad := range []string{"", "noslash", "ns/", "/name"} {
		at.Spec.WorkflowRef = bad
		if reqs := r.attemptTerminalToWorkflow(context.Background(), at); len(reqs) != 0 {
			t.Fatalf("malformed workflowRef %q must yield no requests, got %v", bad, reqs)
		}
	}
	// non-Attempt objects: no requests.
	if reqs := r.attemptTerminalToWorkflow(context.Background(), &v1alpha1.Workflow{}); len(reqs) != 0 {
		t.Fatalf("non-Attempt objects must yield no requests, got %v", reqs)
	}
}

// T9 — the published event is a well-formed TriggerEvent whose payload
// still carries the workflow identity (the worker's consumption contract
// is unchanged — only the WHEN moved).
func Test647TriggerEventPayloadUnchanged(t *testing.T) {
	var got TriggerEvent
	var mu = make(chan struct{}, 1)
	ended := time.Now().Add(-time.Second)
	r, publishes, wf := triggerFixture647(t, "succeeded", &ended, false)
	// Rebuild the Dapr server side to capture the payload: simplest is to
	// re-wrap — the fixture's server counts only, so assert via a second
	// publish capture on the SAME reconcile path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		buf := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(buf)
		_ = json.Unmarshal(buf, &got)
		publishes.Add(1)
		select {
		case mu <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	r.DaprClient = dapr.New(srv.URL)

	reconcile647(t, r, wf)
	<-mu
	if got.Workflow != "wiki-lint-test" || got.Namespace != "harmostes" {
		t.Fatalf("trigger event must carry the workflow identity, got %+v", got)
	}
}

// T10 — the wake tells the truth: the published event's triggerType is
// "run-terminal", not "schedule" (the timeline must not need an
// investigation to tell a completion wake from a poll tick — #646).
func Test647TriggerTypeIsRunTerminal(t *testing.T) {
	var got TriggerEvent
	ready := make(chan struct{}, 1)
	ended := time.Now().Add(-time.Second)
	r, _, wf := triggerFixture647(t, "succeeded", &ended, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		buf := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(buf)
		_ = json.Unmarshal(buf, &got)
		ready <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	r.DaprClient = dapr.New(srv.URL)

	reconcile647(t, r, wf)
	<-ready
	if got.TriggerType != "run-terminal" {
		t.Fatalf("a completion wake must publish triggerType run-terminal, got %q", got.TriggerType)
	}
}
