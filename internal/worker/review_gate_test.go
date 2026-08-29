package worker

import (
	"context"
	"encoding/json"
	"fmt"
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

// clearTriggerEnv makes the gate tests hermetic: a worker pod exports
// HARMOSTES_TRIGGER_* (the trigger payload), and reviewGate prefers env
// over annotations — uncleaned, six tests fail with "missing #"
// (the reviewer's round-E2E finding).
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
// inspect st.last (which the seed itself populates).
func seedLive(st *fakeStatus, wf *v1alpha1.Workflow) {
	if rr := wf.Status.ReviewReady; rr != nil {
		cp := *rr
		st.last.ReviewReady = &cp
	}
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

func TestReviewGateWaitingReturnsNilAtSeam(t *testing.T) {
	clearTriggerEnv(t)
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
	clearTriggerEnv(t)
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
	// Proceed stays armed with the dispatch marker (#250 r4): the run is
	// in flight; sweeps wait on the verdict window, consume clears.
	rr := st.last.ReviewReady
	if rr == nil || rr.LastDecision != "proceed" || rr.ArmedPR != 42 || rr.ArmedSha != "abc123def456" {
		t.Fatalf("post-proceed state = %+v", rr)
	}
	if rr.DispatchedAt == nil {
		t.Fatalf("dispatch marker missing: %+v", rr)
	}

	// proceed → persist → re-sweep end-to-end: the written status feeds
	// back; the next sweep must WAIT in flight, not re-dispatch.
	srv2 := verdictServerNoVerdictYet(t)
	pinReviewAPI(t, srv2, true)
	wf.Status.ReviewReady = st.last.ReviewReady
	env2 := reviewGate(context.Background(), testDeps(st), wf)
	if env2 != nil {
		t.Fatal("re-sweep after proceed re-dispatched (marker not durable)")
	}
	if rr2 := st.last.ReviewReady; rr2.LastDecision != "waiting" || !strings.Contains(rr2.LastReason, "in flight") || rr2.DispatchedAt == nil {
		t.Fatalf("re-sweep state = %+v", rr2)
	}
}

func TestReviewGateDisarmHint(t *testing.T) {
	clearTriggerEnv(t)
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
	clearTriggerEnv(t)
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
	seedLive(st, wf)
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("armed+green must proceed without a new event")
	}
}

func TestReviewGateOutOfScopeRepoIgnored(t *testing.T) {
	clearTriggerEnv(t)
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
	clearTriggerEnv(t)
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

func TestReviewGateUnrelatedPushDoesNotHijackArmed(t *testing.T) {
	// Live regression: a synchronize wake on another in-scope PR must not
	// re-target/disarm an armed review.
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedRepo: "git.rezus.cloud/tibrez/rhesadox", ArmedPR: 1566,
		ArmedSha: "abc123", ArmedSince: &since, LastDecision: "waiting",
	}
	seedLive(st, wf)
	// push wake for a DIFFERENT pr (env-first path)
	t.Setenv("HARMOSTES_TRIGGER_PR", "git.rezus.cloud/tibrez/rhesadox#1577")
	t.Setenv("HARMOSTES_TRIGGER_ACTION", "synchronize")
	t.Setenv("HARMOSTES_TRIGGER_REVISION", "dd8cfd14")
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("unrelated push must not proceed")
	}
	// the gate must not PATCH at all — the live armed state (seeded by
	// seedLive) is what stays preserved, untouched
	if st.patches != 0 {
		t.Fatalf("unrelated push must not touch status, got %d patches: %+v", st.patches, st.last.ReviewReady)
	}
}

func TestReviewGateLabelWakeRetargets(t *testing.T) {
	clearTriggerEnv(t)
	// A labeled wake on another PR re-targets (newest REQUEST wins).
	srv := reviewServer(t, "open", []string{"needs-review"},
		map[string]string{"ci / build-test (push)": "success"}, []string{"ci / build-test (push)"})
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedRepo: "git.rezus.cloud/tibrez/rhesadox", ArmedPR: 1566,
		ArmedSha: "abc123", ArmedSince: &since, LastDecision: "waiting",
	}
	seedLive(st, wf)
	wf.Annotations = map[string]string{
		"harmostes.dev/trigger-pr":     "git.rezus.cloud/tibrez/rhesadox#42",
		"harmostes.dev/trigger-action": "labeled",
	}
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil || env.PR != 42 {
		t.Fatalf("label wake must re-target to 42, got %+v", env)
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

func TestEmitGateTransitionDeduplicatesWaiting(t *testing.T) {
	var emitted []string
	w := &capturingWriter{kinds: &emitted}
	armed := &v1alpha1.ReviewReadyStatus{
		LastDecision: "waiting",
		LastReason:   "ci red at head",
	}
	// same waiting state → non-event
	emitGateTransition(t.Context(), w, armed, review.Result{
		Evaluation: review.Evaluation{Decision: review.DecisionWaiting, Reason: "ci red at head"},
	}, "git.rezus.cloud/tibrez/rhesadox", 1566)
	if len(emitted) != 0 {
		t.Fatalf("repeated waiting must not emit, got %v", emitted)
	}
	// proceed → one event (plus armed if sha changed)
	emitGateTransition(t.Context(), w, armed, review.Result{
		Evaluation:  review.Evaluation{Decision: review.DecisionProceed, Reason: "label present"},
		NewArmedSha: "abc",
	}, "git.rezus.cloud/tibrez/rhesadox", 1566)
	if len(emitted) == 0 || emitted[len(emitted)-1] != "gate.proceed" {
		t.Fatalf("proceed must emit gate.proceed, got %v", emitted)
	}
}

type capturingWriter struct{ kinds *[]string }

func (c *capturingWriter) Emit(_ context.Context, kind, _ string, _ any) error {
	*c.kinds = append(*c.kinds, kind)
	return nil
}

// stubGateAPI is a minimal review.API for gate tests that don't need HTTP.
type stubGateAPI struct {
	labeled    []review.PullRequest
	labeledErr error
}

func (s *stubGateAPI) GetPullRequest(context.Context, string, int) (*review.PullRequest, error) {
	return nil, fmt.Errorf("not stubbed")
}
func (s *stubGateAPI) ListLabeledOpenPulls(context.Context, string, string) ([]review.PullRequest, error) {
	return s.labeled, s.labeledErr
}
func (s *stubGateAPI) RequiredContexts(context.Context, string, string) ([]string, error) {
	return nil, fmt.Errorf("not stubbed")
}
func (s *stubGateAPI) ContextStates(context.Context, string, string) (map[string]string, error) {
	return nil, fmt.Errorf("not stubbed")
}
func (s *stubGateAPI) ListComments(context.Context, string, int, time.Time) ([]review.IssueComment, error) {
	return nil, fmt.Errorf("not stubbed")
}

// #249: idle is no longer nothing — a backlog pass lists labeled open PRs.
// Nothing labeled → still nil envelope, still no status write.
func TestReviewGateIdleWithoutBacklogCostsNothing(t *testing.T) {
	clearTriggerEnv(t)
	prev := newReviewAPI
	newReviewAPI = func() review.API { return &stubGateAPI{} }
	t.Cleanup(func() { newReviewAPI = prev })
	st := &fakeStatus{}
	wf := gateWorkflow()
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("idle gate with empty backlog must return nil envelope")
	}
	if st.last.ReviewReady != nil {
		t.Fatalf("must not write status, got %+v", st.last.ReviewReady)
	}
}

// #249 live incident: labels added while the gate was busy starve — the
// newest label stole the single armed slot. The backlog pass arms the OLDEST
// labeled open PR on the next sweep.
func TestReviewGateBacklogArmsOldestLabeled(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			// oldest-first as the API promises (sort=oldest)
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "labels": []map[string]string{{"name": "needs-review"}}},
				{"number": 9, "labels": []map[string]string{{"name": "needs-review"}}},
			})
		case strings.HasSuffix(req.URL.Path, "/pulls/7"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "backlog77"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
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
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow() // no trigger, nothing armed

	reviewGate(context.Background(), testDeps(st), wf)
	rr := st.last.ReviewReady
	// #7 is green+labeled → the gate must have evaluated IT and proceeded
	// (proceed clears the armed fields by design; the decision is the
	// witness). #9 has no route: had the gate picked the newest instead,
	// evaluation would have errored, not proceeded.
	if rr == nil || rr.LastDecision != "proceed" {
		t.Fatalf("backlog must arm the oldest labeled PR #7 and proceed, got %+v", rr)
	}
	if !strings.Contains(rr.LastReason, "green") {
		t.Fatalf("proceed reason wrong: %+v", rr)
	}
}

// An armed slot must never consult the backlog: re-evaluation of the armed
// PR is the sweep's whole job.
func TestReviewGateArmedSweepDoesNotRetargetToBacklog(t *testing.T) {
	clearTriggerEnv(t)
	// PR 42 (armed) is green; the backlog would offer #7. Serving BOTH and
	// asserting the gate still evaluates 42 proves no hijack.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "labels": []map[string]string{}}, // not labeled: a hijack would stand down, not proceed
			})
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
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
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.Now()
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox", ArmedSha: "abc123def456", ArmedSince: &since, LastDecision: "waiting"}
	seedLive(st, wf)

	reviewGate(context.Background(), testDeps(st), wf)
	rr := st.last.ReviewReady
	// #42 green → proceed (armed fields cleared by design). #7 unlabeled:
	// a backlog hijack would NOT proceed — so proceed proves #42 ran.
	if rr == nil || rr.LastDecision != "proceed" {
		t.Fatalf("armed sweep must evaluate PR 42 to proceed, got %+v", rr)
	}
}

// GitHub-kind dialect (#250 r1): the backlog list must carry
// per_page/direction=asc (sort=oldest&limit are Forgejo params — on GitHub
// the shared form returned newest-first capped at 30, hiding the starved).
func TestBacklogListGitHubDialect(t *testing.T) {
	clearTriggerEnv(t)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/pulls") {
			gotQuery = req.URL.RawQuery
			json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	prev := newReviewAPI
	newReviewAPI = func() review.API {
		return &review.RESTAPI{Client: srv.Client(), TokenLookup: func(string) string { return "tok" }, BaseOverride: srv.URL}
	}
	t.Cleanup(func() { newReviewAPI = prev })

	wf := gateWorkflow()
	wf.Spec.Config = []byte(`{"repos": ["github.com/tibrezus/harmostes"]}`)
	st := &fakeStatus{}
	reviewGate(context.Background(), testDeps(st), wf)
	for _, want := range []string{"per_page=50", "direction=asc", "sort=created"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("GitHub backlog query missing %q: %s", want, gotQuery)
		}
	}
	if strings.Contains(gotQuery, "sort=oldest") || strings.Contains(gotQuery, "limit=") {
		t.Errorf("Forgejo params leaked into GitHub query: %s", gotQuery)
	}
}

// Dispatched reviews must not re-dispatch on sweeps (#250 r2): armed with
// lastDecision=proceed, label present, CI green, NO verdict in the window →
// waiting ("in flight"), never a second proceed envelope.
func TestDispatchedReviewNotReDispatched(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{}) // no verdict yet
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.Now()
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &since,
		DispatchedAt: &since, LastDecision: "proceed",
	}
	seedLive(st, wf)

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("dispatched review must not produce a second proceed envelope")
	}
	rr := st.last.ReviewReady
	if rr == nil || rr.LastDecision != "waiting" || !strings.Contains(rr.LastReason, "in flight") {
		t.Fatalf("expected waiting/in-flight, got %+v", rr)
	}

	// sweep+2 (#250 r3): the in-flight WAITING evaluation overwrote
	// lastDecision — feed the written status back and sweep again. The
	// durable marker (dispatchedAt) must keep suppressing dispatch; a
	// lastDecision-based marker evaporates here and re-proceeds.
	for sweep := 0; sweep < 3; sweep++ {
		wf.Status.ReviewReady = st.last.ReviewReady
		env := reviewGate(context.Background(), testDeps(st), wf)
		if env != nil {
			t.Fatalf("sweep+%d: re-dispatched (marker evaporated): %+v", sweep+2, st.last.ReviewReady)
		}
		if rr := st.last.ReviewReady; rr.LastDecision != "waiting" || !strings.Contains(rr.LastReason, "in flight") {
			t.Fatalf("sweep+%d: expected in-flight waiting, got %+v", sweep+2, rr)
		}
	}

	// Verdict lands in the window → consumed → standdown (armed cleared).
	srv2 := verdictServer(t)
	pinReviewAPI(t, srv2, true)
	wf.Status.ReviewReady = st.last.ReviewReady
	env = reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("consumed review must not re-dispatch")
	}
	if rr := st.last.ReviewReady; rr == nil || rr.LastDecision != "standdown" || rr.ArmedPR != 0 {
		t.Fatalf("expected standdown after verdict, got %+v", rr)
	}
}

// verdictServer: PR 42 open+labeled+green, one verdict trailer comment since
// the epoch window.
func verdictServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{
				{"body": "verdict\n<!-- pr-review: APPROVE @ abc -->"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A failing backlog listing degrades to idle — never wedges the gate.
func TestBacklogListErrorDegradesToIdle(t *testing.T) {
	clearTriggerEnv(t)
	prev := newReviewAPI
	newReviewAPI = func() review.API { return &stubGateAPI{labeledErr: fmt.Errorf("boom")} }
	t.Cleanup(func() { newReviewAPI = prev })
	st := &fakeStatus{}
	env := reviewGate(context.Background(), testDeps(st), gateWorkflow())
	if env != nil {
		t.Fatal("list error must degrade to nil envelope")
	}
	if st.last.ReviewReady != nil {
		t.Fatalf("no status write expected, got %+v", st.last.ReviewReady)
	}
}

// verdictServerNoVerdictYet: PR 42 open+labeled+green, zero comments.
func verdictServerNoVerdictYet(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// #250 r5: a labeled wake on a DIFFERENT PR while a review is dispatched
// must retarget with a FRESH slot — the old PR's dispatch marker must not
// leak, or the new PR waits "in flight" (scanning the wrong PR's comments)
// for up to a full Horizon.
func TestReviewGateLabelWakeRetargetsWhileDispatched(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/7"):
			// the NEW PR: labeled, green → must PROCEED on its own merits
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "newpr777"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
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
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.Now()
	// OLD armed review on #42, dispatched (in flight), still labeled.
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &since, DispatchedAt: &since,
	}
	seedLive(st, wf)
	// Labeled wake for #7.
	os.Setenv("HARMOSTES_TRIGGER_PR", "git.rezus.cloud/tibrez/rhesadox#7")
	os.Setenv("HARMOSTES_TRIGGER_ACTION", "labeled")
	t.Cleanup(func() { os.Unsetenv("HARMOSTES_TRIGGER_PR"); os.Unsetenv("HARMOSTES_TRIGGER_ACTION") })

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil || env.PR != 7 {
		t.Fatalf("labeled #7 (green) must proceed on retarget, got env=%+v status=%+v", env, st.last.ReviewReady)
	}
	rr := st.last.ReviewReady
	if rr.ArmedPR != 7 || rr.DispatchedAt == nil {
		t.Fatalf("retarget must arm #7 with a FRESH dispatch marker, got %+v", rr)
	}
	// The stale marker never made #7 wait in flight: it proceeded (env != nil
	// above) — the r5 bug would have returned nil with waiting/in-flight.
}

// Dispatched + horizon: a run that dies (outage class) must be released by
// the horizon standdown — armed slot AND marker cleared, backlog can re-arm.
func TestDispatchedHorizonReleasesSlot(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	old := metav1.NewTime(time.Now().Add(-7 * time.Hour)) // beyond the 6h horizon
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &old, DispatchedAt: &old,
	}
	seedLive(st, wf)
	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("horizon-exceeded dispatched review must not dispatch")
	}
	rr := st.last.ReviewReady
	if rr == nil || rr.LastDecision != "standdown" || rr.ArmedPR != 0 || rr.DispatchedAt != nil {
		t.Fatalf("horizon must release slot + marker, got %+v", rr)
	}
}

// #248 acceptance: a dispatch that dies (helm-roll kill, wedged worker,
// lost run) recovers WITHOUT any external label toggle — the stale marker
// stands the slot down, and the very next idle sweep's backlog pass (#249)
// re-arms the still-labeled PR and re-dispatches. Total recovery =
// DispatchTimeout + one sweep interval.
func TestDispatchedDeadRunRecoversViaBacklog(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.HasSuffix(req.URL.Path, "/pulls"):
			// backlog listing: the dead review's label is still on the PR
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 42, "labels": []map[string]string{{"name": "needs-review"}},
			}})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{}) // no verdict — the run died
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Spec.ReviewReady.DispatchTimeout = "45m"
	stale := metav1.NewTime(time.Now().Add(-50 * time.Minute)) // past the bound
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &stale, DispatchedAt: &stale,
		LastDecision: "waiting", LastReason: "review in flight — dispatched, verdict pending",
	}
	seedLive(st, wf)

	// Sweep 1: the dead dispatch is presumed dead — standdown, slot+marker released.
	if env := reviewGate(context.Background(), testDeps(st), wf); env != nil {
		t.Fatal("a dead dispatch must not produce an envelope")
	}
	rr := st.last.ReviewReady
	if rr == nil || rr.LastDecision != "standdown" || !strings.Contains(rr.LastReason, "presumed dead") {
		t.Fatalf("sweep 1 must stand down the dead dispatch, got %+v", rr)
	}
	if rr.ArmedPR != 0 || rr.DispatchedAt != nil {
		t.Fatalf("sweep 1 must release slot + marker, got %+v", rr)
	}

	// Sweep 2 (no trigger, nothing armed): the backlog pass re-arms the
	// still-labeled PR and the fresh evaluation PROCEEDS — the review
	// re-runs without any external label toggle.
	wf2 := gateWorkflow()
	wf2.Spec.ReviewReady.DispatchTimeout = "45m"
	wf2.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		LastDecision: "standdown", LastReason: rr.LastReason,
	}
	env := reviewGate(context.Background(), testDeps(st), wf2)
	if env == nil {
		t.Fatal("sweep 2 must re-dispatch the review (backlog re-arm + proceed)")
	}
	if env.PR != 42 || env.HeadSHA != "abc123def456" {
		t.Fatalf("re-dispatch must target the dead review's PR/head, got %+v", env)
	}
	if rr2 := st.last.ReviewReady; rr2 == nil || rr2.DispatchedAt == nil || rr2.ArmedPR != 42 {
		t.Fatalf("re-dispatch must stamp a fresh marker and re-arm, got %+v", rr2)
	}
}

// TestGateMarkerSurvivesStaleSnapshot is THE #257 regression: the live CR
// carries a dispatch marker (another writer set it after this run's
// consumer fetched its snapshot — snapshot has none). A waiting evaluation
// must keep the LIVE marker (the claim is still in flight) and must not
// re-proceed on the marker-less snapshot. Live incident: dispatchedAt
// re-stamped 13:04→13:49 under DNS-failure churn, each sweep re-proceeding.
func TestGateMarkerSurvivesStaleSnapshot(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/42"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "abc123def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/branch_protections/"):
			json.NewEncoder(w).Encode(map[string]any{"status_check_contexts": []string{"ci / build-test (push)"}})
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		case strings.Contains(req.URL.Path, "/issues/42/comments"):
			json.NewEncoder(w).Encode([]map[string]any{}) // no verdict yet
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	since := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	// Run-start SNAPSHOT: armed but marker-less (fetched before the marker
	// was set).
	wf.Status.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &since, LastDecision: "proceed",
	}
	// LIVE status: the marker EXISTS (dispatch happened after the fetch).
	liveMarker := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", ArmedSince: &since,
		DispatchedAt: &liveMarker, LastDecision: "proceed",
	}

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env != nil {
		t.Fatal("live marker must suppress the second dispatch — re-proceed is the #257 bug")
	}
	rr := st.last.ReviewReady
	if rr == nil || rr.LastDecision != "waiting" || rr.DispatchedAt == nil {
		t.Fatalf("waiting must preserve the live marker, got %+v", rr)
	}
	if !rr.DispatchedAt.Time.Equal(liveMarker.Time) {
		t.Fatalf("marker must be the LIVE one, got %+v want %v", rr.DispatchedAt, liveMarker.Time)
	}
}

// TestGateRetargetClearsLiveMarker: the marker belongs to the OLD armed
// review — a request-shaped wake re-targeting to a different PR clears it
// even though the live claim carried one (r5, now live-state aware).
func TestGateRetargetClearsLiveMarker(t *testing.T) {
	clearTriggerEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/99"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "newhead99"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
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
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	marker := metav1.NewTime(time.Now().Add(-3 * time.Minute))
	// Live claim: armed on #42 with a marker.
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 42, ArmedRepo: "git.rezus.cloud/tibrez/rhesadox",
		ArmedSha: "abc123def456", DispatchedAt: &marker, LastDecision: "proceed",
	}
	// Request-shaped wake: label touched on #99 — re-target.
	t.Setenv("HARMOSTES_TRIGGER_PR", "git.rezus.cloud/tibrez/rhesadox#99")
	t.Setenv("HARMOSTES_TRIGGER_ACTION", "labeled")
	t.Setenv("HARMOSTES_TRIGGER_REVISION", "newhead99")

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("retarget to a labeled green PR must proceed")
	}
	rr := st.last.ReviewReady
	if rr == nil || rr.ArmedPR != 99 || rr.DispatchedAt == nil || rr.DispatchedAt.Time.Equal(marker.Time) {
		t.Fatalf("retarget must re-arm on #99 with a FRESH marker, got %+v", rr)
	}
}

// greenPRServer serves the minimal API a proceed needs for repo/name.
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
// NXDOMAIN there (live incident 2026-08-29).
func TestGateNormalizesBareRepoWake(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow() // scope: git.rezus.cloud/tibrez/rhesadox (full form)
	t.Setenv("HARMOSTES_TRIGGER_PR", "tibrez/rhesadox#99")
	t.Setenv("HARMOSTES_TRIGGER_ACTION", "labeled")
	t.Setenv("HARMOSTES_TRIGGER_REVISION", "headabc123")

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("bare-form wake on a full-form scope must proceed")
	}
	if env.Repo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("envelope Repo must be host-qualified, got %q", env.Repo)
	}
	if rr := st.last.ReviewReady; rr == nil || rr.ArmedRepo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("armedRepo must persist host-qualified, got %+v", rr)
	}
}

// #268: a bare armedRepo persisted by an older gate self-heals — the sweep
// normalizes before evaluation and rewrites the armed state at proceed.
func TestGateSelfHealsBareArmedRepo(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	st.last.ReviewReady = &v1alpha1.ReviewReadyStatus{
		ArmedPR: 99, ArmedRepo: "tibrez/rhesadox", ArmedSha: "headabc123", // poisoned form
	}

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("sweep on a bare armedRepo must proceed (self-healed)")
	}
	if env.Repo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("envelope Repo must be host-qualified, got %q", env.Repo)
	}
	if rr := st.last.ReviewReady; rr == nil || rr.ArmedRepo != "git.rezus.cloud/tibrez/rhesadox" {
		t.Fatalf("armedRepo must be rewritten host-qualified, got %+v", rr)
	}
}

// #268: bare wake AND bare scope (the exact live-incident config) — bare
// scope means GitHub implied (repoInScope), so the pointer qualifies to
// github.com and still proceeds with a host-qualified envelope.
func TestGateNormalizesBareScopeToGitHub(t *testing.T) {
	clearTriggerEnv(t)
	srv := greenPRServer(t)
	t.Cleanup(srv.Close)
	pinReviewAPI(t, srv, true)
	st := &fakeStatus{}
	wf := gateWorkflow()
	wf.Spec.Config = []byte(`{"repos": ["tibrez/rhesadox"]}`) // bare scope
	t.Setenv("HARMOSTES_TRIGGER_PR", "tibrez/rhesadox#99")
	t.Setenv("HARMOSTES_TRIGGER_ACTION", "labeled")
	t.Setenv("HARMOSTES_TRIGGER_REVISION", "headabc123")

	env := reviewGate(context.Background(), testDeps(st), wf)
	if env == nil {
		t.Fatal("bare wake on a bare scope must proceed (github.com implied)")
	}
	if env.Repo != "github.com/tibrez/rhesadox" {
		t.Fatalf("envelope Repo must be github-qualified, got %q", env.Repo)
	}
}
