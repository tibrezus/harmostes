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
	pr           *PullRequest
	prErr        error
	labeledPulls []PullRequest
	required     []string
	reqErr       error
	states       map[string]string
	ctxErr       error
	comments     []fakeComment
	commentsErr  error
}

// fakeComment pairs an IssueComment with its host-side updated_at (the
// field the real API filters on via `since`).
type fakeComment struct {
	IssueComment
	updatedAt time.Time
}

func (f *fakeAPI) GetPullRequest(ctx context.Context, repo string, n int) (*PullRequest, error) {
	return f.pr, f.prErr
}

func (f *fakeAPI) ListLabeledOpenPulls(_ context.Context, _, _ string) ([]PullRequest, error) {
	return f.labeledPulls, nil
}
func (f *fakeAPI) RequiredContexts(ctx context.Context, repo, branch string) ([]string, error) {
	return f.required, f.reqErr
}
func (f *fakeAPI) ContextStates(ctx context.Context, repo, sha string) (map[string]string, error) {
	return f.states, f.ctxErr
}
func (f *fakeAPI) ListComments(ctx context.Context, repo string, n int, since time.Time) ([]IssueComment, error) {
	if f.commentsErr != nil {
		return nil, f.commentsErr
	}
	// Emulate the hosts' `since` filter (updated_at ≥ since) so the window
	// semantics are exercised, not just the scan.
	var out []IssueComment
	for _, c := range f.comments {
		if !c.updatedAt.IsZero() && c.updatedAt.Before(since) {
			continue
		}
		out = append(out, c.IssueComment)
	}
	return out, nil
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
	// Proceed stays armed at the dispatched head (#250 r4): the review runs
	// async; sweeps re-check the verdict window instead of re-dispatching.
	// Consume (verdict posted) clears the slot.
	if r.NewArmedSha != "abc123" {
		t.Fatalf("proceed must stay armed at the head, got %q", r.NewArmedSha)
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
	// The consumed case: the deploy plugin removed the label AFTER posting
	// the verdict — the trailer is the durable consume signal.
	api := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"},
		comments: []fakeComment{{IssueComment: IssueComment{Body: "## Adversarial Review\n…\n<!-- pr-review: APPROVE @ abc123 -->"}, updatedAt: base.Now}}}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionStanddown || r.NewArmedSha != "" {
		t.Fatalf("want standdown+disarm, got %s", r.Decision)
	}
}

func TestLabelAbsentNoVerdictStaysArmed(t *testing.T) {
	// #237 live regression: an 'unlabeled' wake racing a newer 'labeled'
	// arm (label rm+add cycles) or a wake lost to a host outage observes
	// label-absent at a stale instant while the request is still pending.
	// Disarming here lost the review permanently (rhesadox #1635: gate
	// stood down "label absent" with the label present and CI green).
	api := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"}}
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || r.NewArmedSha != "abc123" {
		t.Fatalf("want waiting+armed@abc123, got %s sha=%s (%s)", r.Decision, r.NewArmedSha, r.Reason)
	}
	// Already-armed clock is preserved (the horizon keeps ticking from the
	// original arm, not restarted by the ambiguity).
	p := base
	p.ArmedSha = "abc123"
	p.ArmedAt = base.Now.Add(-1 * time.Hour)
	r = Evaluate(context.Background(), api, p)
	if r.Decision != DecisionWaiting || !r.NewArmedAt.Equal(p.ArmedAt) {
		t.Fatalf("want waiting with preserved ArmedAt, got %s at=%v", r.Decision, r.NewArmedAt)
	}
}

func TestLabelAbsentNoVerdictHorizonStandsDown(t *testing.T) {
	// The stay-armed recovery is bounded: past the horizon the gate stands
	// down instead of waiting forever on a label that never returns.
	api := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"}}
	p := base
	p.ArmedSha = "abc123"
	p.ArmedAt = base.Now.Add(-7 * time.Hour) // horizon is 6h
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionStanddown || r.NewArmedSha != "" {
		t.Fatalf("want standdown past horizon, got %s sha=%s", r.Decision, r.NewArmedSha)
	}
}

func TestVerdictWindowFreshConsumeOnly(t *testing.T) {
	// #238 review MAJOR: the verdict scan is TIME-WINDOWED. An OLD verdict
	// (posted before this arm — a prior review cycle on the same PR) must
	// NOT consume a fresh request; the current cycle's verdict (inside the
	// window) must. Ordering (oldest-first hosts) is irrelevant by
	// construction — the window, not the page, decides.
	old := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"},
		comments: []fakeComment{{IssueComment: IssueComment{Body: "<!-- pr-review: APPROVE @ dead000 -->"}, updatedAt: base.Now.Add(-2 * time.Hour)}}}
	r := Evaluate(context.Background(), old, base)
	if r.Decision != DecisionWaiting || r.NewArmedSha != "abc123" {
		t.Fatalf("old verdict must not consume: got %s sha=%s (%s)", r.Decision, r.NewArmedSha, r.Reason)
	}
	fresh := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"},
		comments: []fakeComment{{IssueComment: IssueComment{Body: "old noise"}, updatedAt: base.Now.Add(-2 * time.Hour)},
			{IssueComment: IssueComment{Body: "<!-- pr-review: COMMENT @ abc123 -->"}, updatedAt: base.Now.Add(-1 * time.Minute)}}}
	r = Evaluate(context.Background(), fresh, base)
	if r.Decision != DecisionStanddown || r.NewArmedSha != "" {
		t.Fatalf("fresh verdict must consume: got %s sha=%s", r.Decision, r.NewArmedSha)
	}
}

func TestLabelAbsentHeadMovedDuringAmbiguityResetsHorizon(t *testing.T) {
	// #238 review MINOR: the ambiguity branch now matches the label-present
	// head-moved path — a head that moved during the window inherits a fresh
	// horizon clock, not the stale armed clock.
	api := &fakeAPI{pr: &PullRequest{State: "open", HeadSHA: "newsha", Base: "main", Labels: []string{"other"}}}
	p := base
	p.ArmedSha = "abc123"
	p.ArmedAt = base.Now.Add(-5 * time.Hour) // near the 6h horizon
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionWaiting || !r.NewArmedAt.Equal(p.Now) {
		t.Fatalf("head move during ambiguity must reset the clock: got %s at=%v", r.Decision, r.NewArmedAt)
	}
}

func TestLabelAbsentVerdictCheckFailsStaysArmed(t *testing.T) {
	// Transient comments-fetch failure: keep armed (retry next cycle),
	// never disarm on an API hiccup.
	api := &fakeAPI{pr: openPR("full-pipeline"), required: []string{"a"}, states: map[string]string{"a": "success"},
		commentsErr: fmt.Errorf("HTTP 503")}
	p := base
	p.ArmedSha = "def456" // previously armed at an older head
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionWaiting || r.NewArmedSha != "def456" {
		t.Fatalf("want waiting keeping armed sha, got %s sha=%s", r.Decision, r.NewArmedSha)
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
	p := base
	p.DisarmHint = true
	r := Evaluate(context.Background(), &fakeAPI{prErr: fmt.Errorf("404")}, p)
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

func TestFreshWakeTransientFailureArms(t *testing.T) {
	// Round-3 MAJOR: a fresh wake (no prior armed state) whose first PR
	// fetch fails must ARM at the wake SHA — an empty NewArmedSha would
	// write the disarm branch and silently lose the review.
	p := base
	p.ArmedSha = ""
	p.ArmedAt = time.Time{}
	p.WakeSHA = "wake123"
	r := Evaluate(context.Background(), &fakeAPI{prErr: fmt.Errorf("boom")}, p)
	if r.Decision != DecisionWaiting {
		t.Fatalf("want waiting, got %s (%s)", r.Decision, r.Reason)
	}
	if r.NewArmedSha != "wake123" {
		t.Fatalf("fresh-wake transient failure must arm at wake SHA, got %q", r.NewArmedSha)
	}
	if r.NewArmedAt.IsZero() {
		t.Fatal("fresh-wake arming must stamp armedAt")
	}
}

func Test403ProtectionMeansNoReadableContexts(t *testing.T) {
	// Round-3 MAJOR: 403 (token cannot read protection) must behave like
	// no-protection — proceed on label alone — not block until horizon.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/pulls/1"):
			json.NewEncoder(w).Encode(map[string]any{
				"state": "open", "head": map[string]string{"sha": "s1"},
				"base":   map[string]string{"ref": "main"},
				"labels": []map[string]string{{"name": "needs-review"}},
			})
		case strings.Contains(req.URL.Path, "/protection"):
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	api := &RESTAPI{Client: srv.Client(), BaseOverride: srv.URL}
	ctxs, err := api.RequiredContexts(context.Background(), "github.com/o/r", "main")
	if err != nil || len(ctxs) != 0 {
		t.Fatalf("403 protection must mean no readable contexts, got %v err=%v", ctxs, err)
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

func TestRESTListCommentsShapes(t *testing.T) {
	// Both host kinds expose issue comments at the same path shape; the
	// gate scans them for the verdict trailer (the consume signal, #237).
	for _, tc := range []struct {
		name, repo, path string
		number           int
	}{
		{"forgejo", "git.rezus.cloud/tibrez/rhesadox", "/api/v1/repos/tibrez/rhesadox/issues/1566/comments", 1566},
		{"github", "tibrezus/harmostes", "/repos/tibrezus/harmostes/issues/237/comments", 237},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != tc.path {
					http.NotFound(w, req)
					return
				}
				// The request MUST carry the since window — without it the
				// hosts' oldest-first, page-1-only response hides a fresh
				// verdict behind 100 stale comments on a busy PR (#238
				// review MAJOR).
				sinceQ := req.URL.Query().Get("since")
				if sinceQ == "" {
					t.Errorf("request missing since param: %s", req.URL.RawQuery)
				}
				if _, err := time.Parse(time.RFC3339, sinceQ); err != nil {
					t.Errorf("since not RFC3339: %q", sinceQ)
				}
				// Server-side `since` filter (updated_at ≥ since) over a
				// >100-comment PR: 140 stale, 10 fresh — the verdict rides
				// LAST in the filtered response, the position page-1-only
				// fetching would never reach without the window.
				since, _ := time.Parse(time.RFC3339, sinceQ)
				out := []map[string]string{}
				for i := 0; i < 140; i++ {
					out = append(out, map[string]string{"body": fmt.Sprintf("stale comment %d", i)})
				}
				for i := 0; i < 9; i++ {
					out = append(out, map[string]string{"body": fmt.Sprintf("fresh comment %d", i)})
				}
				out = append(out, map[string]string{"body": "review…\n<!-- pr-review: COMMENT @ abc123 -->"})
				_ = since
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(out)
			}))
			defer srv.Close()
			api := &RESTAPI{Client: srv.Client()}
			// BaseOverride must be the API base; derive it per host kind.
			if tc.name == "forgejo" {
				api.BaseOverride = srv.URL + "/api/v1"
			} else {
				api.BaseOverride = srv.URL
			}
			cs, err := api.ListComments(context.Background(), tc.repo, tc.number, time.Now().Add(-time.Hour))
			if err != nil || len(cs) != 150 || !hasVerdict(cs) {
				t.Fatalf("ListComments: %d comments, verdict=%v err=%v", len(cs), hasVerdict(cs), err)
			}
		})
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
		// skipped/neutral SATISFY (host merge-rule parity — round-4 MAJOR)
		{"completed", "skipped", "success"},
		{"completed", "neutral", "success"},
		{"completed", "stale", "pending"},
		{"", "", "pending"},
	}
	for _, c := range cases {
		if got := normalizeCheckRun(c.status, c.conclusion); got != c.want {
			t.Errorf("normalizeCheckRun(%q,%q)=%q want %q", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestContextStatesNewestFirstWins(t *testing.T) {
	// Live regression (rhesadox #1566): Forgejo lists the fresh attempt
	// FIRST, superseded pendings below. Last-wins read the head as pending
	// forever; first-wins reads it green.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "decode / decode (cuda) (pull_request)", "state": "success"}, // newest
				{"context": "decode / decode (cuda) (pull_request)", "state": "pending"}, // superseded
				{"context": "decode / decode (cuda) (pull_request)", "state": "pending"}, // superseded
				{"context": "ci / build-test (push)", "state": "success"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	api := &RESTAPI{Client: srv.Client(), BaseOverride: srv.URL + "/api/v1"}
	states, err := api.ContextStates(context.Background(), "git.rezus.cloud/o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if states["decode / decode (cuda) (pull_request)"] != "success" {
		t.Fatalf("newest entry must win: %+v", states)
	}
	if states["ci / build-test (push)"] != "success" {
		t.Fatalf("single entries unaffected: %+v", states)
	}
}

func TestForgejoStatusFieldParsed(t *testing.T) {
	// Live regression (rhesadox #1566): Forgejo's status objects carry the
	// field `status`, GitHub's carry `state`. Parsing only `state` read
	// every Forgejo context as failure ("") → all-green head judged red.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(req.URL.Path, "/statuses"):
			json.NewEncoder(w).Encode([]map[string]string{
				{"context": "ci / build-test (push)", "status": "success"},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	api := &RESTAPI{Client: srv.Client(), BaseOverride: srv.URL + "/api/v1"}
	states, err := api.ContextStates(context.Background(), "git.rezus.cloud/o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if states["ci / build-test (push)"] != "success" {
		t.Fatalf("Forgejo `status` field must parse: %+v", states)
	}
}

func TestSkippedStatusSatisfies(t *testing.T) {
	// Live regression: a label event re-ran a guarded workflow whose legs
	// skipped; Forgejo posted `skipped` statuses; the PR is mergeable=True
	// (host rules: skipped satisfies) but the gate read failure and held
	// armed on red forever.
	if got := normalizeStatusState("skipped"); got != "success" {
		t.Fatalf(`normalizeStatusState("skipped")=%q, want success`, got)
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

// ---------------------------------------------------------------------------
// Dispatch liveness (#248): a dispatch marker older than DispatchTimeout
// with no verdict is provably dead — the consumer wraps every one-shot run
// in a 30m context, so no live run can outlive the bound. The gate stands
// down (slot + marker released); the backlog pass re-arms the still-labeled
// PR on the next sweep. The verdict scan runs FIRST: a late verdict
// consumes even past the timeout (it outranks the dead presumption).
// ---------------------------------------------------------------------------

func TestDispatchedInFlightWithinTimeoutWaits(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"ci"},
		states:   map[string]string{"ci": "success"},
	}
	p := base
	p.DispatchedAt = p.Now.Add(-10 * time.Minute) // fresh dispatch
	p.DispatchTimeout = 45 * time.Minute
	res := Evaluate(context.Background(), api, p)
	if res.Decision != DecisionWaiting || !strings.Contains(res.Reason, "in flight") {
		t.Fatalf("fresh dispatch must wait in flight, got %s: %s", res.Decision, res.Reason)
	}
	if res.NewArmedSha == "" {
		t.Fatal("in-flight waiting must stay armed")
	}
}

func TestDispatchedDeadPresumedStandsDown(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"ci"},
		states:   map[string]string{"ci": "success"},
	}
	p := base
	p.ArmedSha = "abc123" // armed long enough that horizon is not the trigger
	p.DispatchedAt = p.Now.Add(-50 * time.Minute)
	p.DispatchTimeout = 45 * time.Minute
	res := Evaluate(context.Background(), api, p)
	if res.Decision != DecisionStanddown {
		t.Fatalf("stale dispatch must stand down, got %s: %s", res.Decision, res.Reason)
	}
	if !strings.Contains(res.Reason, "presumed dead") {
		t.Fatalf("reason must name the dead-dispatch presumption, got %q", res.Reason)
	}
	if res.NewArmedSha != "" {
		t.Fatal("presumed-dead standdown must release the slot (marker goes with it)")
	}
}

func TestDispatchedTimeoutBoundaryStandsDown(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"ci"},
		states:   map[string]string{"ci": "success"},
	}
	p := base
	p.ArmedSha = "abc123"
	p.DispatchedAt = p.Now.Add(-45 * time.Minute)
	p.DispatchTimeout = 45 * time.Minute
	res := Evaluate(context.Background(), api, p)
	if res.Decision != DecisionStanddown || !strings.Contains(res.Reason, "presumed dead") {
		t.Fatalf("at-boundary dispatch is dead (>= convention), got %s: %s", res.Decision, res.Reason)
	}
}

func TestDispatchedVerdictBeatsTimeout(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR(), // label already removed by the deploy plugin
		required: []string{"ci"},
		states:   map[string]string{"ci": "success"},
		comments: []fakeComment{{
			IssueComment: IssueComment{Body: "verdict\n<!-- pr-review: APPROVE @ abc123 -->"},
			updatedAt:    base.Now.Add(-5 * time.Minute),
		}},
	}
	p := base
	p.ArmedSha = "abc123"
	p.DispatchedAt = p.Now.Add(-50 * time.Minute) // way past the timeout
	p.DispatchTimeout = 45 * time.Minute
	res := Evaluate(context.Background(), api, p)
	if res.Decision != DecisionStanddown || !strings.Contains(res.Reason, "verdict posted — consumed") {
		t.Fatalf("a late verdict consumes even past the timeout, got %s: %s", res.Decision, res.Reason)
	}
}

func TestDispatchedZeroTimeoutKeepsWaiting(t *testing.T) {
	api := &fakeAPI{
		pr:       openPR("needs-review"),
		required: []string{"ci"},
		states:   map[string]string{"ci": "success"},
	}
	p := base
	p.ArmedSha = "abc123"
	p.DispatchedAt = p.Now.Add(-3 * time.Hour) // ancient marker
	p.DispatchTimeout = 0                      // unconfigured caller: no liveness bound
	res := Evaluate(context.Background(), api, p)
	if res.Decision != DecisionWaiting || !strings.Contains(res.Reason, "in flight") {
		t.Fatalf("zero DispatchTimeout must keep waiting (horizon remains the bound), got %s: %s", res.Decision, res.Reason)
	}
}
