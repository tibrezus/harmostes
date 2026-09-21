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
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// refusedPRServer: a labeled, green PR whose conversation carries a
// REQUEST_CHANGES verdict at its head (the "already reviewed" world).
func refusedPRServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			// the labeled scan (ListLabeledOpenPulls) — one page, oldest first
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
				map[string]string{"body": "verdict\n\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->"},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{}})
		default:
			http.NotFound(w, req)
		}
	}))
}

func TestRefusalMemoisesAndSkipsLabeledScan(t *testing.T) {
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
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

	// Sweep 2: the labeled scan re-discovers the still-labeled PR (the gate
	// cannot remove the label) — the memo must skip it BEFORE any host
	// evaluation: no dispatch, and no NEW refusal entries (no churn).
	out2, err := RunReviewGateSweep(ctx, deps, wf)
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(out2) != 0 {
		t.Fatalf("memo hit must not dispatch, got %d dispatches", len(out2))
	}
	if len(st.last.ReviewReady.Refusals) != 1 {
		t.Fatalf("repeat sweeps must not grow the memo, got %d entries", len(st.last.ReviewReady.Refusals))
	}
}

func TestRefusalMemoMissFlowsOnPush(t *testing.T) {
	// A push produces a NEW head: the memo entry (old sha) must not match,
	// and the candidate flows into Evaluate normally.
	clearTriggerEnv(t)
	srv := refusedPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
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
