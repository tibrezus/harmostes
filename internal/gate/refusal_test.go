package gate

// The standing-verdict refusal loop-closer (#567): the gate is read-only —
// it cannot remove the label from a refused PR — so the refusal is memoised
// on the workflow status and the labeled scan must SKIP memoised heads
// before any host call. A push (new head) misses the memo and flows
// normally. These tests pin the loop at the sweep boundary: without the
// memo, a refused PR re-arms every sweep forever (the rhesadox#2359
// churn — six identical reviews of one head).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// refusedPRServer: a labeled, green PR whose conversation carries a
// REQUEST_CHANGES verdict at its head (the "already reviewed" world).
// Counters make the memo's cost contract assertable: a memo hit must walk
// ZERO comment pages (the expensive full-history scan) — the single-PR
// head fetch is the one permitted call for scan candidates (sha-matching
// needs it); /comments must never be hit for a skipped candidate.
type countingServer struct {
	*httptest.Server
	commentWalks *int32
	singlePulls  *int32
	commentPosts *int32
}

func refusedPRServer(t *testing.T) countingServer {
	t.Helper()
	var commentWalks, singlePulls, commentPosts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/comments") {
			atomic.AddInt32(&commentPosts, 1) // the #577 refusal notice
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			// the labeled scan (ListLabeledOpenPulls) — one page, oldest first
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"number": 99, "updated_at": "2026-09-21T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			atomic.AddInt32(&singlePulls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			atomic.AddInt32(&commentWalks, 1)
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"body": "verdict\n\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->",
					"id": 41053, "html_url": "https://git.rezus.cloud/tibrez/rhesadox/pulls/99#issuecomment-41053"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{}})
		default:
			http.NotFound(w, req)
		}
	})
	return countingServer{httptest.NewServer(mux), &commentWalks, &singlePulls, &commentPosts}
}

func TestRefusalMemoisesAndSkipsLabeledScan(t *testing.T) {
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv.Server, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	// Sweep 1: the labeled scan finds the PR, Evaluate refuses (verdict
	// standing at head), the refusal is memoised, nothing dispatches.
	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a refused head must not dispatch, got %d dispatches", len(out))
	}
	rr := st.last.ReviewReady
	if rr == nil || len(rr.Refusals) != 1 {
		t.Fatalf("refusal must be memoised on the workflow status, got %+v", rr)
	}
	ref := rr.Refusals[0]
	if ref.HeadSHA != "deadbeef123" || ref.PR != 99 {
		t.Fatalf("memo keyed to the refused (pr, head), got %+v", ref)
	}
	if !strings.Contains(ref.Reason, "exactly once") {
		t.Fatalf("memo must carry the dev-facing directive, got %q", ref.Reason)
	}
	walks1, pulls1 := *srv.commentWalks, *srv.singlePulls

	// Sweep 2: the labeled scan re-discovers the still-labeled PR (the gate
	// cannot remove the label) — the memo must skip it with ZERO comment
	// walks (the full-history scan is the expensive call this exists to
	// avoid; neuter the Matches check and this assertion is what goes red —
	// mutation-verified) and exactly ONE single-PR head fetch (the sha
	// match for a scan candidate needs it). No dispatch, no new entries.
	out2, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(out2) != 0 {
		t.Fatalf("memo hit must not dispatch, got %d dispatches", len(out2))
	}
	if *srv.commentWalks != walks1 {
		t.Fatalf("memo hit must not walk the conversation: %d → %d comment walks", walks1, *srv.commentWalks)
	}
	if *srv.singlePulls != pulls1+1 {
		t.Fatalf("memo hit on a sha-less scan candidate pays exactly one head fetch: %d → %d", pulls1, *srv.singlePulls)
	}
	if len(st.last.ReviewReady.Refusals) != 1 {
		t.Fatalf("repeat sweeps must not grow the memo, got %d entries", len(st.last.ReviewReady.Refusals))
	}
	if !strings.Contains(st.last.ReviewReady.LastReason, "verdict standing") {
		t.Fatalf("the skip must speak in status, got %q", st.last.ReviewReady.LastReason)
	}
}

func TestRefusalMemoMissFlowsOnPush(t *testing.T) {
	// A push produces a NEW head: the memo entry (old sha) must not match,
	// and the candidate flows into Evaluate normally.
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv.Server, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// Seed the memo through the status the sweep reads (liveAgg): a refusal
	// at some OTHER head — the pre-push world.
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{Refusals: []v1alpha1.ReviewRefusal{{
		Repo: "git.rezus.cloud/tibrez/rhesadox", PR: 99,
		HeadSHA: "0000000",
		At:      &metav1.Time{Time: time.Now()},
	}}}
	deps, ctx := gateEnv(t, wf, st)

	out, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// The pushed head has no standing verdict (server verdict is at
	// deadbeef123, memo at 0000000) — but the head IS deadbeef123 with a
	// standing verdict in this fixture, so Evaluate refuses afresh and
	// re-memoises at the new head. Either way: no dispatch, memo updated.
	if len(out) != 0 {
		t.Fatalf("a head with a standing verdict must never dispatch, got %d", len(out))
	}
	rr := st.last.ReviewReady
	if len(rr.Refusals) != 2 {
		t.Fatalf("a non-matching memo entry must survive and the fresh refusal join it, got %+v", rr.Refusals)
	}
}

// A human-shaped re-request at a refused head is NOT silently skipped
// (#328 precedence, r36): it falls into Evaluate, whose verdict-standing
// refusal re-emits the directive VISIBLY (status + timeline) and refreshes
// the memo — refused-with-a-directive, never an invisible continue. On
// Forgejo every label add is a granular label_updated; "human-shaped" is
// the label being PRESENT at the resolved head (a removal answers nothing).
func TestRefusalHumanReRequestAnswersVisibly(t *testing.T) {
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv.Server, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	// Pre-seed the memo: the head was refused in an earlier sweep.
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{Refusals: []v1alpha1.ReviewRefusal{{
		Repo: "git.rezus.cloud/tibrez/rhesadox", PR: 99,
		HeadSHA: "deadbeef123",
		At:      &metav1.Time{Time: time.Now()},
	}}}
	deps, ctx := gateEnv(t, wf, st)
	deps.Wake = GateWake{
		Repo: "git.rezus.cloud/tibrez/rhesadox", Action: "label_updated", // Forgejo granular: add/remove indistinguishable in the payload
		PR: "git.rezus.cloud/tibrez/rhesadox#99", Revision: "deadbeef123",
	}

	out, err := RunReviewGateWake(ctx, deps, wf)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a refused head must not dispatch even for a human re-request, got %d", len(out))
	}
	rr := st.last.ReviewReady
	if rr == nil {
		t.Fatal("no aggregates written")
	}
	if !strings.Contains(rr.LastReason, "verdict already stands at head deadbeef123") {
		t.Fatalf("the human re-request must be ANSWERED with the standing-verdict directive in status, got %q", rr.LastReason)
	}
	if len(rr.Refusals) != 1 {
		t.Fatalf("re-memoise must upsert, not grow, got %d", len(rr.Refusals))
	}
}

// ── #577: the standing-verdict refusal posts a one-line PR comment —
// status + timeline alone read as "no response" (rhesadox#2340: four
// re-arms over four hours against a verdict that landed before the first
// re-arm). ──

func TestRefusalNoticePostsOnceWithVerdictLink(t *testing.T) {
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv.Server, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	// Sweep 1: the refusal is created — exactly ONE host notice, carrying
	// the deep link to the verdict.
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if got := atomic.LoadInt32(srv.commentPosts); got != 1 {
		t.Fatalf("the first refusal must post exactly one notice, got %d", got)
	}
	rr := st.last.ReviewReady
	if rr == nil || len(rr.Refusals) != 1 {
		t.Fatalf("refusal memoised, got %+v", rr)
	}
	if !rr.Refusals[0].HostNotified {
		t.Fatalf("the HostNotified marker must persist, got %+v", rr.Refusals[0])
	}
	if rr.Refusals[0].VerdictURL != "https://git.rezus.cloud/tibrez/rhesadox/pulls/99#issuecomment-41053" {
		t.Fatalf("the refusal must carry the verdict deep link, got %q", rr.Refusals[0].VerdictURL)
	}

	// Sweep 2: the memo skip must NOT re-post (HostNotified guards it).
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := atomic.LoadInt32(srv.commentPosts); got != 1 {
		t.Fatalf("repeat sweeps must not re-post the notice, got %d posts", got)
	}
}

// The retry leg: a host whose comment POST fails on the first sweep — the
// marker stays false and the next sweep posts exactly once more. A lost
// notice must not become a permanently silent refusal.
func TestRefusalNoticeRetriesAfterHostFailure(t *testing.T) {
	clearTriggerEnv(t)
	var posts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/comments"):
			if atomic.AddInt32(&posts, 1) == 1 {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"number": 99, "updated_at": "2026-09-21T00:00:00Z",
					"labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.Contains(req.URL.Path, "/pulls/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "deadbeef123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"body": "verdict\n\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->",
					"id": 41053, "html_url": "https://git.rezus.cloud/tibrez/rhesadox/pulls/99#issuecomment-41053"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{}})
		default:
			http.NotFound(w, req)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	wf := gateWorkflow()
	st := &fakeStatus{}
	deps, ctx := gateEnv(t, wf, st)

	// Sweep 1: the notice post fails — the refusal memoises, the marker
	// stays false (retry), and nothing else breaks.
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	rr := st.last.ReviewReady
	if rr == nil || len(rr.Refusals) != 1 || rr.Refusals[0].HostNotified {
		t.Fatalf("a failed post must leave HostNotified false for the retry, got %+v", rr)
	}
	// Sweep 2: the host recovered — exactly one notice lands, marker flips.
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("the retry must post exactly once more, got %d attempts", got)
	}
	if !st.last.ReviewReady.Refusals[0].HostNotified {
		t.Fatalf("the marker must persist after the successful retry, got %+v", st.last.ReviewReady.Refusals[0])
	}
	// Sweep 3: silent again.
	if _, err := RunReviewGateSweep(ctx, deps, wf); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("post-retry sweeps must stay silent, got %d attempts", got)
	}
}
