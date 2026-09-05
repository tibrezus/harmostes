package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// greenPullBody is the green, labeled, open PR at deadbeef123.
func greenPullBody() map[string]any {
	return map[string]any{
		"state": "open", "head": map[string]string{"sha": "deadbeef123"},
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
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{},
			})
		case strings.Contains(req.URL.Path, "/pulls/100"):
			json.NewEncoder(w).Encode(greenPullBody())
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"body": "review done\n<!-- pr-review: APPROVE @ deadbeef123 -->", "created_at": "2026-08-30T01:00:00Z"},
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
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
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
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
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
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
	st := &fakeStatus{}
	now := time.Now()
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-2*time.Minute), &now)
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-10*time.Minute), &now)
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
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
	st := &fakeStatus{}
	stale := time.Now().Add(-50 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", stale, &stale)
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
	if re.Status.Review == nil || re.Status.Review.Released || re.Status.Review.HeadSHA != "deadbeef123" {
		t.Fatalf("dispatched claim must be armed at the head, got %+v", re.Status.Review)
	}
	if claim.Status.Review.Released == false {
		_ = claim // fixture object is a pre-patch snapshot; state checked via re-list below
	}
	// #343 era reuse: the stale fixture claim (same pr + head) is REVIVED as
	// the deterministic attempt rather than duplicated — exactly ONE live
	// claim for the PR may exist.
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	live := 0
	for _, c := range claims {
		if c.Status.Review.PR == "git.rezus.cloud/tibrez/rhesadox#99" {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("era reuse must leave exactly one live claim for the PR, got %d", live)
	}
}

// ── r5 re-indexed: a request-shaped wake on a claimed PR whose head moved
// supersedes the old claim and arms fresh at the new head. ──
func TestMultiArmRequestWakeSupersedesMovedHead(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t) // PR 99 green at NEW head deadbeef123
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
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
	inFlight := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-2*time.Minute), &now)
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
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
	wf.Annotations = wakeAnnotations("tibrez/rhesadox#99", "labeled", "deadbeef123")
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
	wf.Annotations = wakeAnnotations("github.com/other/repo#7", "labeled", "deadbeef123")
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", stale, nil)
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
	if re.Status.Review == nil || re.Status.Review.Released || re.Status.Review.HeadSHA != "deadbeef123" {
		t.Fatalf("refilled claim must be armed at the head, got %+v", re.Status.Review)
	}
	var old v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &old); err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	// #343 era reuse is the CONTRACT: the refill REVIVES the same (pr, head)
	// claim — the armed era stays sticky, the slot stays filled, and exactly
	// one live claim exists for the pointer (r4 P8: no disjunction — a
	// supersede-recreate regression must fail here).
	if old.Name != re.Name {
		t.Fatalf("era reuse must revive the same attempt, got %s", re.Name)
	}
	if re.Status.Review.Released {
		t.Fatalf("revived era must be live, got %+v", re.Status.Review)
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", fresh, nil)
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", stale, &stale)
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
	// #343 era reuse is the CONTRACT (r4 P8): the refill revives the dead
	// claim's era (same pr + head) — same attempt, live, one claim for the
	// pointer. NOTE: a dispatch-TIMEOUT death is the dead-dispatch class —
	// reuse excludes it (fresh era), so this test's release reason matters;
	// the fixture uses the never-dispatched class by construction.
	if old.Name != out[0].Attempt {
		t.Fatalf("era reuse must revive the same attempt, got %s", out[0].Attempt)
	}
	if old.Status.Review.Released {
		t.Fatalf("revived era must be live, got %+v", old.Status.Review)
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
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", stale, &stale)
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

// ── #328: a dispatched claim with no live job is a dead dispatch — the
// sweeper counts it, releases the slot, and finalizes the ledger (the
// SIGKILLed worker can never write its own outcome). ──
func TestSweepDeadDispatchCountsAndFinalizes(t *testing.T) {
	clearTriggerEnv(t)
	srv := noLabelServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-3 * time.Minute) // past jobDeathGrace: the Job is provably gone
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-3*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	claim.Status.Message = ""
	claim.Status.Runs = []v1alpha1.RunRecord{
		{Name: "run-lost-1", StartedAt: metav1.NewTime(now.Add(-3 * time.Minute)), Phase: "running"},
	}
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("dead claim must not dispatch, got %d", len(out))
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	r := got.Status.Review
	if !r.Released || r.ReleaseReason != "dispatch-lost" {
		t.Fatalf("claim = released:%v reason:%q, want released dispatch-lost", r.Released, r.ReleaseReason)
	}
	if r.DeadDispatches != 1 {
		t.Fatalf("dead dispatches = %d, want 1", r.DeadDispatches)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseFailed {
		t.Fatalf("phase = %q, want failed (the gate is the death observer)", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "run ended without a verdict") {
		t.Fatalf("message = %q, want the honest no-verdict reason", got.Status.Message)
	}
	for _, run := range got.Status.Runs {
		if run.Name == "run-lost-1" && run.Phase != "failed" {
			t.Fatalf("stale running record not finalized: %+v", run)
		}
	}
}

// ── #328: at MaxDeadDispatchesPerHead the breaker refuses the automatic
// re-arm and the sweep surfaces it as the decision, not a failure. ──
func TestSweepBreakerBlocksReDispatch(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledGreenServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// Build the dead-dispatch history through the REAL primitives — the
	// breaker lives on the deterministic attempt ArmClaim will resolve,
	// so a hand-built fixture under a guessed name would never be found.
	deps, ctx := gateEnv(t, wf, st)
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead; i++ {
		at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf,
			"git.rezus.cloud/tibrez/rhesadox#100", "deadbeef123", "needs-review", false)
		if err != nil {
			t.Fatalf("arm %d: %v", i+1, err)
		}
		if err := attempt.MarkClaimDispatched(ctx, deps.Client, wf.Namespace, at.Name); err != nil {
			t.Fatalf("dispatch %d: %v", i+1, err)
		}
		if _, _, err := attempt.ReleaseClaimDead(ctx, deps.Client, wf.Namespace, at.Name, "dispatch-lost"); err != nil {
			t.Fatalf("dead release %d: %v", i+1, err)
		}
	}

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("breaker must block dispatch, got %d", len(out))
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastDecision != "standdown" {
		t.Fatalf("aggregates must surface the breaker, got %+v", st.last.ReviewReady)
	}
	if !strings.Contains(st.last.ReviewReady.LastReason, "dead-dispatch breaker") {
		t.Fatalf("reason = %q, want the breaker explanation", st.last.ReviewReady.LastReason)
	}
	// The evidence survived the refused arm — the sweep must not clobber it.
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: "deadbeef123"})
	at2, _, err := attempt.ResolveOrCreate(ctx, deps.Client, obj, attempt.ResolveOptions{
		Namespace: wf.Namespace, WorkflowRef: wf.Namespace + "/" + wf.Name, Scheme: deps.Scheme,
	})
	if err != nil {
		t.Fatalf("resolve claim: %v", err)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: at2.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Review.DeadDispatches != v1alpha1.MaxDeadDispatchesPerHead {
		t.Fatalf("dead dispatches after refused arm = %d, want %d (evidence preserved)", got.Status.Review.DeadDispatches, v1alpha1.MaxDeadDispatchesPerHead)
	}
}

// labeledGreenServer: the labeled scan finds PR 100, and everything about
// it is green — the drain proceeds to the arm, where the breaker lives.
func labeledGreenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 100, "updated_at": "2026-08-30T00:00:00Z", "labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/100"):
			json.NewEncoder(w).Encode(greenPullBody())
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]any{})
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

// ── #328 escape hatch: an explicit label wake on a breaker-open head is
// the human override — it resets the counter and dispatches. ──
func TestSweepBreakerHumanOverrideDispatches(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead; i++ {
		at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf,
			"git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", "needs-review", false)
		if err != nil {
			t.Fatalf("arm %d: %v", i+1, err)
		}
		_ = attempt.MarkClaimDispatched(ctx, deps.Client, wf.Namespace, at.Name)
		_, _, _ = attempt.ReleaseClaimDead(ctx, deps.Client, wf.Namespace, at.Name, "dispatch-lost")
	}

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("human override must dispatch, got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil || len(claims) != 1 {
		t.Fatalf("override must leave a live claim, got %d (%v)", len(claims), err)
	}
	if claims[0].Status.Review.DeadDispatches != 0 {
		t.Fatalf("override must reset the counter, got %d", claims[0].Status.Review.DeadDispatches)
	}
}

// ── #328 blocking finding: the two release passes (timer + job-death) both
// observe a claim dispatched 46m ago with a dead Job — the death must be
// counted ONCE (idempotent ReleaseClaimDead), or MaxDeadDispatchesPerHead
// is secretly 2 cycles. ──
func TestSweepDeadDispatchCountedOnceAcrossPasses(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-46 * time.Minute) // past DispatchTimeout (45m) AND jobDeathGrace (2m)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-46*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	claim.Status.Runs = []v1alpha1.RunRecord{
		{Name: "run-lost-x", StartedAt: metav1.NewTime(disp), Phase: "running"},
	}
	deps, ctx := gateEnv(t, wf, st, claim)

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Review.DeadDispatches != 1 {
		t.Fatalf("one death counted %d times — double-count halves the breaker's budget", got.Status.Review.DeadDispatches)
	}
	if got.Status.Review.ReleaseReason != "dispatch-timeout" {
		t.Fatalf("first observer wins the classification, got %q", got.Status.Review.ReleaseReason)
	}
	if got.Status.Phase != v1alpha1.AttemptPhaseFailed {
		t.Fatalf("phase = %q, want failed", got.Status.Phase)
	}
}

// ── #328: the override fires even when a claim is LIVE at the same head
// (armed with prior deaths): the labeled wake supersedes (uncounted),
// re-arms, and resets the counter. ──
func TestSweepBreakerOverrideThroughLiveClaim(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	wf.Annotations = wakeAnnotations("git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)
	// Two deaths, then a re-arm: a LIVE claim holding a partial count.
	for i := 0; i < 2; i++ {
		at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf,
			"git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", "needs-review", false)
		if err != nil {
			t.Fatalf("arm %d: %v", i+1, err)
		}
		_ = attempt.MarkClaimDispatched(ctx, deps.Client, wf.Namespace, at.Name)
		_, _, _ = attempt.ReleaseClaimDead(ctx, deps.Client, wf.Namespace, at.Name, "dispatch-lost")
	}
	if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf,
		"git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", "needs-review", false); err != nil {
		t.Fatalf("re-arm: %v", err)
	}

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("labeled wake through a live partial-count claim must dispatch, got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf.Namespace, wf.Name)
	if err != nil || len(claims) != 1 {
		t.Fatalf("exactly one live claim after override, got %d (%v)", len(claims), err)
	}
	if claims[0].Status.Review.DeadDispatches != 0 {
		t.Fatalf("override must reset the counter, got %d", claims[0].Status.Review.DeadDispatches)
	}
}

// neverVerdictServer: the single-PR fetch answers, the verdict check finds
// no comments — the in-flight claim stays, waiting.
func noVerdictServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, req)
		}
	}))
}

// #331: the dispatch-timeout bound presumes death, but if the attempt's Job
// is observably still alive, the fact wins — no count, no release, the claim
// stays live for the job-death pass to classify from fact. The negative
// control (a live Job belonging to a DIFFERENT attempt) pins the label
// matcher: an implementation that treats any live Job as proof of life must
// fail.
func TestSweepDispatchTimeoutWithAliveJobNotCounted(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-46 * time.Minute) // past DispatchTimeout (45m)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#101", "deadbeef321", now.Add(-46*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-job-alive", Namespace: wf.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "harmostes",
				"harmostes.dev/workflow": wf.Name,
				v1alpha1.AttemptLabel:    claim.Name,
			},
		},
	}
	deps, ctx := gateEnv(t, wf, st, claim, job)

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.Review.Released {
		t.Fatal("an observably alive run must not be released by the timer pass")
	}
	if got.Status.Review.DeadDispatches != 0 {
		t.Fatalf("dead dispatches = %d, want 0 — a live run counted dead burns breaker budget", got.Status.Review.DeadDispatches)
	}
	if got.Status.Phase == v1alpha1.AttemptPhaseFailed {
		t.Fatal("ledger finalized a failed phase while the run is still alive")
	}
	// Positive control on the mechanism: the hold is durable evidence, not a
	// silent pass — the sweep's aggregates record the standdown evaluation
	// with the hold.
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastDecision != "standdown" || !strings.Contains(st.last.ReviewReady.LastReason, "held") {
		t.Fatalf("aggregates must record the held standdown, got %+v", st.last.ReviewReady)
	}
}

// Negative control: a live Job belonging to a DIFFERENT attempt is not
// proof of life for this claim — the label matcher is what saves the run,
// so with only a foreign live Job present, the dispatch-timeout death is
// counted.
func TestSweepDispatchTimeoutForeignLiveJobStillCounts(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-46 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#102", "deadbeef432", now.Add(-46*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	foreign := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-job-foreign", Namespace: wf.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "harmostes",
				"harmostes.dev/workflow": wf.Name,
				v1alpha1.AttemptLabel:    "attempt-some-other-claim",
			},
		},
	}
	deps, ctx := gateEnv(t, wf, st, claim, foreign)

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if !got.Status.Review.Released || got.Status.Review.DeadDispatches != 1 {
		t.Fatalf("a foreign live Job is not proof of life: released=%v dead=%d, want released=true dead=1", got.Status.Review.Released, got.Status.Review.DeadDispatches)
	}
}

// Fail-closed: a ListActiveJobs failure latches for the whole sweep, so the
// timer pass cannot consult the fact — release is destructive, unknown is
// treated as dead (the pre-#328-class behavior must not silently skip).
func TestSweepDispatchTimeoutJobListFailureCountsDead(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-46 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#103", "deadbeef543", now.Add(-46*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling

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
		WithRuntimeObjects(claim).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*batchv1.JobList); ok {
				return fmt.Errorf("api blip")
			}
			return cl.List(ctx, list, opts...)
		}}).
		Build()
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf}
	ctx := context.Background()

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if !got.Status.Review.Released || got.Status.Review.DeadDispatches != 1 || got.Status.Review.ReleaseReason != "dispatch-timeout" {
		t.Fatalf("unknown liveness must fail closed: released=%v dead=%d reason=%q", got.Status.Review.Released, got.Status.Review.DeadDispatches, got.Status.Review.ReleaseReason)
	}
}

// ── r6 P1: the aged never-dispatched release is DISPATCH-LOST, not horizon
// — "we could not dispatch" must not set the ambiguity guard's clock ("we
// stopped asking"). The cycle now converges through the COUNTER: each sweep
// releases the never-dispatched era again (#1, #2), the third release arms
// the guard (Max dispatch-lost releases), and a human re-request escapes
// with a fresh clock (r6 P2). ──
func TestMultiArmNeverDispatchedAgedClaimConvergesDispatchLost(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 103)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow() // horizon 6h
	aged := time.Now().Add(-7 * time.Hour)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#103", "deadbeef777", aged, nil)
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim)

	const pr = "git.rezus.cloud/tibrez/rhesadox#103"
	const sha = "deadbeef777"
	expectRelease := func(n int) {
		t.Helper()
		var got v1alpha1.Attempt
		if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
			t.Fatalf("get claim: %v", err)
		}
		if !got.Status.Review.Released || got.Status.Review.ReleaseReason != v1alpha1.ReleaseReasonDispatchLost {
			t.Fatalf("aged never-dispatched claim must release as dispatch-lost #%d, got released=%v reason=%q",
				n, got.Status.Review.Released, got.Status.Review.ReleaseReason)
		}
		if got.Status.Review.DispatchLostReleases != n {
			t.Fatalf("dispatch-lost release #%d must bump the counter to %d, got %d", n, n, got.Status.Review.DispatchLostReleases)
		}
	}

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	expectRelease(1)
	if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("auto revival #1 (counter under max) must reuse the era: %v", err)
	}

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	expectRelease(2)
	if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("auto revival #2: %v", err)
	}

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	expectRelease(3)
	// The guard: the next AUTOMATIC arm is refused — the cycle converged
	// into a visible standdown, not an infinite loop.
	if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, pr, sha, "needs-review", false); !errors.Is(err, attempt.ErrRecentlyDismissed) {
		t.Fatalf("exhausted counter must refuse the auto re-arm, got %v", err)
	}
	// r6 P2: the HUMAN override escapes — with a FRESH clock (the anchored
	// one is already past the horizon and would re-release instantly).
	at2, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, pr, sha, "needs-review", true)
	if err != nil {
		t.Fatalf("human re-request must override the exhausted counter: %v", err)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: at2.Name}, &got); err != nil {
		t.Fatal(err)
	}
	r2 := got.Status.Review
	if r2.Released {
		t.Fatal("the overridden era must be live")
	}
	if age := time.Since(r2.ArmedSince.Time); age > time.Minute {
		t.Fatalf("human override must anchor a FRESH era clock, got age %v", age)
	}
	if r2.DispatchLostReleases != 0 {
		t.Fatalf("human wake must reset the counter, got %d", r2.DispatchLostReleases)
	}
}
