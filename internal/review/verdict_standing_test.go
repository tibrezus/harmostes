package review

// The standing-verdict refusal (#567): a head is reviewed exactly once.
// A verdict trailer at the CURRENT head must turn any re-arm into a
// terminal refusal (the rhesadox#2359 class — six identical reviews of
// one head), while verdicts at other heads never block and in-flight
// reviews stay untouched.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStandingVerdictAt(t *testing.T) {
	head := strings.Repeat("deadbeef", 5) // 40 hex — the contract ceiling
	comments := []IssueComment{
		{Body: "unrelated chatter quoting <!-- pr-review: APPROVE @ other --> prose"},
		{Body: "REQUEST_CHANGES at head — 1 blocking finding\n\n<!-- pr-review: REQUEST_CHANGES @ deadbeefdeadbeef -->"},
		{Body: "older round: <!-- pr-review: APPROVE @ caafeedcaafeedcaafeedcaafeedcaafeedcaafeed -->"},
	}
	if d, ok := standingVerdictAt(comments, head); !ok || d != "REQUEST_CHANGES" {
		t.Fatalf("abbreviated trailer (16 hex, a true prefix of the head) must match, got %q ok=%v", d, ok)
	}
	if _, ok := standingVerdictAt(comments, "beefcafe"); ok {
		t.Fatalf("an unrelated head must not match any trailer")
	}
	if _, ok := standingVerdictAt(comments, strings.Repeat("12345678", 5)); ok {
		t.Fatalf("an unrelated head must not match any trailer")
	}
	full := []IssueComment{{Body: "<!-- pr-review: APPROVE @ " + strings.Repeat("deadbeef", 5) + " -->"}}
	if d, ok := standingVerdictAt(full, head); !ok || d != "APPROVE" {
		t.Fatalf("full-length trailer must match the full head, got %q ok=%v", d, ok)
	}
	upper := []IssueComment{{Body: "<!-- pr-review: APPROVE @ " + strings.Repeat("DEADBEEF", 5) + " -->"}}
	if _, ok := standingVerdictAt(upper, head); ok {
		t.Fatalf("uppercase sha violates the lowercase-only trailer contract — must not match")
	}
	if _, ok := standingVerdictAt(nil, head); ok {
		t.Fatalf("no comments → no standing verdict")
	}
}

// verdictStubAPI = the all-green proceed world + one conversation comment.
// The head is a realistic 10-hex sha so trailer abbreviation semantics are
// exercised against the contract's 7-hex floor.
func verdictStubAPI(body string) *fakeAPI {
	pr := openPR("needs-review")
	pr.HeadSHA = "abcdef1234"
	return &fakeAPI{
		pr:       pr,
		required: []string{"ci / build-test (push)"},
		states:   map[string]string{"ci / build-test (push)": "success"},
		comments: []fakeComment{{IssueComment: IssueComment{Body: body}}},
	}
}

func TestVerdictStandingRefusesRecheck(t *testing.T) {
	api := verdictStubAPI("verdict\n\n<!-- pr-review: REQUEST_CHANGES @ abcdef1 -->")
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionStanddown {
		t.Fatalf("re-arm at a reviewed head must refuse, got %s (%s)", r.Decision, r.Reason)
	}
	if r.Code != CodeVerdictStanding {
		t.Fatalf("code = %q, want verdict-standing", r.Code)
	}
	if r.Envelope == nil || r.Envelope.HeadSHA != "abcdef1234" {
		t.Fatalf("refusal must carry the refused head for memoisation, got %+v", r.Envelope)
	}
	for _, want := range []string{"exactly once", "push a fix commit", "fj review reply", "fj review resolve"} {
		if !strings.Contains(r.Reason, want) {
			t.Fatalf("refusal directive must teach the close-out (%q missing): %s", want, r.Reason)
		}
	}
}

func TestVerdictStandingApproveAlsoBlocks(t *testing.T) {
	// An APPROVE at head is merge currency — re-reviewing the same diff is
	// the same deterministic waste. The refusal text says so.
	r := Evaluate(context.Background(), verdictStubAPI("ok\n\n<!-- pr-review: APPROVE @ abcdef1234 -->"), base)
	if r.Decision != DecisionStanddown || r.Code != CodeVerdictStanding {
		t.Fatalf("APPROVE at head must also refuse, got %s/%s", r.Decision, r.Code)
	}
}

func TestVerdictStandingOtherShaDoesNotBlock(t *testing.T) {
	// The verdict is at an OLDER head: this is a fresh push re-arming — the
	// normal flow. No required contexts needed: even the label-alone path
	// must proceed past the check.
	api := verdictStubAPI("old round\n\n<!-- pr-review: REQUEST_CHANGES @ 0000000 -->")
	api.required = nil
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionProceed {
		t.Fatalf("verdict at another sha must not block the new head, got %s (%s)", r.Decision, r.Reason)
	}
}

func TestVerdictStandingFailureStaysArmed(t *testing.T) {
	// A failed verdict scan is infrastructure weather: wait and retry the
	// whole evaluation next sweep (same polarity as the in-flight check).
	api := verdictStubAPI("x")
	api.commentsErr = context.DeadlineExceeded
	api.required = nil
	r := Evaluate(context.Background(), api, base)
	if r.Decision != DecisionWaiting || !strings.Contains(r.Reason, "verdict-standing check failed") {
		t.Fatalf("scan failure must wait, got %s (%s)", r.Decision, r.Reason)
	}
}

func TestInFlightUnaffectedByOldVerdict(t *testing.T) {
	// A running review is left alone to land: an OLD verdict at this head
	// (predating the flight) must not stand the claim down — the in-flight
	// window owns the decision, and its own verdict scan is window-scoped.
	p := base
	armed := p.Now.Add(-2 * time.Minute)
	p.ArmedAt = armed
	p.ArmedSha = "abc123"
	p.DispatchedAt = armed.Add(-1 * time.Minute)
	old := fakeComment{IssueComment: IssueComment{Body: "<!-- pr-review: APPROVE @ abcdef1234 -->"}, updatedAt: armed.Add(-24 * time.Hour)}
	api := &fakeAPI{pr: openPR("needs-review"), comments: []fakeComment{old}}
	r := Evaluate(context.Background(), api, p)
	if r.Decision != DecisionWaiting || !strings.Contains(r.Reason, "in flight") {
		t.Fatalf("old verdict must not disturb an in-flight review, got %s (%s)", r.Decision, r.Reason)
	}
}
