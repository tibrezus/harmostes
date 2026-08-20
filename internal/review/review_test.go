package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAPI stubs the gate's API surface for decision-logic tests.
type fakeAPI struct {
	pr       *PullRequest
	prErr    error
	required []string
	reqErr   error
	states   map[string]string
	ctxErr   error
}

func (f *fakeAPI) GetPullRequest(ctx context.Context, repo string, n int) (*PullRequest, error) {
	return f.pr, f.prErr
}
func (f *fakeAPI) RequiredContexts(ctx context.Context, repo, branch string) ([]string, error) {
	return f.required, f.reqErr
}
func (f *fakeAPI) ContextStates(ctx context.Context, repo, sha string) (map[string]string, error) {
	return f.states, f.ctxErr
}

var base = Params{
	Repo: "git.rezus.cloud/tibrez/rhesadox", PR: 1566, Label: "needs-review",
	Horizon: 6 * time.Hour, Now: time.Unix(1_800_000_000, 0),
}

func openPR(labels ...string) *PullRequest {
	return &PullRequest{State: "open", HeadSHA: "abc123", Base: "main", Labels: labels}
}

func TestProceedAllGreen(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"ci / build-test (push)", "decode / decode (cuda) (pull_request)"},
		states: map[string]string{
			"ci / build-test (push)":                "success",
			"decode / decode (cuda) (pull_request)": "success",
		},
	}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionProceed {
		t.Fatalf("want proceed, got %s (%s)", r.Decision, r.Reason)
	}
	if r.Envelope == nil || r.Envelope.HeadSHA != "abc123" || r.Envelope.PR != 1566 {
		t.Fatalf("bad envelope: %+v", r.Envelope)
	}
	if len(r.Envelope.GreenContexts) != 2 {
		t.Fatalf("green contexts not carried: %+v", r.Envelope.GreenContexts)
	}
	if r.NewArmedSha != "" {
		t.Fatal("proceed must consume armed state")
	}
}

func TestWaitingPending(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"a", "b"},
		states:   map[string]string{"a": "success", "b": "pending"},
	}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || !strings.Contains(r.Reason, "b") {
		t.Fatalf("want waiting(b), got %s (%s)", r.Decision, r.Reason)
	}
	if r.NewArmedSha != "abc123" {
		t.Fatal("waiting must stay armed")
	}
}

func TestWaitingRedStaysArmed(t *testing.T) {
	// Q9.1: red CI is a silent non-event — no verdict, stays armed.
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"a"},
		states:   map[string]string{"a": "failure"},
	}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || !strings.Contains(r.Reason, "red") {
		t.Fatalf("want waiting(red), got %s (%s)", r.Decision, r.Reason)
	}
	if r.NewArmedSha != "abc123" {
		t.Fatal("red must stay armed for the next push")
	}
}

func TestWaitingMissingContext(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"a", "never-started"},
		states:   map[string]string{"a": "success"},
	}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || !strings.Contains(r.Reason, "never-started") {
		t.Fatalf("want waiting(missing), got %s (%s)", r.Decision, r.Reason)
	}
}

func TestStanddownLabelAbsent(t *testing.T) {
	api := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"}}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionStanddown || r.NewArmedSha != "" {
		t.Fatalf("want standdown+disarm, got %s", r.Decision)
	}
}

func TestStanddownClosed(t *testing.T) {
	api := &fakeAPI{pr: &PullRequest{State: "closed", HeadSHA: "x", Base: "main", Labels: []string{"needs-review"}}}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionStanddown {
		t.Fatalf("want standdown, got %s", r.Decision)
	}
	if r.NewArmedSha != "" {
		t.Fatal("closed must disarm")
	}
}

func TestDisarmHintBeatsFetch(t *testing.T) {
	// action=closed arrives: stand down without trusting a possibly-404 PR.
	r := Evaluate(context.Background(), &fakeAPI{prErr: fmt.Errorf("404")}, base)
	base2 := base
	base2.DisarmHint = true
	r = Evaluate(context.Background(), &fakeAPI{prErr: fmt.Errorf("404")}, base2)
	if r.Decision != DecisionStanddown {
		t.Fatalf("want standdown on disarm hint, got %s (%s)", r.Decision, r.Reason)
	}
}

func TestHorizonExceeded(t *testing.T) {
	p := base
	p.ArmedSha = "abc123"
	p.ArmedAt = base.Now.Add(-7 * time.Hour) // armed 7h ago, horizon 6h
	api := &fakeAPI{pr: openPR("needs-review"), required: []string{"a"}, states: map[string]string{"a": "pending"}}
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionStanddown || !strings.Contains(r.Reason, "horizon") {
		t.Fatalf("want standdown(horizon), got %s (%s)", r.Decision, r.Reason)
	}
}

func TestHeadMoveRearmsClock(t *testing.T) {
	// Armed 7h at old sha, head moved → horizon restarts, not expires.
	p := base
	p.ArmedSha = "oldsha"
	p.ArmedAt = base.Now.Add(-7 * time.Hour)
	api := &fakeAPI{pr: openPR("needs-review"), required: []string{"a"}, states: map[string]string{"a": "pending"}}
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionWaiting {
		t.Fatalf("want waiting after re-arm, got %s (%s)", r.Decision, r.Reason)
	}
	if !r.NewArmedAt.Equal(base.Now) {
		t.Fatalf("re-arm must reset armedAt, got %v", r.NewArmedAt)
	}
}

func TestNoProtectionProceedsOnLabel(t *testing.T) {
	api := &fakeAPI{pr: openPR("needs-review"), required: nil}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionProceed {
		t.Fatalf("want proceed (no required contexts), got %s (%s)", r.Decision, r.Reason)
	}
}

func TestTransientAPIFailureStaysArmed(t *testing.T) {
	p := base
	p.ArmedSha = "abc123"
	p.ArmedAt = base.Now
	r := Evaluate(context.Background(), &fakeAPI{prErr: fmt.Errorf("boom")}, p)
	if r.Decision != DecisionWaiting || r.NewArmedSha != "abc123" {
		t.Fatalf("transient failure must wait+keep armed, got %s", r.Decision)
	}
}

// ---------------------------------------------------------------------------
// REST shape tests against httptest servers (GitHub + Forgejo payloads)
// ---------------------------------------------------------------------------

func TestRESTForgejoShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/api/v1/repos/tibrez/rhesadox/pulls/1566":
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open",
				"head":  map[string]string{"sha": "abc123"},
				"base":  map[string]string{"ref": "main"},
				"labels": []map[string]string{
					{"name": "needs-review"}, {"name": "full-pipeline"},
				},
			})
		case "/api/v1/repos/tibrez/rhesadox/branch_protections/main":
			json.NewEncoder(w).Encode(map[string]any{
				"status_check_contexts": []string{"ci / build-test (push)", "decode / decode (cuda) (pull_request)"},
			})
		case "/api/v1/repos/tibrez/rhesadox/commits/abc123/statuses":
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "state": "success"},
				{"context": "decode / decode (cuda) (pull_request)", "state": "pending"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	api := &RESTAPI{Client: srv.Client(), TokenLookup: func(string) string { return "tok" }, BaseOverride: srv.URL + "/api/v1"}
	pr, err := api.GetPullRequest(context.Background(), "git.rezus.cloud/tibrez/rhesadox", 1566)
	if err != nil || pr.State != "open" || pr.HeadSHA != "abc123" || pr.Base != "main" || len(pr.Labels) != 2 {
		t.Fatalf("GetPullRequest: %+v err=%v", pr, err)
	}
	reqCtx, err := api.RequiredContexts(context.Background(), "git.rezus.cloud/tibrez/rhesadox", "main")
	if err != nil || len(reqCtx) != 2 || reqCtx[0] != "ci / build-test (push)" {
		t.Fatalf("RequiredContexts: %+v err=%v", reqCtx, err)
	}
	states, err := api.ContextStates(context.Background(), "git.rezus.cloud/tibrez/rhesadox", "abc123")
	if err != nil || states["decode / decode (cuda) (pull_request)"] != "pending" || states["ci / build-test (push)"] != "success" {
		t.Fatalf("ContextStates: %+v err=%v", states, err)
	}

	// End-to-end through the gate: pending decode -> waiting, armed at head.
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || r.NewArmedSha != "abc123" {
		t.Fatalf("gate: want waiting@abc123, got %s (%s) sha=%s", r.Decision, r.Reason, r.NewArmedSha)
	}
}

func TestRESTGitHubShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/repos/tibrezus/harmostes/pulls/10":
			json.NewEncoder(w).Encode(map[string]any{
				"state":  "open",
				"head":   map[string]string{"sha": "def456"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case req.URL.Path == "/repos/tibrezus/harmostes/branches/main/protection":
			json.NewEncoder(w).Encode(map[string]any{
				"required_status_checks": map[string]any{"contexts": []string{"build / test", "lint"}},
			})
		case req.URL.Path == "/repos/tibrezus/harmostes/commits/def456/status":
			json.NewEncoder(w).Encode(map[string]any{
				"statuses": []map[string]string{{"context": "build / test", "state": "success"}},
			})
		case req.URL.Path == "/repos/tibrezus/harmostes/commits/def456/check-runs":
			json.NewEncoder(w).Encode(map[string]any{
				"check_runs": []map[string]string{{"name": "lint", "status": "completed", "conclusion": "success"}},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	api := &RESTAPI{Client: srv.Client(), TokenLookup: func(string) string { return "tok" }, BaseOverride: srv.URL}
	p := base
	p.Repo = "github.com/tibrezus/harmostes"
	p.PR = 10
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionProceed {
		t.Fatalf("gate: want proceed, got %s (%s)", r.Decision, r.Reason)
	}
	// Required context satisfied by a CHECK-RUN (lint), not a status.
	if len(r.Envelope.GreenContexts) != 2 {
		t.Fatalf("green via check-runs not merged: %+v", r.Envelope.GreenContexts)
	}
}

func TestRESTGitHubNoProtection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/o/r/branches/main/protection":
			// GitHub returns 404 when no protection is configured.
			http.NotFound(w, req)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	api := &RESTAPI{Client: srv.Client(), BaseOverride: srv.URL}
	ctxs, err := api.RequiredContexts(context.Background(), "o/r", "main")
	if err != nil || len(ctxs) != 0 {
		t.Fatalf("404 protection must mean no required contexts: %v %v", ctxs, err)
	}
}

func TestNormalizeCheckRun(t *testing.T) {
	cases := []struct {
		status, conclusion, want string
	}{
		{"completed", "success", "success"},
		{"completed", "failure", "failure"},
		{"completed", "cancelled", "failure"},
		{"completed", "timed_out", "failure"},
		{"in_progress", "", "pending"},
		{"queued", "", "pending"},
		{"completed", "skipped", "pending"},
		{"completed", "neutral", "pending"},
		{"", "", "pending"},
	}
	for _, c := range cases {
		if got := normalizeCheckRun(c.status, c.conclusion); got != c.want {
			t.Errorf("normalizeCheckRun(%q,%q)=%q want %q", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestResolveHost(t *testing.T) {
	cases := []struct {
		in, base, kind string
	}{
		{"github.com/tibrezus/harmostes", "https://api.github.com", "github"},
		{"tibrezus/harmostes", "https://api.github.com", "github"},
		{"git.rezus.cloud/tibrez/rhesadox", "https://git.rezus.cloud/api/v1", "forgejo"},
		{"codeberg.org/x/y", "https://codeberg.org/api/v1", "forgejo"},
		{"git.example.com/o/r", "https://git.example.com/api/v1", "forgejo"},
	}
	for _, c := range cases {
		h, err := ResolveHost(c.in)
		if err != nil || string(h.Kind) != c.kind || h.APIBase != c.base {
			t.Errorf("ResolveHost(%q) = %+v err=%v want base=%s kind=%s", c.in, h, err, c.base, c.kind)
		}
	}
	if _, err := ResolveHost("bad"); err == nil {
		t.Error("ResolveHost must reject non-repo paths")
	}
}
