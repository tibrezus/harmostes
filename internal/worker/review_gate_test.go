package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/review"
)

// reviewServer stands up a fake Forgejo/GitHub API with a scriptable PR view.
func reviewServer(t *testing.T, prState string, labels []string, contexts map[string]string, required []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		labelObjs := []map[string]string{}
		for _, l := range labels {
			labelObjs = append(labelObjs, map[string]string{"name": l})
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": prState, "head": map[string]string{"sha": "abc123def456"},
				"base": map[string]string{"ref": "main"}, "labels": labelObjs,
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": required})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			out := []map[string]string{}
			for c, st := range contexts {
				out = append(out, map[string]string{"context": c, "state": st})
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

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

func TestReviewGateIdleCostsNothing(t *testing.T) {
	// Unarmed, unwoken: no API calls — the review package must not even be
	// constructed with a server. If it tried to call out, it would fail.
	os.Setenv("HARMOSTES_FORGEJO_TOKEN", "tok")
	defer os.Unsetenv("HARMOSTES_FORGEJO_TOKEN")
	wf := gateWorkflow() // no annotations, no status
	st := &fakeStatus{}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("idle gate must return nil envelope")
	}
	if st.last.ReviewReady != nil {
		t.Fatalf("idle gate must not write status, got %+v", st.last.ReviewReady)
	}
}

func TestReviewGateWaitingReturnsNilAtSeam(t *testing.T) {
	// The production seam: the one-shot main calls RunReviewGate BEFORE any
	// provisioning/graph execution; nil means "exit this cycle". (The gate
	// no longer lives in pipeline.Run — that path is legacy since #177.)
	srv := reviewServer(t, "open", []string{"needs-review"},
		map[string]string{"ci / build-test (push)": "pending"}, []string{"ci / build-test (push)"})
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "git.rezus.cloud/tibrez/rhesadox#42",
		"harmostes.dev/trigger-action": "labeled",
	}

	env := RunReviewGate(context.Background(), st, func(format string, a ...any) {}, wf)
	if env != nil {
		t.Fatalf("waiting gate must return nil at the seam, got %+v", env)
	}
	// Armed state persisted: sha + since + decision.
	rr := st.last.ReviewReady
	if rr == nil || rr.ArmedPR != 42 || rr.ArmedSha != "abc123def456" || rr.LastDecision != "waiting" {
		t.Fatalf("armed state = %+v", rr)
	}
	if rr.ArmedSince == nil || time.Since(rr.ArmedSince.Time) > time.Minute {
		t.Fatalf("armedSince = %+v", rr.ArmedSince)
	}
}

func TestReviewGateProceedPassesEnvelope(t *testing.T) {
	srv := reviewServer(t, "open", []string{"needs-review", "other"},
		map[string]string{"ci / build-test (push)": "success"}, []string{"ci / build-test (push)"})
	pinReviewAPI(t, srv, true)
	os.Setenv("HARMOSTES_FORGEJO_TOKEN", "tok")
	defer os.Unsetenv("HARMOSTES_FORGEJO_TOKEN")
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "git.rezus.cloud/tibrez/rhesadox#42",
		"harmostes.dev/trigger-action": "labeled",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("green+label must proceed")
	}
	if env.PR != 42 || env.HeadSHA != "abc123def456" || env.Base != "main" || env.Label != "needs-review" {
		t.Fatalf("envelope = %+v", env)
	}
	if len(env.GreenContexts) != 1 || env.GreenContexts[0] != "ci / build-test (push)" {
		t.Fatalf("green contexts = %+v", env.GreenContexts)
	}
	// Proceed consumes the armed state.
	if rr := st.last.ReviewReady; rr == nil || rr.ArmedPR != 0 || rr.LastDecision != "proceed" {
		t.Fatalf("post-proceed state = %+v", rr)
	}
}

func TestReviewGateDisarmHint(t *testing.T) {
	// action=closed with no reachable PR: still stands down and disarms.
	os.Setenv("HARMOSTES_FORGEJO_TOKEN", "tok")
	defer os.Unsetenv("HARMOSTES_FORGEJO_TOKEN")
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "git.rezus.cloud/tibrez/rhesadox#42",
		"harmostes.dev/trigger-action": "closed",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("closed must not proceed")
	}
	if rr := st.last.ReviewReady; rr == nil || rr.LastDecision != "standdown" || rr.ArmedPR != 0 {
		t.Fatalf("closed state = %+v", rr)
	}
}

func TestReviewGateReEvaluationWithoutEvent(t *testing.T) {
	// Armed from a previous cycle (status), no new annotation: the gate
	// re-evaluates (the poll fallback after CI turns green).
	srv := reviewServer(t, "open", []string{"needs-review"},
		map[string]string{"ci / build-test (push)": "success"}, []string{"ci / build-test (push)"})
	pinReviewAPI(t, srv, true)
	os.Setenv("HARMOSTES_FORGEJO_TOKEN", "tok")
	defer os.Unsetenv("HARMOSTES_FORGEJO_TOKEN")
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedRepo: "git.rezus.cloud/tibrez/rhesadox", ArmedPR: 42,
		ArmedSha: "abc123def456", ArmedSince: &since, LastDecision: "waiting",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("armed+green must proceed without a new event")
	}
}

func TestReviewGateOutOfScopeRepoIgnored(t *testing.T) {
	// A webhook from a repo not in spec.config.repos must not arm the gate
	// (fail closed) — mis-pointed hooks stand down idle.
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "git.rezus.cloud/other/repo#1",
		"harmostes.dev/trigger-action": "labeled",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("out-of-scope wake must not proceed")
	}
	if rr := st.last.ReviewReady; rr == nil || rr.LastDecision != "idle" {
		t.Fatalf("out-of-scope state = %+v", rr)
	}
}

func TestReviewGateBareRepoScopeMatchesGitHub(t *testing.T) {
	// Hermetic: a server that 404s the PR fetch — the scope MUST have
	// matched (bare owner/name → github.com/owner/name) because the gate
	// reached the fetch (waiting), not the out-of-scope ignore (idle).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	pinReviewAPI(t, srv, false)
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Spec.Config = []byte(`{"repos": ["tibrezus/harmostes"]}`)
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "github.com/tibrezus/harmostes#9",
		"harmostes.dev/trigger-action": "labeled",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("unreachable API must not proceed")
	}
	if rr := st.last.ReviewReady; rr == nil || rr.LastDecision != "waiting" {
		t.Fatalf("scope-matched state = %+v (want waiting: fetch failed)", rr)
	}
}

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
