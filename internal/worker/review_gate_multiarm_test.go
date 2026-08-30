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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
)

// gateEnv: a fake k8s client (claim storage) + fake status (aggregates) +
// GateDeps wired to both. Callers pin the review API via pinReviewAPI.
func gateEnv(t *testing.T, wf *v1alpha1.Workflow, st *fakeStatus, objects ...runtime.Object) (GateDeps, context.Context) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		WithRuntimeObjects(objects...).
		Build()
	deps := GateDeps{
		Status:             st,
		Client:             cl,
		Scheme:             scheme,
		FleetMaxConcurrent: 3,
		Log:                t.Logf,
	}
	return deps, context.Background()
}

// claimFixture builds an unreleased review claim on wf.
func claimFixture(wf *v1alpha1.Workflow, pr, sha string, armedSince time.Time, dispatchedAt *time.Time) *v1alpha1.Attempt {
	name := "attempt-claim-" + pr[strings.LastIndex(pr, "#")+1:]
	at := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: wf.Namespace,
			CreationTimestamp: metav1.NewTime(armedSince.Add(-time.Minute)),
			Labels:            map[string]string{"harmostes.dev/workflow": wf.Name},
		},
		Spec: v1alpha1.AttemptSpec{WorkflowRef: wf.Namespace + "/" + wf.Name},
	}
	r := &v1alpha1.ReviewClaimStatus{PR: pr, HeadSHA: sha, Label: "needs-review"}
	t := metav1.NewTime(armedSince)
	r.ArmedSince = &t
	if dispatchedAt != nil {
		d := metav1.NewTime(*dispatchedAt)
		r.DispatchedAt = &d
	}
	at.Status.Review = r
	return at
}

func wakeAnnotations(pr, action, sha string) map[string]string {
	return map[string]string{
		"harmostes.dev/trigger-pr":       pr,
		"harmostes.dev/trigger-action":   action,
		"harmostes.dev/trigger-revision": sha,
	}
}

// greenPullBody is the green, labeled, open PR at headabc123.
func greenPullBody() map[string]any {
	return map[string]any{
		"state": "open", "head": map[string]string{"sha": "headabc123"},
		"base":   map[string]string{"ref": "main"},
		"labels": []map[string]string{{"name": "needs-review"}},
	}
}

// labeledListServer serves the labeled-PR scan (two green labeled PRs).
func labeledListServer(t *testing.T, numbers ...int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			var pulls []map[string]any
			for _, n := range numbers {
				pulls = append(pulls, map[string]any{
					"number": n, "updated_at": "2026-08-30T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}},
				})
			}
			json.NewEncoder(w).Encode(pulls)
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(greenPullBody())
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
}

// consumedServer: PR 99 has NO label but a verdict comment (the consume
// signal); the labeled scan lists PR 100.
func consumedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]any{{"number": 100, "updated_at": "2026-08-30T00:00:00Z", "labels": []map[string]string{{"name": "needs-review"}}}})
		case strings.HasSuffix(req.URL.Path, "/pulls/99"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "headabc123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{},
			})
		case strings.Contains(req.URL.Path, "/pulls/100"):
			json.NewEncoder(w).Encode(greenPullBody())
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"body": "review done\n<!-- pr-review: APPROVE @ headabc123 -->", "created_at": "2026-08-30T01:00:00Z"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
}

// noLabelServer: the PR is open and green but carries no label.
func noLabelServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "headabc123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{},
			})
		default:
			http.NotFound(w, req)
		}
	}))
}

// ── Waiting: label absent, no verdict → the claim arms (persists) and
// nothing dispatches. ──
func TestMultiArmWaitingArmsClaimWithoutDispatch(t *testing.T) {
	clearTriggerEnv(t)
	srv := noLabelServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "headabc123")
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("waiting must not dispatch, got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil || len(claims) != 1 {
		t.Fatalf("waiting must arm a claim, got %d (%v)", len(claims), err)
	}
	if claims[0].Status.Review.Released {
		t.Fatal("waiting claim must stay unreleased")
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastDecision != "waiting" {
		t.Fatalf("aggregates must record waiting, got %+v", st.last.ReviewReady)
	}
}

// ── r4 core: proceed dispatches; a SECOND sweep with the claim dispatched
// must not re-dispatch (the verdict window is the consume signal). ──
func TestMultiArmInFlightNotReDispatched(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "headabc123")
	st := &fakeStatus{}
	now := time.Now()
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", now.Add(-2*time.Minute), &now)
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("in-flight claim must not re-dispatch, got %d dispatches", len(out))
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LiveClaims != 1 {
		t.Fatalf("aggregates must count the in-flight claim, got %+v", st.last.ReviewReady)
	}
}

// ── r6: verdict posted + label absent → the claim consumes, the slot
// frees, and the NEXT labeled PR dispatches on the same sweep. ──
func TestMultiArmVerdictConsumesAndDrains(t *testing.T) {
	clearTriggerEnv(t)
	srv := consumedServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", now.Add(-10*time.Minute), &now)
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 || out[0].Envelope.PR != 100 {
		t.Fatalf("consumed claim must free the slot for PR 100, got %+v (aggregates %+v)", out, st.last.ReviewReady)
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	for _, c := range claims {
		if c.Status.Review.PR == "git.rezus.cloud/tibrez/rhesadox#99" && !c.Status.Review.Released {
			t.Fatal("the consumed claim must be released")
		}
	}
}

// ── #248: dispatch timeout expires a dead run; the still-labeled PR re-arms
// as a FRESH claim (bounded retry, no external label toggle). ──
func TestMultiArmDispatchTimeoutReleasesAndReArms(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Spec.ReviewReady.DispatchTimeout = "45m"
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "headabc123")
	st := &fakeStatus{}
	stale := time.Now().Add(-50 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", stale, &stale)
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expired claim must re-dispatch (bounded retry), got %d", len(out))
	}
	// The re-dispatch arms the DETERMINISTIC attempt (workflow × head —
	// ADR-0005): armed, unreleased, at the reviewed head. The stale fixture
	// claim (same PR, artificial name) must stay released — one live claim
	// per PR.
	var re v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: out[0].Attempt}, &re); err != nil {
		t.Fatalf("dispatched claim: %v", err)
	}
	if re.Status.Review == nil || re.Status.Review.Released || re.Status.Review.HeadSHA != "headabc123" {
		t.Fatalf("dispatched claim must be armed at the head, got %+v", re.Status.Review)
	}
	if claim.Status.Review.Released == false {
		_ = claim // fixture object is a pre-patch snapshot; state checked via re-list below
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	for _, c := range claims {
		if c.Name == claim.Name {
			t.Fatal("the stale fixture claim must not be live alongside the deterministic one")
		}
	}
}

// ── r5 re-indexed: a request-shaped wake on a claimed PR whose head moved
// supersedes the old claim and arms fresh at the new head. ──
func TestMultiArmRequestWakeSupersedesMovedHead(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t) // PR 99 green at NEW head headabc123
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "headabc123")
	st := &fakeStatus{}
	armed := time.Now().Add(-5 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "oldhead000", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("head move must re-arm and dispatch, got %d", len(out))
	}
	if out[0].Attempt == claim.Name {
		t.Fatal("the moved-head review must arm a NEW claim")
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	for _, c := range claims {
		if c.Name == claim.Name && !c.Status.Review.Released {
			t.Fatal("the stale-head claim must be released as superseded")
		}
	}
}

// ── ADR-0007 drain-to-capacity: two labeled green PRs + capacity 2 → both
// dispatch in ONE sweep. ──
func TestMultiArmDrainsToCapacity(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99, 100)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Spec.ReviewReady.MaxConcurrent = 2
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("one sweep must fill capacity, got %d dispatches", len(out))
	}
}

// ── Capacity: a live dispatched claim leaves no free slot. ──
func TestMultiArmCapacityHoldsQueue(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99, 100)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Spec.ReviewReady.MaxConcurrent = 1
	st := &fakeStatus{}
	now := time.Now()
	inFlight := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", now.Add(-2*time.Minute), &now)
	deps, ctx := gateEnv(t, wf, st, inFlight)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("at capacity nothing new may dispatch, got %d", len(out))
	}
}

// ── Per-PR dedupe: a labeled candidate whose PR already has an armed-queued
// claim is skipped (the labeled scan sees it every sweep). ──
func TestMultiArmLiveClaimSkipsCandidate(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("the live claim owns the PR — no duplicate dispatch, got %d", len(out))
	}
}

// ── #268 in claim form: a bare-form wake arms the host-qualified claim. ──
func TestMultiArmBarePointerNormalizesIntoClaim(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("tibrez/rhesadox#99", "labeled", "headabc123")
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 1 || out[0].Envelope.Repo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("bare pointer must normalize, got %+v", out)
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if len(claims) != 1 || claims[0].Status.Review.PR != "git.rezus.cloud/tibrez/rhesadox#99" {
		t.Fatalf("claim must carry the normalized pointer, got %+v", claims)
	}
}

// ── Out-of-scope wakes arm and dispatch nothing. ──
func TestMultiArmOutOfScopeWakeIgnored(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("github.com/other/repo#7", "labeled", "headabc123")
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out-of-scope wake must not dispatch, got %d", len(out))
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if len(claims) != 0 {
		t.Fatalf("out-of-scope wake must not arm, got %d claims", len(claims))
	}
}

// #279: a claim armed but never dispatched (its arming sweep died before
// the Job create landed) must not hold the PR until the horizon. Past the
// re-dispatch grace, the sweep releases it as dispatch-lost and re-fills
// the slot in the same cycle.
func TestMultiArmDispatchLostClaimRefilled(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	stale := time.Now().Add(-10 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", stale, nil)
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("dispatch-lost claim must be refilled in the same sweep, got %d dispatches", len(out))
	}
	var re v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: out[0].Attempt}, &re); err != nil {
		t.Fatalf("re-armed claim: %v", err)
	}
	if re.Status.Review == nil || re.Status.Review.Released || re.Status.Review.HeadSHA != "headabc123" {
		t.Fatalf("refilled claim must be armed at the head, got %+v", re.Status.Review)
	}
	var old v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &old); err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	if old.Status.Review == nil || !old.Status.Review.Released || old.Status.Review.ReleaseReason != "dispatch-lost" {
		t.Fatalf("stale claim must be released as dispatch-lost, got %+v", old.Status.Review)
	}
}

// #279: a freshly armed claim (inside the grace) is left alone — its own
// sweep's dispatch loop is still in flight.
func TestMultiArmFreshArmedClaimHoldsDuringGrace(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	fresh := time.Now().Add(-time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", fresh, nil)
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("in-grace claim must not be re-dispatched, got %d", len(out))
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.Status.Review == nil || got.Status.Review.Released {
		t.Fatalf("in-grace claim must stay live, got %+v", got.Status.Review)
	}
}

// #285: a dispatched claim whose Job is terminally failed does not wait
// out the DispatchTimeout — the sweep sees no live Job and refills.
func TestMultiArmDeadJobClaimRefilled(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	stale := time.Now().Add(-10 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", stale, &stale)
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim) // no Job seeded: the run died

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("dead-job claim must be refilled in the same sweep, got %d dispatches", len(out))
	}
	var old v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &old); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if old.Status.Review == nil || !old.Status.Review.Released || old.Status.Review.ReleaseReason != "dispatch-lost" {
		t.Fatalf("dead-job claim must be released as dispatch-lost, got %+v", old.Status.Review)
	}
}

// #285: a dispatched claim WITH a live Job (past the grace) is untouched —
// the run is in flight; the dispatch-timeout bound still governs it.
func TestMultiArmLiveJobClaimHolds(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	stale := time.Now().Add(-10 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "headabc123", stale, &stale)
	liveJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "attempt-claim-99-rx8sr", Namespace: wf.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name": "harmostes",
			"harmostes.dev/workflow": wf.Name,
			"harmostes.dev/attempt":  claim.Name,
		},
	}}
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim, liveJob)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("in-flight claim must not be re-dispatched, got %d", len(out))
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.Status.Review == nil || got.Status.Review.Released {
		t.Fatalf("live-job claim must stay dispatched, got %+v", got.Status.Review)
	}
}
