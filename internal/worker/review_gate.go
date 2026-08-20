package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/review"
)

// newReviewAPI is the seam the tests swap for a server-pinned API.
var newReviewAPI = func() review.API {
	return &review.RESTAPI{Client: http.DefaultClient, TokenLookup: os.Getenv}
}

// reviewGate evaluates the Review-Ready Gate (ADR-0006) when the workflow is
// woken by a pull_request event or is armed from a previous wake. It returns
// the Trigger Envelope when the gate proceeds, nil otherwise (waiting, stood
// down, or gate not configured / nothing armed).
//
// The gate's armed state lives on the Workflow status (persisted via the
// status patcher): unarmed workflows cost zero API calls; armed ones cost
// one PR fetch (+ protection/contexts while the label is present).
// RunReviewGate is the PRODUCTION seam: the one-shot worker calls it after
// template resolution and BEFORE graph execution (the pipeline.Run path is
// legacy — production has routed through graph.ExecuteGraph since #177).
// Returns the Trigger Envelope on proceed; nil otherwise (waiting, stood
// down, or gate not configured). The caller exits before provisioning when
// nil is returned for a gated workflow.
func RunReviewGate(ctx context.Context, status StatusPatcher, logFn func(string, ...any), wf *v1alpha1.Workflow) *review.Envelope {
	deps := Deps{Status: status, Log: logFn}
	return reviewGate(ctx, deps, wf)
}

func reviewGate(ctx context.Context, deps Deps, wf *v1alpha1.Workflow) *review.Envelope {
	rr := wf.Spec.ReviewReady
	if rr == nil {
		return nil // gate not configured for this workflow
	}

	armed := wf.Status.ReviewReady
	// The controller clears the trigger annotations at schedule time (it must,
	// or isDue rapid-fires), so the PR pointer rides the TriggerEvent payload
	// through the consumer into the one-shot worker's environment. The
	// annotation is the fallback for direct/manual invocations.
	trigPR := os.Getenv("HARMOSTES_TRIGGER_PR")
	if trigPR == "" {
		trigPR = wf.Annotations["harmostes.dev/trigger-pr"]
	}
	action := os.Getenv("HARMOSTES_TRIGGER_ACTION")
	if action == "" {
		action = wf.Annotations["harmostes.dev/trigger-action"]
	}

	// Nothing armed, no wake event: idle — the old path would have polled
	// every open PR of every repo; here we do nothing at all.
	if trigPR == "" && (armed == nil || armed.ArmedPR == 0) {
		return nil
	}

	var repo string
	var pr int
	if trigPR != "" {
		r, n, err := parsePRPointer(trigPR)
		if err != nil {
			deps.log()("review-ready: bad trigger annotation %q: %v", trigPR, err)
			return nil
		}
		repo, pr = r, n
	} else {
		repo, pr = armed.ArmedRepo, armed.ArmedPR
	}

	params := review.Params{
		Repo:       repo,
		PR:         pr,
		Label:      rr.EffectiveLabel(),
		Horizon:    rr.HorizonDuration(),
		DisarmHint: action == "closed",
		WakeSHA:    wakeRevision(wf),
	}
	if armed != nil {
		params.ArmedSha = armed.ArmedSha
		if armed.ArmedSince != nil {
			params.ArmedAt = armed.ArmedSince.Time
		}
	}
	// A wake event targeting a DIFFERENT PR than the armed one re-targets
	// the gate ONLY when the wake is request-shaped (a label was touched —
	// a human or the skill asked for something). A synchronize/push wake on
	// an unrelated PR must NOT hijack an armed review: observed live when a
	// push on #1577 re-targeted an armed #1566, whose "label absent"
	// standdown then disarmed it. The armed PR's OWN synchronize arrives as
	// its own wake and re-arms its head correctly.
	if armed != nil && trigPR != "" && armed.ArmedPR != 0 && (armed.ArmedRepo != repo || armed.ArmedPR != pr) {
		switch action {
		case "labeled", "unlabeled", "label_updated":
			// request-shaped: re-target (reset the armed head)
			params.ArmedSha = ""
			params.ArmedAt = time.Time{}
		default:
			// push-shaped on another PR: ignore — keep the armed review
			deps.log()("review-ready: wake for pr=%d (%s) while armed on pr=%d — ignored, armed review preserved", pr, action, armed.ArmedPR)
			return nil
		}
	}

	// Scope allowlist (defense-in-depth): spec.config.repos declares which
	// repos this instance serves. A signed webhook from an undeclared repo
	// (mis-pointed hook) stands down instead of arming.
	if !repoInScope(wf, repo) {
		deps.log()("review-ready: %s not in spec.config.repos — ignoring wake", repo)
		patchIdle(ctx, deps, wf.Name, "wake for out-of-scope repo "+repo)
		return nil
	}

	result := review.Evaluate(ctx, newReviewAPI(), params)

	// Persist the armed state (and the decision, for the UI).
	var since *time.Time
	if !result.NewArmedAt.IsZero() {
		t := result.NewArmedAt
		since = &t
	}
	if err := deps.Status.PatchStatus(ctx, wf.Name, func(s *v1alpha1.WorkflowStatus) {
		if result.NewArmedSha == "" {
			s.ReviewReady = &v1alpha1.ReviewReadyStatus{
				LastDecision: string(result.Decision),
				LastReason:   result.Reason,
			}
		} else {
			s.ReviewReady = &v1alpha1.ReviewReadyStatus{
				ArmedRepo:    repo,
				ArmedPR:      pr,
				ArmedSha:     result.NewArmedSha,
				ArmedSince:   metaTime(since),
				LastDecision: string(result.Decision),
				LastReason:   result.Reason,
			}
		}
	}); err != nil {
		deps.log()("review-ready: status patch failed: %v", err)
	}

	deps.log()("review-ready: %s pr=%d — %s", result.Decision, pr, result.Reason)
	if result.Decision == review.DecisionProceed {
		return result.Envelope
	}
	return nil
}

// wakeRevision returns the wake event's trigger-revision (env first — the
// controller clears annotations at schedule time — annotation fallback).
func wakeRevision(wf *v1alpha1.Workflow) string {
	if rev := os.Getenv("HARMOSTES_TRIGGER_REVISION"); rev != "" {
		return rev
	}
	return wf.Annotations["harmostes.dev/trigger-revision"]
}

// repoInScope reports whether repo matches the instance's configured repos.
// The config stores either "host/owner/name" or bare "owner/name" (GitHub);
// both forms must match the annotation's normalized "host/owner/name".
// An empty/missing config accepts nothing (fail closed).
func repoInScope(wf *v1alpha1.Workflow, repo string) bool {
	if len(wf.Spec.Config) == 0 {
		return false
	}
	var cfg struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(wf.Spec.Config, &cfg); err != nil {
		return false
	}
	for _, r := range cfg.Repos {
		if r == repo {
			return true
		}
		// bare 2-segment "owner/name" form: GitHub implied
		if len(strings.Split(r, "/")) == 2 && repo == "github.com/"+r {
			return true
		}
	}
	return false
}

func patchIdle(ctx context.Context, deps Deps, name, reason string) {
	// Preserve any armed state: an out-of-scope wake must not disarm a
	// legitimately armed in-scope PR (self-heals, but do not cause it).
	if err := deps.Status.PatchStatus(ctx, name, func(s *v1alpha1.WorkflowStatus) {
		if s.ReviewReady != nil {
			s.ReviewReady.LastDecision = "idle"
			s.ReviewReady.LastReason = reason
			return
		}
		s.ReviewReady = &v1alpha1.ReviewReadyStatus{LastDecision: "idle", LastReason: reason}
	}); err != nil {
		deps.log()("review-ready: status patch failed: %v", err)
	}
}

// parsePRPointer splits "host/owner/name#N" (the webhook's annotation form).
func parsePRPointer(s string) (string, int, error) {
	i := strings.LastIndex(s, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("missing #")
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("bad PR number")
	}
	repo := s[:i]
	if !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("bad repo path")
	}
	return repo, n, nil
}

func metaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	m := metav1.NewTime(*t)
	return &m
}
