package gate

// Investigation + regression tests for tibrezus/harmostes#646 and its
// scoped residue #648 (gate-local refusal-notice dedupe).
//
// T1/T2/T4/T5 pin the #646 record (what is already state-based, bounded,
// or by-design); the TestIssue648* tests are the regression coverage for
// the host-side notice dedupe.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/attempt"
)

// greenForgjoServer serves a labeled, open, all-green PR at deadbeef123
// (Forgejo dialect) with a clean conversation — the "already green at arm
// time" world of the issue's evidence 1.
func greenForgejoServer646(t *testing.T, conversation []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 2621, "updated_at": "2026-10-04T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.Contains(req.URL.Path, "/comments"):
			if conversation != nil {
				_ = json.NewEncoder(w).Encode(conversation)
				return
			}
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, req)
		}
	}))
}

// T1 — issue claim 1 (primary): "a pr-review claim armed while the head's CI
// is already green never dispatches".
//
// Desired semantics (issue request 1): state-based readiness — an
// armed-queued claim at an unchanged, green head dispatches on the next
// sweep without needing a green EVENT after the arm.
//
// EXPECTED after the fix: dispatch. (Prediction at HEAD: PASSES — the
// armed-queued re-evaluation (#379/#381) + CI-wake convergence (#556/#557)
// already make the sweep's re-dispatch a state test, not an event test.)
func TestIssue646ArmedQueuedGreenHeadDispatches(t *testing.T) {
	srv := greenForgejoServer646(t, nil)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// The strand shape: armed an hour ago (well past reDispatchGrace) while
	// CI was pending; CI has since gone green at the SAME head; no CI wake
	// ever arrived (a poll sweep is the only evaluator).
	at := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#2621", "deadbeef123",
		time.Now().Add(-time.Hour), nil)
	deps, ctx := gateEnv(t, wf, st, at)
	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("issue #646 primary: queued claim at a green head must dispatch on a state check, got %d dispatch(es)", len(out))
	}
	if out[0].Attempt != at.Name {
		t.Fatalf("dispatch must complete the EXISTING claim %s, got %s", at.Name, out[0].Attempt)
	}
}

// T2 — issue claim 4 ("consumed claims linger"): the review Job COMPLETED and
// the verdict trailer was posted, but the claim is still live. Desired
// semantics (issue request 2): the claim is consumed — released as
// "consumed", NOT counted as a dead dispatch — as soon as any sweep observes
// the state.
//
// EXPECTED: release reason "consumed", DeadDispatches == 0.
// (Prediction at HEAD: PASSES via section A's in-flight verdict scan; the
// residual gap is TRIGGER-side — nothing schedules a sweep for a workflow
// whose claims are all dispatched except the controller's 5m poll.)
func TestIssue646ConsumedClaimNotDispatchLost(t *testing.T) {
	verdict := []map[string]string{
		{"body": "verdict\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->", "created_at": "2026-10-04T00:20:00Z"},
	}
	srv := greenForgejoServer646(t, verdict)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// Dispatched 30m ago (past jobDeathGrace), no live Job (the fixture
	// adds none), verdict posted since the arm.
	dispatched := time.Now().Add(-30 * time.Minute)
	at := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#2621", "deadbeef123",
		dispatched.Add(-time.Hour), &dispatched)
	deps, ctx := gateEnv(t, wf, st, at)
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
	if err != nil {
		t.Fatalf("live claims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("issue #646 (4): consumed claim must be released, still live: %d", len(claims))
	}
	var rel v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: at.Name}, &rel); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if r := rel.Status.Review; r == nil || r.ReleaseReason != "consumed" {
		t.Fatalf("issue #646 (4): a posted verdict must release the claim as consumed, got %+v", rel.Status.Review)
	}
	if rel.Status.Review.DeadDispatches != 0 {
		t.Fatalf("a consumed claim is not a dead dispatch — breaker struck %d time(s)", rel.Status.Review.DeadDispatches)
	}
}

// T3 — issue #648 (from #646 point 5): the refusal notice dedupe must hold
// when the MEMO is lost — the live shape was two overlapping sweeps 9s
// apart (two identical comments 51412/51414); a second worker pod, or the
// construct-and-replace status patch clobbering the first sweep's
// Refusals, both present as "sweep 2 with an empty memo". The HOST
// conversation is the shared state: sweep 1's posted notice must stop
// sweep 2's post. EXPECTED after the fix: exactly 1 PostComment, and sweep
// 2 re-records the refusal as HostNotified.
func TestIssue648NoticeDedupesOnHostStateAfterMemoLoss(t *testing.T) {
	// A REAL host store: GET returns everything ever POSTed — the property
	// the fix leans on (both sweeps see the same conversation).
	var mu sync.Mutex
	var conversation []map[string]string
	posts := 0
	verdict := []map[string]string{
		{"body": "verdict\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->", "created_at": "2026-10-04T00:38:00Z"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 2621, "updated_at": "2026-10-04T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/comments"):
			var posted struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(req.Body).Decode(&posted)
			mu.Lock()
			posts++
			conversation = append(conversation, map[string]string{"body": posted.Body, "created_at": "2026-10-04T00:38:10Z"})
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.Contains(req.URL.Path, "/comments"):
			mu.Lock()
			defer mu.Unlock()
			all := append([]map[string]string{}, verdict...)
			all = append(all, conversation...)
			_ = json.NewEncoder(w).Encode(all)
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()

	run := func() *fakeStatus {
		st := &fakeStatus{} // fresh memo each sweep: the pod-split / clobbered patch shape
		deps, ctx := gateEnv(t, wf, st)
		if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return st
	}
	st1 := run()
	st2 := run()
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("issue #648: the host conversation must dedupe the notice across memo loss, got %d post(s)", posts)
	}
	if !st1.last.ReviewReady.Refusals[0].HostNotified {
		t.Fatal("sweep 1 posted — its refusal must carry HostNotified")
	}
	if !st2.last.ReviewReady.Refusals[0].HostNotified {
		t.Fatal("sweep 2 skipped via the host scan — its re-recorded refusal must be marked HostNotified")
	}
	if !strings.Contains(conversation[0]["body"], "<!-- harmostes: review-gate-refusal @ deadbeef123 -->") {
		t.Fatalf("the posted notice must carry the machine-readable marker, got %q", conversation[0]["body"])
	}
}

// T3b — legacy notices (pre-marker prose, the production shape from the
// incident) dedupe too: the body prefix is stable since #577 and carries
// the full head verbatim.
func TestIssue648LegacyNoticeDedupes(t *testing.T) {
	conversation := []map[string]string{
		{"body": "Review gate (#567): this head (`deadbeef1234567890deadbeef1234567890abcd`) was already reviewed — no new review runs at the same SHA.  verdict standing", "created_at": "2026-10-04T00:38:10Z"},
		{"body": "verdict\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->", "created_at": "2026-10-04T00:38:00Z"},
	}
	posts := 0
	srv := noticeStubServer648(t, conversation, &posts)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if posts != 0 {
		t.Fatalf("a legacy (pre-marker) notice at this head must dedupe the post, got %d", posts)
	}
	if !st.last.ReviewReady.Refusals[0].HostNotified {
		t.Fatal("the host-side dedupe must mark the refusal HostNotified")
	}
}

// T3c — a notice standing at a DIFFERENT head does not cover this head: a
// new head earns a fresh notice.
func TestIssue648NoticeAtOtherHeadPosts(t *testing.T) {
	conversation := []map[string]string{
		{"body": "Review gate (#567): this head (`1234567890abcdef1234567890abcdef12345678`) was already reviewed <!-- harmostes: review-gate-refusal @ 1234567890abcdef1234567890abcdef12345678 -->", "created_at": "2026-10-04T00:38:10Z"},
		{"body": "verdict\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->", "created_at": "2026-10-04T00:38:00Z"},
	}
	posts := 0
	srv := noticeStubServer648(t, conversation, &posts)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if posts != 1 {
		t.Fatalf("a notice at another head must not cover this head — one fresh notice expected, got %d", posts)
	}
}

// T3d — inconclusive dedupe scan (conversation exceeds the cap): the
// refusal is already memoised (this is the memo-skip path — the evaluation
// itself never runs), the scan cannot see the newest end, so the post
// goes out: visibility wins (#577), the degraded dedupe is logged.
func TestIssue648InconclusiveScanPostsAnyway(t *testing.T) {
	page := make([]map[string]string, 100)
	for i := range page {
		page[i] = map[string]string{"body": "chat", "created_at": "2026-09-01T00:00:00Z"}
	}
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 2621, "updated_at": "2026-10-04T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/comments"):
			posts++
			w.WriteHeader(http.StatusOK)
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(page) // always a full page → truncated at the cap
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// The memoised refusal from a prior sweep (HostNotified false — the
	// clobbered/lost shape) drives the memo-skip path.
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{
		LastDecision: "standdown",
		Refusals: []v1alpha1.ReviewRefusal{{
			Repo: "git.rezus.cloud/tibrez/rhesadox", PR: 2621,
			HeadSHA: "deadbeef123", Reason: "verdict standing",
		}},
	}
	deps, ctx := gateEnv(t, wf, st)
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if posts != 1 {
		t.Fatalf("an inconclusive dedupe scan must post (visibility wins), got %d", posts)
	}
}

// noticeStubServer648: a static conversation store + counted posts — the
// shared shape for the dedupe-table tests above.
func noticeStubServer648(t *testing.T, conversation []map[string]string, posts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 2621, "updated_at": "2026-10-04T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/comments"):
			*posts++
			w.WriteHeader(http.StatusOK)
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(conversation)
		default:
			http.NotFound(w, req)
		}
	}))
}

// T4 — a strand mechanism candidate: the standing-verdict scan goes
// INCONCLUSIVE (conversation exceeds maxCommentPages) → Evaluate returns
// waiting forever → the armed-queued claim is shielded (keepArmed) from the
// never-dispatched release and can neither dispatch nor release until the 6h
// horizon. On a busy PR this IS a same-head strand with only the horizon as
// escape.
//
// EXPECTED (if this is to be the accepted trade): documented + bounded —
// assert the claim neither dispatches NOR releases within the horizon, and
// name the horizon as the exit. If the fix re-scans newest-first instead,
// this test flips to expecting a dispatch.
func TestIssue646ScanInconclusiveStrandBoundedByHorizon(t *testing.T) {
	// 10 FULL pages (maxCommentPages) → ListCommentsAll reports truncated.
	page := make([]map[string]string, 100)
	for i := range page {
		page[i] = map[string]string{"body": "chat", "created_at": "2026-09-01T00:00:00Z"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(page) // always a full page → truncated at the cap
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// Armed 5h ago — inside the 6h horizon, far past every grace.
	at := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#2621", "deadbeef123",
		time.Now().Add(-5*time.Hour), nil)
	deps, ctx := gateEnv(t, wf, st, at)
	_ = at
	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("inconclusive scan must not dispatch (fail-closed), got %d", len(out))
	}
	claims, err := attempt.LiveReviewClaims(ctx, deps.Client, wf)
	if err != nil || len(claims) != 1 {
		t.Fatalf("inconclusive strand: the claim stays armed until the horizon, live=%d (%v)", len(claims), err)
	}
}

// T5 — the era-strand candidate the issue points at with "each already green
// or green-before-arm": the merge-rule contract and the human's eyes
// disagree. A required context with NO record at the head ("no records at
// head") reads as pending forever — no CI wake can ever arrive for a context
// that never runs — and the queued claim waits out the horizon. This is the
// gate behaving as ADR-0006 designs it (merge rules are the contract), but
// it is observationally identical to the #646 strand from the PR surface.
// Documents the shape so the fix discussion has the mechanism on the table.
func TestIssue646MissingRequiredContextHoldsQueuedClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			// The human sees green — but that context is NOT the required
			// one; the required context never posts at this head.
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"context": "some-other-ci", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	at := claimFixture(wf, "git.rezus.cloud/tibrez/rhesadox#2621", "deadbeef123",
		time.Now().Add(-time.Hour), nil)
	deps, ctx := gateEnv(t, wf, st, at)
	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a required context with no record must hold the claim (merge rules are the contract), got %d dispatches", len(out))
	}
	// The why is persisted on the CLAIM (hold note, #644's wall row) — the
	// sweep headline may belong to another surface this cycle.
	var held v1alpha1.Attempt
	if err := deps.Client.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: at.Name}, &held); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if held.Status.Review == nil || !strings.Contains(held.Status.Review.HoldNote, "no records at head") {
		t.Fatalf("the hold note must name the missing contexts, got %+v", held.Status.Review)
	}
}
