package worker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/review"
)

// Shared fixtures for the gate tests. The single-slot suite was re-indexed
// onto Attempt claims (ADR-0007 phase 4) in review_gate_multiarm_test.go;
// the r4–r6 semantics live there. Evaluate's own table stays in
// internal/review (the claim-shaped core was unchanged by the re-index).

func clearTriggerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HARMOSTES_TRIGGER_PR", "HARMOSTES_TRIGGER_ACTION", "HARMOSTES_TRIGGER_REVISION"} {
		t.Setenv(k, "")
	}
}

// seedLive mirrors the run-start snapshot into the live status the gate now
// reads (#257): decisions come from live state; the snapshot is what the
// consumer fetched at trigger time. Tests that model a DIVERGENCE between
// snapshot and live must seed st.last explicitly AFTER calling this — and
// assertions about "the gate did not write" must count st.patches, not

func gateWorkflow() *v1alpha1.Workflow {
	wf := newWorkflow()
	wf.Spec.ReviewReady = &v1alpha1.ReviewReadySpec{Label: "needs-review", Horizon: "6h"}
	wf.Spec.Config = []byte(`{"repos": ["git.rezus.cloud/tibrez/rhesadox"]}`)
	return wf
}

func pinReviewAPI(t *testing.T, srv *httptest.Server, forgejo bool) {
	t.Helper()
	prev := newReviewAPI
	newReviewAPI = func() review.API {
		base := srv.URL
		if forgejo {
			base += "/api/v1"
		}
		return &review.RESTAPI{Client: srv.Client(), TokenLookup: func(string) string { return "tok" }, BaseOverride: base}
	}
	t.Cleanup(func() { newReviewAPI = prev })
}

func greenPRServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.URL.Path, "/pulls/"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "headabc123"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]any{}) // no verdict yet
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

// #268: a bare owner/name wake (GitHub's full_name form) must arm and
// proceed at the host-qualified form — the envelope's Repo feeds the
// workspace plugin's host split, and a bare pointer is a guaranteed

func TestParsePRPointer(t *testing.T) {
	cases := []struct {
		in        string
		repo      string
		pr        int
		wantError bool
	}{
		{"git.rezus.cloud/tibrez/rhesadox#1566", "git.rezus.cloud/tibrez/rhesadox", 1566, false},
		{"github.com/tibrezus/harmostes#10", "github.com/tibrezus/harmostes", 10, false},
		{"nohash", "", 0, true},
		{"repo/x#zero", "", 0, true},
	}
	for _, c := range cases {
		repo, pr, err := parsePRPointer(c.in)
		if c.wantError {
			if err == nil {
				t.Errorf("parsePRPointer(%q): want error", c.in)
			}
			continue
		}
		if err != nil || repo != c.repo || pr != c.pr {
			t.Errorf("parsePRPointer(%q) = %q,%d,%v", c.in, repo, pr, err)
		}
	}
}

// testDeps builds the minimal Deps the gate needs (status patcher + log).

func testDeps(st *fakeStatus) Deps {
	return Deps{
		Status: st,
		Log:    func(format string, a ...any) {},
	}
}

var _ = review.DecisionProceed // keep the review import for the envelope type assertions
