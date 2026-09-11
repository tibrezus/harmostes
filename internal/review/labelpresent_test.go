package review

import (
	"context"
	"errors"
	"testing"
	"time"
)

// #423 r2 P9: Result.LabelPresent is hand-threaded through withPresence —
// the one field whose wrongness (silently false) re-unreachable's the
// gate's Forgejo override AND passes CI (false is fail-closed). This table
// pins it per DECISION PATH, so a future return added to Evaluate without
// the closure goes red here instead of in production.
func TestEvaluateLabelPresentPerDecisionPath(t *testing.T) {
	now := time.Now()
	base := Params{
		Repo: "git.rezus.cloud/tibrez/rhesadox", PR: 99, Label: "needs-review",
		Horizon: time.Hour, DispatchTimeout: 45 * time.Minute,
		WakeSHA: "deadbeef123", Now: now,
	}
	labeled := &PullRequest{State: "open", HeadSHA: "deadbeef123", Base: "main",
		Labels: []string{"needs-review"}}
	unlabeled := &PullRequest{State: "open", HeadSHA: "deadbeef123", Base: "main",
		Labels: []string{"full-pipeline"}}

	cases := []struct {
		name     string
		pr       *PullRequest
		prE      error
		reqErr   error
		req      []string
		states   map[string]string
		comments []IssueComment
		// Expectations:
		wantDecision Decision
		wantPresent  bool
		wantKnown    bool // false = pre-fetch exit (presence unknown → zero)
	}{
		{
			name: "proceed (label + green)", pr: labeled, req: []string{"ci"},
			states:       map[string]string{"ci": "success"},
			wantDecision: DecisionProceed, wantPresent: true, wantKnown: true,
		},
		{
			name: "waiting CI red (label present)", pr: labeled, req: []string{"ci"},
			states:       map[string]string{"ci": "failure"},
			wantDecision: DecisionWaiting, wantPresent: true, wantKnown: true,
		},
		{
			name: "waiting CI pending (label present)", pr: labeled, req: []string{"ci"},
			states:       map[string]string{"ci": "pending"},
			wantDecision: DecisionWaiting, wantPresent: true, wantKnown: true,
		},
		{
			name: "waiting merge-rules fetch failed (label present)", pr: labeled, reqErr: errors.New("boom"),
			wantDecision: DecisionWaiting, wantPresent: true, wantKnown: true,
		},
		{
			// The #1635 stay-armed ambiguity — the path the Waiting-arm
			// override resolution exists to survive: label ABSENT but the
			// decision is WAITING. LabelPresent false is the fact that keeps
			// a granular wake from resetting the breaker here.
			name: "waiting label-absent-no-verdict (#1635)", pr: unlabeled,
			comments:     []IssueComment{},
			wantDecision: DecisionWaiting, wantPresent: false, wantKnown: true,
		},
		{
			name: "standdown label-absent-with-verdict (consumed)", pr: unlabeled,
			comments:     []IssueComment{{Body: "done <!-- pr-review: APPROVE @ deadbeef123 -->"}},
			wantDecision: DecisionStanddown, wantPresent: false, wantKnown: true,
		},
		{
			name: "standdown PR closed", pr: &PullRequest{State: "closed", HeadSHA: "deadbeef123", Labels: []string{"needs-review"}},
			wantDecision: DecisionStanddown, wantPresent: true, wantKnown: true,
		},
		{
			name: "waiting pr fetch failed (presence UNKNOWN — zero value)", prE: errors.New("boom"),
			wantDecision: DecisionWaiting, wantPresent: false, wantKnown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{pr: tc.pr, prErr: tc.prE, reqErr: tc.reqErr, required: tc.req, states: tc.states}
			for _, c := range tc.comments {
				api.comments = append(api.comments, fakeComment{IssueComment: c, updatedAt: now.Add(-time.Minute)})
			}
			p := base
			if tc.pr != nil && tc.pr.State == "open" && len(tc.comments) == 0 && tc.name == "waiting label-absent-no-verdict (#1635)" {
				// armed state needed for the ambiguity window to wait rather than arm fresh
				p.ArmedSha = "deadbeef123"
				p.ArmedAt = now
			}
			res := Evaluate(context.Background(), api, p)
			if res.Decision != tc.wantDecision {
				t.Fatalf("decision = %s (%s), want %s", res.Decision, res.Reason, tc.wantDecision)
			}
			if tc.wantKnown && res.LabelPresent != tc.wantPresent {
				t.Fatalf("LabelPresent = %v, want %v (%s)", res.LabelPresent, tc.wantPresent, res.Reason)
			}
			if !tc.wantKnown && res.LabelPresent {
				t.Fatalf("pre-fetch exit must leave LabelPresent unknown (false), got true (%s)", res.Reason)
			}
		})
	}
}
