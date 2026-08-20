package worker

import (
	"context"
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
func reviewGate(ctx context.Context, deps Deps, wf *v1alpha1.Workflow) *review.Envelope {
	rr := wf.Spec.ReviewReady
	if rr == nil {
		return nil // gate not configured for this workflow
	}

	armed := wf.Status.ReviewReady
	trigPR := wf.Annotations["harmostes.dev/trigger-pr"]
	action := wf.Annotations["harmostes.dev/trigger-action"]

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
	}
	if armed != nil {
		params.ArmedSha = armed.ArmedSha
		if armed.ArmedSince != nil {
			params.ArmedAt = armed.ArmedSince.Time
		}
	}
	// A wake event targeting a different PR than the armed one re-targets
	// the gate (single-flight per workflow: newest request wins).
	if armed != nil && trigPR != "" && armed.ArmedPR != 0 && (armed.ArmedRepo != repo || armed.ArmedPR != pr) {
		params.ArmedSha = ""
		params.ArmedAt = time.Time{}
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
