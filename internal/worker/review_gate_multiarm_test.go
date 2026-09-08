package worker

import (
	"context"
	"encoding/json"
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// claimFixture builds an unreleased review claim on wf. The attempt name
// is the REAL derived identity (source repo + head SHA) — ArmClaim resolves
// pointer-locally by that name (r7 P1), so a synthetic name would silently
// test a different object.
func claimFixture(wf *v1alpha1.Workflow, pr, sha string, armedSince time.Time, dispatchedAt *time.Time) *v1alpha1.Attempt {
	obj := attempt.DeriveObjective(wf, attempt.TriggerContext{Revision: sha, Source: "webhook"})
	name := attempt.AttemptName(wf.Name, attempt.Identity(obj))
	at := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: wf.Namespace,
			CreationTimestamp: metav1.NewTime(armedSince.Add(-time.Minute)),
			// No review-claim marker: ABSENCE means live (r8 P1) — the
			// fixture IS the pre-upgrade shape. The objective-kind label
			// rides along: the live-list selector pins it.
			Labels: map[string]string{
				"harmostes.dev/workflow":       wf.Name,
				"harmostes.dev/objective-kind": attempt.DeriveKind(wf),
			},
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

// gateEnvW is gateEnv with the trigger event threaded the production way:
// GateDeps.Wake* as handed down from the RunRequest (#349). The old
// annotation/env scraping is gone — the controller clears the annotations
// at schedule time and the env vars land on dispatched JOB pods (which
// skip the gate), so in the worker-pool topology the wake never arrived.
func gateEnvW(t *testing.T, wf *v1alpha1.Workflow, st *fakeStatus, pr, action, sha string, objects ...runtime.Object) (GateDeps, context.Context) {
	deps, ctx := gateEnv(t, wf, st, objects...)
	deps.Wake = GateWake{PR: pr, Action: action, Revision: sha}
	return deps, ctx
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("waiting must not dispatch, got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	now := time.Now()
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-2*time.Minute), &now)
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123", claim)

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
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	stale := time.Now().Add(-50 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", stale, &stale)
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123", claim)

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
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	armed := time.Now().Add(-5 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "oldhead000", armed, nil)
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123", claim)

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
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// The saturated sweep SKIPS the labeled scan (r11 — the one
	// unbounded-per-repo call) and says so in the aggregates: "capacity
	// full", not the sweep-start "nothing to evaluate" (pillar 7).
	if st.last.ReviewReady == nil {
		t.Fatal("no aggregates recorded")
	}
	if !strings.Contains(st.last.ReviewReady.LastReason, "capacity full") {
		t.Fatalf("a saturated sweep must report capacity, got %q", st.last.ReviewReady.LastReason)
	}
}

// ── Per-PR dedupe: a labeled candidate whose PR already has an armed-queued
// claim re-evaluates THE EXISTING claim (r27 #379): when CI is green the
// sweep completes the dispatch against the SAME attempt — it must never arm
// a second attempt for a claimed pointer. ──
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
	if len(out) != 1 {
		t.Fatalf("the green queued claim must complete its dispatch, got %d", len(out))
	}
	if out[0].Attempt != claim.Name {
		t.Fatalf("dispatch must reuse the existing attempt %s, got %s", claim.Name, out[0].Attempt)
	}
}

// ── #268 in claim form: a bare-form wake arms the host-qualified claim. ──
func TestMultiArmBarePointerNormalizesIntoClaim(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "tibrez/rhesadox#99", "labeled", "deadbeef123")

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 1 || out[0].Envelope.Repo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("bare pointer must normalize, got %+v", out)
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "github.com/other/repo#7", "labeled", "deadbeef123")

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out-of-scope wake must not dispatch, got %d", len(out))
	}
	claims, _ := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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

// #279/r27: a freshly armed claim whose CI is GREEN completes its dispatch
// on the next sweep — same attempt, no re-arm (the createMu + live-Job
// dedupe make a racing arming sweep safe). The claim stays live either way.
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
	if len(out) != 1 {
		t.Fatalf("green queued claim must complete its dispatch, got %d", len(out))
	}
	if out[0].Attempt != claim.Name {
		t.Fatalf("dispatch must reuse the existing attempt %s, got %s", claim.Name, out[0].Attempt)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got.Status.Review == nil || got.Status.Review.Released {
		t.Fatalf("dispatched claim must stay live, got %+v", got.Status.Review)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
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
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
	// wake rides the EVENT (GateDeps.Wake*), not annotations (#349):
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123")
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
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
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
// r27 #379: an aged queued claim converges through Evaluate's horizon
// bound — the claim is released as HORIZON (the ambiguity dismissal), NOT
// dispatch-lost: waiting for CI past the horizon is not a dispatch failure
// and must not burn the churn budget. The churn counter converges at the
// re-dispatch boundary instead
// (TestSweepRefusalAttribution_ChurnRefusalReportsBudget).
func TestMultiArmAgedQueuedClaimConvergesHorizon(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 103)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow() // horizon 6h
	aged := time.Now().Add(-7 * time.Hour)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#103", "deadbeef123", aged, nil)
	deps, ctx := gateEnv(t, wf, &fakeStatus{}, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a horizon-expired queued claim must not dispatch, got %d", len(out))
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if !got.Status.Review.Released || got.Status.Review.ReleaseReason != v1alpha1.ReleaseReasonHorizon {
		t.Fatalf("aged queued claim must release as horizon, got released=%v reason=%q",
			got.Status.Review.Released, got.Status.Review.ReleaseReason)
	}
	if got.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("a CI-wait expiry is not a dispatch failure — churn counter must stay 0, got %d",
			got.Status.Review.DispatchLostReleases)
	}
}

// ── r8: refusal ATTRIBUTION + budget reset, both driven through the gate. ──

// TestSweepRefusalAttribution_ChurnRefusalReportsBudget drives the
// never-dispatched convergence THROUGH RunReviewGateSweep (the r6-era test
// called ArmClaim directly, which is why the guard's attribution was never
// observed at the boundary): an exhausted counter must surface as the
// CHURN BUDGET's standdown ("re-apply the label..."), never as the
// dead-dispatch breaker nor as the horizon dismissal — the three refusal
// classes are distinct failures to an operator, in the timeline and in
// harmostes_review_gate_total (r7 P4.2/P7, r12 must-fix 2).
func TestSweepRefusalAttribution_ChurnRefusalReportsBudget(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 104)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	// The fixture head MUST equal the served PR head (greenPullBody's
	// deadbeef123) — the sweep evaluates the candidate at the PR's head,
	// and the pointer-local guard only fires on the matching attempt.
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#104", "deadbeef123", time.Now().Add(-time.Hour), nil)
	claim.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st, claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("exhausted counter must refuse the dispatch, got %d", len(out))
	}
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastDecision != "standdown" {
		t.Fatalf("aggregates must surface the standdown, got %+v", st.last.ReviewReady)
	}
	reason := st.last.ReviewReady.LastReason
	if !strings.Contains(reason, "re-apply the label to request a fresh review") {
		t.Fatalf("churn refusal must carry the override text, got %q", reason)
	}
	if strings.Contains(reason, "dead-dispatch breaker") {
		t.Fatalf("never-dispatched churn must NOT be reported as the dead-dispatch breaker, got %q", reason)
	}
	if strings.Contains(reason, "recently dismissed by horizon") {
		t.Fatalf("never-dispatched churn must NOT be reported as the horizon dismissal, got %q", reason)
	}
	if !strings.Contains(reason, "consecutive never-dispatched releases") {
		t.Fatalf("churn refusal must carry the BUDGET cause, got %q", reason)
	}
}

// TestRunGate_DispatchClearsChurnBudget pins the field's contract at the
// boundary the cycle actually runs (r7 P4.3/P8: the reset lived only in
// MarkClaimDispatched and no test observed it — deleting the line left the
// suite green). Dispatch success clears the never-dispatched budget, so a
// single later release starts from 1 and the head is NOT refused.
func TestRunGate_DispatchClearsChurnBudget(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 105)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	// The churn cycle's actual shape: the era was RELEASED dispatch-lost
	// (counter at Max-1), and this sweep's drain REVIVES it — the revival
	// arms, the dispatcher marks dispatched, and THAT must clear the
	// budget. Head matches the served PR (greenPullBody's deadbeef123).
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#105", "deadbeef123", time.Now().Add(-time.Hour), nil)
	claim.Status.Review.Released = true
	claim.Status.Review.ReleaseReason = v1alpha1.ReleaseReasonDispatchLost
	claim.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases - 1
	claim.Labels[v1alpha1.ReviewClaimLabel] = v1alpha1.ReviewClaimReleased
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st, claim)

	const pr = "git.rezus.cloud/tibrez/rhesadox#105"
	const sha = "deadbeef123"

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil || len(out) != 1 {
		t.Fatalf("counter under max must dispatch, got %d dispatches err=%v", len(out), err)
	}
	// The dispatcher's contract: a created job marks the claim dispatched.
	if err := attempt.MarkClaimDispatched(ctx, deps.Client, wf.Namespace, out[0].Attempt); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("dispatch success must clear the budget (was %d), got %d", v1alpha1.MaxDispatchLostReleases-1, got.Status.Review.DispatchLostReleases)
	}
	if got.Status.Review.Released {
		t.Fatal("the revival must have committed Released=false")
	}
	if _, marked := got.Labels[v1alpha1.ReviewClaimLabel]; marked {
		t.Fatal("the revival must have removed the released marker (absence = live)")
	}

	// One later never-dispatched release starts from 1 (not 3) — the head
	// is still revivable. This is the assertion the reset-line deletion
	// flips (mutation probe).
	if err := attempt.ReleaseClaim(ctx, deps.Client, wf.Namespace, claim.Name, v1alpha1.ReleaseReasonDispatchLost); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Review.DispatchLostReleases != 1 {
		t.Fatalf("one release after a successful dispatch must count 1, got %d", got.Status.Review.DispatchLostReleases)
	}
	if _, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf, pr, sha, "needs-review", false); err != nil {
		t.Fatalf("one churned release after a dispatch must NOT refuse the auto re-arm: %v", err)
	}
}

// withManualMeter / collectMetrics mirror internal/controller/telemetry_test.go
// (package-private there — the 18 lines are cheaper than an exported testutil
// for two packages; revisit if a third package needs the pattern).
func withManualMeter(t *testing.T) (*sdkmetric.ManualReader, func() metricdata.ResourceMetrics) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background())
	})
	return reader, func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		return rm
	}
}

// TestSweepHeldReasonSurvivesLaterRefusal (r8 F2): pass A's hold — "a live
// Job is observably alive past its DispatchTimeout" (#331) — is a
// durability promise about a run IN FLIGHT. A second candidate whose arm
// the churn guard refuses later in the same sweep must not clobber it:
// the aggregates must still carry the held reason. This is the test the
// r8 HasPrefix form was written for and could never pass (the marker is a
// suffix; the flag now tracks it).
func TestSweepHeldReasonSurvivesLaterRefusal(t *testing.T) {
	clearTriggerEnv(t)
	// Combined server: the labeled scan lists ONLY PR 106 (the churned
	// candidate); PR 101 (the held claim) fetches as open, green, and
	// VERDICT-LESS — the hold's Evaluate must see no verdict to classify
	// dispatch-timeout. labeledListServer cannot serve that shape (its
	// /comments 404s), so this test carries its own handler.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 106, "updated_at": "2026-08-30T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]any{})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(greenPullBody())
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-46 * time.Minute) // past DispatchTimeout (45m)

	// Claim A: dispatched, past the timeout, Job observably ALIVE → held.
	held := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#101", "deadbeef321", now.Add(-46*time.Minute), &disp)
	held.Status.Phase = v1alpha1.AttemptPhaseReconciling
	liveJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-job-alive", Namespace: wf.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "harmostes",
				"harmostes.dev/workflow": wf.Name,
				v1alpha1.AttemptLabel:    held.Name,
			},
		},
	}

	// Claim B: churned out — its automatic arm must be refused in the drain.
	refused := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#106", "deadbeef123", now.Add(-time.Hour), nil)
	refused.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases

	_, collect := withManualMeter(t)
	deps, ctx := gateEnv(t, wf, st, held, liveJob, refused)

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if st.last.ReviewReady == nil {
		t.Fatal("no aggregates recorded")
	}
	if !strings.Contains(st.last.ReviewReady.LastReason, "held") {
		t.Fatalf("the held durability promise must survive a later candidate's refusal, got %q", st.last.ReviewReady.LastReason)
	}

	// PREFERENCE, not a mute (r10): the preserved headline must not
	// swallow the refusal — the turned-away candidate is still counted.
	rm := collect()
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "harmostes_review_gate_total" {
				continue
			}
			found = true
			dp, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected datapoint type %T", m.Data)
			}
			total := int64(0)
			budget := int64(0)
			for _, point := range dp.DataPoints {
				total += point.Value
				for _, kv := range point.Attributes.ToSlice() {
					if kv.Key == attribute.Key("reason") && kv.Value.Emit() == "budget" {
						budget += point.Value
					}
				}
			}
			// Claim B's refusal is the BUDGET class (exhausted dispatch-lost
			// counter) — the r12 sentinel split means it must count under
			// reason="budget", NOT "dismissed" (that is the horizon's).
			if total == 0 || budget == 0 {
				t.Fatalf("the refusal must be counted (reason=budget), got total=%d budget=%d", total, budget)
			}
		}
	}
	if !found {
		t.Fatal("harmostes_review_gate_total was never incremented — the refusal went unrecorded")
	}
}

// TestSweepNeverDispatchedPassSparesLiveJob (r9 (b)): DispatchedAt == nil
// means "nobody ran MarkClaimDispatched", NOT "no Job" — the dispatcher can
// Create the Job and fail the mark, and the live-Job dedupe continues
// before the mark. Pass C (the never-dispatched release) is the only pass
// that never consulted jobAlive; before the fix it spent the churn budget
// on a RUNNING review (probe-verified upstream: counter 1→2 while the Job
// lived). The budget must only be spent on claims with no Job at all.
func TestSweepNeverDispatchedPassSparesLiveJob(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()

	// Armed past reDispatchGrace, never marked dispatched, budget already
	// warm — and a LIVE Job carrying the attempt label.
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#107", "deadbeef123", now.Add(-10*time.Minute), nil)
	claim.Status.Review.DispatchLostReleases = 1
	liveJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-job-running-unmarked", Namespace: wf.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "harmostes",
				"harmostes.dev/workflow": wf.Name,
				v1alpha1.AttemptLabel:    claim.Name,
			},
		},
	}
	deps, ctx := gateEnv(t, wf, st, claim, liveJob)

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Review.Released {
		t.Fatal("a claim whose review Job is RUNNING must not be released as never-dispatched")
	}
	if gotCount := got.Status.Review.DispatchLostReleases; gotCount != 1 {
		t.Fatalf("the churn budget must not be spent on a live run: counter = %d, want 1", gotCount)
	}
}

// TestSweepNeverDispatchedPassFailsClosedOnJobListError (r11 must-fix 3):
// an empty Job snapshot on a list error means "we could not tell", not "no
// Job" — and on the Create-succeeded-mark-failed case the claim's Job is
// RUNNING. Releasing it there is the r9 (b) bug class on the error path;
// passes A/B fail closed on this exact signal and pass C must too.
func TestSweepNeverDispatchedPassFailsClosedOnJobListError(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()

	// Armed far past reDispatchGrace, never marked dispatched: pass C's
	// exact release shape — if it consulted the (broken) Job snapshot, it
	// would see "no Job" and release. The head MATCHES the served PR
	// (r30): a moved-head queued claim is section A's superseded-release
	// class — a liveness-independent head-currency decision — and would
	// bypass the fail-closed contract this test pins.
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#108", "deadbeef123", now.Add(-10*time.Minute), nil)
	claim.Status.Review.DispatchLostReleases = 1

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
	ctx := t.Context()

	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	var got v1alpha1.Attempt
	if err := cl.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Review.Released {
		t.Fatal("liveness-unknown must fail closed: pass C must not release on a Job-list error")
	}
	if gotCount := got.Status.Review.DispatchLostReleases; gotCount != 1 {
		t.Fatalf("the churn budget must not be spent on liveness-unknown: counter = %d, want 1", gotCount)
	}
}

// TestGateSweepDeadlineInsideReDispatchGrace pins the ordering invariant the
// comments on both constants describe (r11 pillar 1): the sweep deadline plus
// an arm's worst case must stay comfortably inside reDispatchGrace, or an
// aborted sweep strands armed claims the NEXT sweep eats before their
// dispatch loop can speak — retuning either constant alone re-opens #343.
// Three lines here do what two packages of prose cannot: fail CI on a
// retune that violates it.
func TestGateSweepDeadlineInsideReDispatchGrace(t *testing.T) {
	if gateSweepDeadline >= reDispatchGrace {
		t.Fatalf("gateSweepDeadline (%v) must stay strictly below reDispatchGrace (%v): an aborted sweep must leave armed claims room for the next sweep's dispatch loop", gateSweepDeadline, reDispatchGrace)
	}
	if reDispatchGrace < 2*gateSweepDeadline {
		t.Fatalf("reDispatchGrace (%v) should be at least 2x gateSweepDeadline (%v): the sweep is not the only consumer of the grace window (arm + dispatch are)", reDispatchGrace, gateSweepDeadline)
	}
}

// TestSweepAbortSpeaksInTheAggregates (r11 pillar 7): an aborted sweep must
// not leave the sweep-start default "nothing to evaluate" as the reason a
// human reads first — the round the operator most needs the cause is the
// round it was being erased. The abort keeps its counter (sweep-abort) AND
// lands in LastReason.
func TestSweepAbortSpeaksInTheAggregates(t *testing.T) {
	clearTriggerEnv(t)
	srv := noVerdictServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()

	// Armed far past the grace window: pass C would release this claim if
	// the sweep got that far — it must not, and the status must say why.
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#109", "deadbeef789", now.Add(-10*time.Minute), nil)
	deps, ctx := gateEnv(t, wf, st, claim)

	deadlineCtx, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // let the deadline actually fire
	// The sweep handles its own abort: it returns nil (the abort is logged
	// and counted, not an error to the caller) — the assertions below pin
	// what must still be TRUE after it: aggregates written, cause recorded,
	// no releases.
	RunReviewGateSweep(deadlineCtx, deps, wf)

	if st.last.ReviewReady == nil {
		t.Fatal("no aggregates recorded — durable records must survive the abort")
	}
	if !strings.Contains(st.last.ReviewReady.LastReason, "sweep aborted") {
		t.Fatalf("an aborted sweep must record its cause in the aggregates, got %q", st.last.ReviewReady.LastReason)
	}
	var got v1alpha1.Attempt
	if err := deps.Client.Get(context.Background(), client.ObjectKey{Namespace: wf.Namespace, Name: claim.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Review.Released {
		t.Fatal("an aborted sweep must not release claims")
	}
}

// TestRunGate_StaleAnnotationsDoNotOverride (issue #349): the breaker's
// human override must engage on the EVENT (GateDeps.Wake* from the
// RunRequest), never on scraped state. The controller stamps
// harmostes.dev/trigger-* annotations before publishing and CLEARS them at
// schedule time; a stale pair left on the fetched CR (failed clear, manual
// kubectl edit, pre-upgrade object) used to wake the gate as a HUMAN
// request — an automatic override with no human in the loop. The event
// threading replaced the scrape; this pins that the scrape is gone: stale
// annotations plus NO event must leave the breaker closed.
func TestRunGate_StaleAnnotationsDoNotOverride(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	// The stale state: annotations still on the CR, event never delivered.
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":       "git.rezus.cloud/tibrez/rhesadox#99",
		"harmostes.dev/trigger-action":   "labeled",
		"harmostes.dev/trigger-revision": "deadbeef123",
	}
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st) // no wake: the pool got no event
	for i := 0; i < v1alpha1.MaxDeadDispatchesPerHead; i++ {
		at, err := attempt.ArmClaim(ctx, deps.Client, deps.Scheme, wf,
			"git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", "needs-review", false)
		if err != nil {
			t.Fatalf("arm %d: %v", i+1, err)
		}
		_ = attempt.MarkClaimDispatched(ctx, deps.Client, wf.Namespace, at.Name)
		_, _, _ = attempt.ReleaseClaimDead(ctx, deps.Client, wf.Namespace, at.Name, "dispatch-lost")
	}

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("stale annotations must NOT engage the human override, got %d dispatches", len(out))
	}
	reason := st.last.ReviewReady.LastReason
	if !strings.Contains(reason, "dead-dispatch breaker") {
		t.Fatalf("the refusal must still be the breaker's, got %q", reason)
	}
}

// TestGateWakeActionVocabulary (#357 Judge): the action mapping moved from
// parseWake into GateDeps.wake — pin the WHOLE vocabulary, not just
// "labeled". Request-shaped (label touched) wakes may supersede a live
// claim; of those, only the label APPLYING is the breaker's human override.
// Push-shaped actions arm nothing into a live claim (review_gate.go's
// !cand.request continue) and are not human arms.
func TestGateWakeActionVocabulary(t *testing.T) {
	wf := gateWorkflow()
	cases := []struct {
		action      string
		wantRequest bool
		wantLabeled bool
	}{
		{"labeled", true, true},
		// A labeled wake WITHOUT a revision cannot name the head the human
		// re-labeled ("" != HeadSHA reads as a moved head and supersedes a
		// dispatched claim on no evidence) — the override is refused
		// (r16 pillar 4). It is still the label being applied (labeled).
		{"labeled-without-revision", false, true},
		{"unlabeled", true, false},
		{"label_updated", true, false},
		{"synchronize", false, false},
		{"opened", false, false},
		{"reopened", false, false},
		{"closed", false, false},
		{"ready_for_review", false, false},
	}
	for _, tc := range cases {
		rev := "deadbeef123"
		action := tc.action
		if action == "labeled-without-revision" {
			action, rev = "labeled", ""
		}
		deps := GateDeps{Log: t.Logf, Wake: GateWake{
			PR: "git.rezus.cloud/tibrez/rhesadox#99", Action: action, Revision: rev,
		}}
		c := deps.wake(wf)
		if c == nil {
			t.Fatalf("%s: wake must materialize (candidate build is action-independent)", tc.action)
		}
		if c.request != tc.wantRequest || c.labeled != tc.wantLabeled {
			t.Fatalf("%s: want request=%v labeled=%v, got request=%v labeled=%v",
				tc.action, tc.wantRequest, tc.wantLabeled, c.request, c.labeled)
		}
	}
	// And the dead-dispatch-counter distinction this encodes (#328): the
	// counter resets only through the human arm (labeled), so an unlabeled
	// wake riding a live claim may supersede NOTHING and reset NOTHING —
	// covered behaviorally by the !cand.request continue and ArmClaim's
	// humanRequest gate; the table above is the seam's contract.
}

// TestRunGate_RevisionlessWakeCannotSupersedeDispatched (r17 must-fix — the
// behavioral pin the vocabulary table cannot be): a labeled wake carrying NO
// revision names no head; "" != HeadSHA reads as "the head moved", so the
// drain would release a live, DISPATCHED review and burn its slot on no
// evidence. The wake must arm nothing and leave the claim untouched.
// Mutation-verified: dropping the Revision leg of requestShaped turns this
// red (the claim is released and re-armed).
func TestRunGate_RevisionlessWakeCannotSupersedeDispatched(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	disp := now.Add(-5 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-30*time.Minute), &disp)
	claim.Status.Phase = v1alpha1.AttemptPhaseReconciling
	// An observably ALIVE Job: pass C must HOLD the dispatched claim (the
	// thing under test is the drain's supersede, not a dispatch-lost release).
	liveJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-job-alive-revless", Namespace: wf.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "harmostes",
				"harmostes.dev/workflow": wf.Name,
				v1alpha1.AttemptLabel:    claim.Name,
			},
		},
	}
	deps, ctx := gateEnv(t, wf, st, claim, liveJob)
	// The wake IS delivered (labeled) but carries NO revision: the head the
	// human re-labeled is unnameable, so the override must not engage.
	deps.Wake = GateWake{PR: "git.rezus.cloud/tibrez/rhesadox#99", Action: "labeled", Revision: ""}

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a revisionless labeled wake must not dispatch, got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
	if err != nil || len(claims) != 1 {
		t.Fatalf("the dispatched claim must stay live, got %d (%v)", len(claims), err)
	}
	if claims[0].Name != claim.Name || claims[0].Status.Review.Released {
		t.Fatalf("the dispatched claim must be untouched: %v", claims[0].Status.Review)
	}
}

// TestMultiArmHostilePrefixWakeIgnored (r18 nit — the security row): a wake
// pointer with a hostile host PREFIX must be rejected by repoInScope's
// EXACT match. If a future refactor loosens the match (suffix/prefix
// contains), this row goes red before the gate arms against evil.com.
func TestMultiArmHostilePrefixWakeIgnored(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnvW(t, wf, st, "evil.com/github.com/tibrezus/harmostes#99", "labeled", "deadbeef123")

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("hostile host prefix must not arm, got %d dispatches", len(out))
	}
}

// ── r30 F1: a POLL sweep (no wake) whose queued claim armed at an older
// head must release it SUPERSEDED and re-arm+dispatch at the new head in
// the same sweep — holding the stale-head claim strands the PR (the
// strand the moved-head branch's old comment promised a wake would fix). ──
func TestMultiArmMovedHeadPollReleasesSupersededAndReArms(t *testing.T) {
	clearTriggerEnv(t)
	srv := labeledListServer(t, 99) // PR 99 green at deadbeef123
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "oldhead000", armed, nil)
	deps, ctx := gateEnv(t, wf, st, claim) // poll sweep: no wake

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("moved-head queued claim must release and re-dispatch at the new head, got %d dispatches", len(out))
	}
	if out[0].Attempt == claim.Name || out[0].Envelope.HeadSHA != "deadbeef123" {
		t.Fatalf("dispatch must be the NEW-head claim, got attempt=%s head=%s", out[0].Attempt, out[0].Envelope.HeadSHA)
	}
	var re v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, &re); err != nil {
		t.Fatalf("re-list stale claim: %v", err)
	}
	if !re.Status.Review.Released || re.Status.Review.ReleaseReason != "superseded" {
		t.Fatalf("stale-head claim must be released superseded, got released=%t reason=%q",
			re.Status.Review.Released, re.Status.Review.ReleaseReason)
	}
	if re.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("superseded release must not burn the churn budget, got %d strikes", re.Status.Review.DispatchLostReleases)
	}
}

// ── r30 F2: a request-shaped wake on a queued claim's pointer escapes
// section A — the SAME sweep's never-dispatched release pass must not eat
// the claim when C declines to decide (same head, no override). An aged
// claim is the sharp case: only the keepArmed shield saves it. The release
// write is observed directly: a same-head wake REVIVES a released claim
// (clearing Released and resetting the counter), so final state cannot
// testify — the interceptor's count can. ──
func TestMultiArmWakeEscapeShieldsQueuedClaimFromReleasePass(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	lostWrites := 0
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		WithRuntimeObjects(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if at, ok := obj.(*v1alpha1.Attempt); ok && at.Status.Review != nil &&
					at.Status.Review.Released && at.Status.Review.ReleaseReason == v1alpha1.ReleaseReasonDispatchLost {
					lostWrites++
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf}
	deps.Wake = GateWake{PR: "git.rezus.cloud/tibrez/rhesadox#99", Action: "labeled", Revision: "deadbeef123"}
	ctx := context.Background()

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	_ = out // C may skip (same head, no override) — the finding is the release pass
	var re v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, &re); err != nil {
		t.Fatalf("re-list claim: %v", err)
	}
	if re.Status.Review.Released {
		t.Fatalf("wake-escape must shield the queued claim, got released reason=%q", re.Status.Review.ReleaseReason)
	}
	if lostWrites != 0 {
		t.Fatalf("the release pass must never write a dispatch-lost release for the shielded claim, saw %d", lostWrites)
	}
}

// ── r30 F3: the churn-guard standdown must not be contradicted one pass
// later — the never-dispatched release pass must not re-release the refused
// claim and bump the counter PAST Max on every sweep. ──
func TestMultiArmChurnGuardStanddownNotDoubleReleased(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-6 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	claim.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases
	deps, ctx := gateEnv(t, wf, st, claim) // poll sweep: no wake

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("churn-refused claim must not dispatch, got %d", len(out))
	}
	if st.last.ReviewReady == nil || !strings.Contains(st.last.ReviewReady.LastReason, "consecutive never-dispatched releases") {
		t.Fatalf("aggregates must record the churn refusal, got %+v", st.last.ReviewReady)
	}
	var re v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, &re); err != nil {
		t.Fatalf("re-list claim: %v", err)
	}
	if re.Status.Review.Released {
		t.Fatalf("the refused claim must stay unreleased (no double release), got reason=%q", re.Status.Review.ReleaseReason)
	}
	if re.Status.Review.DispatchLostReleases != v1alpha1.MaxDispatchLostReleases {
		t.Fatalf("the refusal must not increment the budget it refuses to spend, got %d", re.Status.Review.DispatchLostReleases)
	}
}

// ── blockingTL: a sidecar that accepts the connection and never answers —
// the #379 hazard. Bounded by tlWriteTimeout, non-fatal to the sweep. ──
type blockingTL struct{ block chan struct{} }

func (b *blockingTL) Emit(ctx context.Context, kind, node string, payload any) error {
	select {
	case <-b.block: // released only by the test
	case <-ctx.Done(): // a real dapr client honors ctx — the bound relies on it
	}
	return ctx.Err()
}

// ── r30 #379 acceptance: a hung TL write must not own the sweep — the
// dispatch still lands. ──
func TestSweepTLWriteBoundedNonFatal(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	now := time.Now()
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", now.Add(-2*time.Minute), nil)
	deps, ctx := gateEnv(t, wf, st, claim)
	tl := &blockingTL{block: make(chan struct{})}
	deps.TL = tl
	prev := tlWriteTimeout
	tlWriteTimeout = 30 * time.Millisecond
	t.Cleanup(func() { tlWriteTimeout = prev; close(tl.block) })

	start := time.Now()
	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a hung timeline write must not eat the dispatch, got %d", len(out))
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("the bounded write must cap the hang at tlWriteTimeout, sweep took %s", took)
	}
}

// ── r30 #379 acceptance: a sweep that dies mid-flight records the abort
// (status stamp), and the strand it left behind releases as sweep-aborted —
// NO churn strike — on the next healthy sweep. ──
func TestSweepAbortRecordedNoChurnStrike(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-30 * time.Minute) // far past reDispatchGrace
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("batchv1: %v", err)
	}
	var killCurrent context.CancelFunc
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		WithRuntimeObjects(claim).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				err := c.List(ctx, list, opts...)
				if killCurrent != nil {
					killCurrent() // the sweep ctx dies right after the claims list
					killCurrent = nil
				}
				return err
			},
		}).
		Build()
	deps := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf}

	// Sweep 1: the abort. The claim list succeeds; everything after runs on
	// a dead ctx — the pass must skip and the abort must be STAMPED.
	ctx1, cancel1 := context.WithCancel(context.Background())
	killCurrent = cancel1
	if _, err := RunReviewGateSweep(ctx1, deps, wf); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	cancel1()
	if st.last.ReviewReady == nil || st.last.ReviewReady.LastSweepAbortAt == nil {
		t.Fatalf("the abort must be stamped in status, got %+v", st.last.ReviewReady)
	}
	var re v1alpha1.Attempt
	if err := deps.Client.Get(context.Background(), client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, &re); err != nil {
		t.Fatal(err)
	}
	if re.Status.Review.Released {
		t.Fatal("an aborted sweep must not release the claim itself (fail closed)")
	}

	// Sweep 2: healthy. The designed recovery is STRONGER than a release:
	// section A re-evaluates the strand and dispatches the EXISTING claim
	// (r27 backfill). The sweep-aborted release below the pass is the
	// floor for sweeps that CANNOT re-evaluate — either way, no churn
	// strike may land on an abort the operator already owns.
	deps2 := GateDeps{Status: st, Client: cl, Scheme: scheme, FleetMaxConcurrent: 3, Log: t.Logf}
	out2, err := RunReviewGateSweep(context.Background(), deps2, wf)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if err := deps.Client.Get(context.Background(), client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, &re); err != nil {
		t.Fatal(err)
	}
	if len(out2) == 1 && out2[0].Attempt == claim.Name {
		// backfilled by dispatch — the good path
		if re.Status.Review.DispatchLostReleases != 0 {
			t.Fatalf("the backfill must not burn the churn budget, got %d", re.Status.Review.DispatchLostReleases)
		}
		return
	}
	// floor: released as sweep-aborted, still no strike
	if !re.Status.Review.Released || re.Status.Review.ReleaseReason != "sweep-aborted" {
		t.Fatalf("the strand must be backfilled by dispatch (got %+v) or released sweep-aborted (got released=%t reason=%q)",
			out2, re.Status.Review.Released, re.Status.Review.ReleaseReason)
	}
	if re.Status.Review.DispatchLostReleases != 0 {
		t.Fatalf("an aborted-sweep stranding must not burn the churn budget, got %d", re.Status.Review.DispatchLostReleases)
	}
}

// ── r30 #379 acceptance: wake backfill — a request-shaped wake shields the
// stranded claim, and the NEXT (poll) sweep dispatches the EXISTING claim. ──
func TestWakeBackfillDispatchesStrandedClaim(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	armed := time.Now().Add(-30 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	deps, ctx := gateEnvW(t, wf, st, "git.rezus.cloud/tibrez/rhesadox#99", "labeled", "deadbeef123", claim)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake sweep: %v", err)
	}
	for _, g := range out {
		if g.Attempt == claim.Name {
			t.Fatalf("the wake sweep must not dispatch the shielded claim itself, got %+v", out)
		}
	}

	deps.Wake = GateWake{} // the label event is consumed — the next sweep polls
	out, err = RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("poll sweep: %v", err)
	}
	if len(out) != 1 || out[0].Attempt != claim.Name {
		t.Fatalf("the poll sweep must backfill-dispatch the stranded claim, got %+v (want attempt %s)", out, claim.Name)
	}
}

// ── r30 #376: the churn budget is a WINDOW, not a life sentence — strikes
// older than the horizon self-clear on the next automatic arm and the PR
// dispatches (the webhook-less forge's operator exit). ──
func TestChurnBudgetSelfClearsAfterHorizon(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow() // horizon 6h
	st := &fakeStatus{}
	armed := time.Now().Add(-30 * time.Minute)
	claim := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#99", "deadbeef123", armed, nil)
	claim.Status.Review.DispatchLostReleases = v1alpha1.MaxDispatchLostReleases
	stale := metav1.NewTime(time.Now().Add(-7 * time.Hour))
	claim.Status.Review.LastDispatchLostAt = &stale
	deps, ctx := gateEnv(t, wf, st, claim) // automatic sweep: no human request

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("stale-exhausted budget must self-clear and dispatch, got %d dispatches (aggregates %+v)", len(out), st.last.ReviewReady)
	}
}
