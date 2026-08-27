// Package review implements the Review-Ready Gate (ADR-0006): the
// deterministic decision that a Pull Request may enter adversarial review.
//
// The gate is event-armed: git hosts send consolidated pull_request events
// (GitHub semantics, action-dispatched) to the per-instance webhook, which
// only annotates the Workflow. This package performs the decision when the
// workflow runs: the required label must be present on an open PR AND every
// merge-rule required context must be green at the head SHA. The
// repository's merge rules (GitHub required_status_checks contexts, Forgejo
// status_check_contexts) are the single definition of "CI green".
//
// Outcomes:
//
//	proceed    — label present, all required contexts green at head: emit the
//	             Trigger Envelope and let the pipeline continue (the kernel
//	             provisions the knowns; the agent investigates the unknowns).
//	waiting    — CI pending (or red: a red-CI verdict is noise — the dev
//	             already sees red CI), gate stays armed; the next push
//	             (synchronize) re-arms at the new head.
//	standdown  — PR closed/merged, horizon exceeded, or the label removed
//	             AFTER a verdict was posted (the post-review consume: the
//	             trailer in the PR comments is the durable signal). A label
//	             absent WITHOUT a verdict keeps the gate armed (lost-ingress
//	             recovery, #237): the removal may have been observed at a
//	             stale instant (label rm+add race, wake lost to an outage)
//	             and the next evaluation re-derives the truth from the host.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// Decision is the gate outcome.
type Decision string

const (
	DecisionProceed   Decision = "proceed"
	DecisionWaiting   Decision = "waiting"
	DecisionStanddown Decision = "standdown"
)

// Envelope is the durable handoff from the gate into the pipeline: the
// minimal knowns the workspace provisions and the agent reviews against.
type Envelope struct {
	Repo             string   `json:"repo"`             // host/owner/name as configured
	PR               int      `json:"pr"`               // pull-request number
	HeadSHA          string   `json:"headSha"`          // the reviewed commit
	Base             string   `json:"base"`             // base branch (merge-rule source)
	Label            string   `json:"label"`            // the review-request label
	RequiredContexts []string `json:"requiredContexts"` // merge-rule required contexts
	GreenContexts    []string `json:"greenContexts"`    // contexts observed green at head
}

// Evaluation is one gate decision plus its reason.
type Evaluation struct {
	Decision Decision
	Reason   string
	Envelope *Envelope // set only on proceed
}

// API is the per-host API surface the gate reads. It exists so tests can
// stub the transport.
type API interface {
	GetPullRequest(ctx context.Context, repo string, number int) (*PullRequest, error)
	ListLabeledOpenPulls(ctx context.Context, repo, label string) ([]PullRequest, error)
	RequiredContexts(ctx context.Context, repo, branch string) ([]string, error)
	ContextStates(ctx context.Context, repo, sha string) (map[string]string, error)
	ListComments(ctx context.Context, repo string, number int, since time.Time) ([]IssueComment, error)
}

// PullRequest is the normalized PR view the gate needs.
type PullRequest struct {
	Number  int      `json:"number"`
	State   string   `json:"state"` // "open" | "closed"
	HeadSHA string   `json:"headSha"`
	Base    string   `json:"base"`
	Labels  []string `json:"labels"`
}

// ---------------------------------------------------------------------------
// Host resolution — mirrors the platform's repo-path convention:
//
//	github.com/owner/repo      → https://api.github.com      (GitHub)
//	codeberg.org/owner/repo    → https://codeberg.org/api/v1 (Forgejo)
//	git.rezus.cloud/owner/repo → https://git.rezus.cloud/api/v1 (Forgejo)
//	host/owner/repo            → https://host/api/v1         (Forgejo)
//	owner/repo                 → https://api.github.com      (GitHub)
// ---------------------------------------------------------------------------

type HostKind string

const (
	HostGitHub  HostKind = "github"
	HostForgejo HostKind = "forgejo"
)

// verdictSinceSlack widens the verdict-scan window below the gate's arm
// time to absorb clock skew between the gate and the git host (both stamp
// `since` filtering with their own clocks). It must stay well under the
// gap between distinct review cycles on one PR (label re-add after a
// consume) so an OLD verdict cannot fall inside a fresh request's window.
const verdictSinceSlack = 5 * time.Minute

type Host struct {
	Kind     HostKind
	APIBase  string
	RepoPath string // owner/name (without host)
}

// ResolveHost maps a configured repo path to its API endpoint kind.
func ResolveHost(repo string) (Host, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	switch {
	case len(parts) == 3 && parts[0] == "github.com":
		return Host{Kind: HostGitHub, APIBase: "https://api.github.com", RepoPath: parts[1] + "/" + parts[2]}, nil
	case len(parts) == 3:
		return Host{Kind: HostForgejo, APIBase: "https://" + parts[0] + "/api/v1", RepoPath: parts[1] + "/" + parts[2]}, nil
	case len(parts) == 2:
		return Host{Kind: HostGitHub, APIBase: "https://api.github.com", RepoPath: parts[0] + "/" + parts[1]}, nil
	default:
		return Host{}, fmt.Errorf("review: bad repo path %q (want host/owner/name or owner/name)", repo)
	}
}

// TokenEnvNames returns the env vars holding the API token for the host,
// in precedence order (first non-empty wins).
func (h Host) TokenEnvNames() []string {
	switch h.Kind {
	case HostGitHub:
		return []string{"HARMOSTES_GIT_TOKEN", "HARMOSTES_GITHUB_TOKEN"}
	case HostForgejo:
		if strings.Contains(h.APIBase, "codeberg.org") {
			return []string{"HARMOSTES_CODEBERG_TOKEN", "LLM_WIKI_CODEBERG_TOKEN"}
		}
		if strings.Contains(h.APIBase, "git.rezus.cloud") {
			return []string{"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_RZC_PASSWORD"}
		}
		return []string{"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_GIT_TOKEN"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// REST API (GitHub + Forgejo shapes normalized)
// ---------------------------------------------------------------------------

// RESTAPI talks to GitHub/Forgejo REST endpoints. Token resolution is
// injected via TokenLookup so environments stay testable.
type RESTAPI struct {
	Client      *http.Client
	TokenLookup func(string) string
	// BaseOverride, when set, replaces the resolved API base (tests).
	BaseOverride string
}

func (a *RESTAPI) baseURL(host Host) string {
	if a.BaseOverride != "" {
		return a.BaseOverride
	}
	return host.APIBase
}

func (a *RESTAPI) get(ctx context.Context, host Host, path, accept string, out any) error {
	if a.Client == nil {
		a.Client = http.DefaultClient
	}
	url := a.baseURL(host) + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	token := ""
	if a.TokenLookup != nil {
		for _, env := range host.TokenEnvNames() {
			if v := a.TokenLookup(env); v != "" {
				token = v
				break
			}
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	req.Header.Set("Accept", accept)
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{Status: resp.StatusCode, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var errNotFound = fmt.Errorf("review: not found")

// httpError carries a non-2xx status from get().
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("review: HTTP %d: %s", e.Status, e.Body) }

func isForbidden(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.Status == http.StatusForbidden
	}
	return false
}

// GetPullRequest fetches the normalized PR view (labels, head, base, state).
func (a *RESTAPI) GetPullRequest(ctx context.Context, repo string, number int) (*PullRequest, error) {
	host, err := ResolveHost(repo)
	if err != nil {
		return nil, err
	}
	var raw struct {
		State string `json:"state"`
		Head  struct {
			Sha string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/pulls/%d", host.RepoPath, number), "application/json", &raw); err != nil {
		return nil, err
	}
	pr := &PullRequest{State: raw.State, HeadSHA: raw.Head.Sha, Base: raw.Base.Ref}
	for _, l := range raw.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr, nil
}

// IssueComment is the minimal issue/PR comment view the gate needs: the
// body, scanned for the verdict trailer.
type IssueComment struct {
	Body string `json:"body"`
}

// ListComments fetches the PR's conversation comments updated at or after
// `since`. Both host kinds expose issue comments at the same path shape AND
// honor the `since` filter (updated_at ≥ since) — which makes the hosts'
// oldest-first ordering (no sort param exists on GitHub's endpoint;
// Gitea/Forgejo also order ascending by id) IRRELEVANT: the consume signal
// (a verdict trailer) is posted after the gate armed, so restricting the
// window to [since, now] must surface it without walking pages. Callers pass
// armedAt minus a slack that covers gate↔host clock skew.
func (a *RESTAPI) ListComments(ctx context.Context, repo string, number int, since time.Time) ([]IssueComment, error) {
	host, err := ResolveHost(repo)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", host.RepoPath, number)
	if !since.IsZero() {
		path += "&since=" + since.UTC().Format(time.RFC3339)
	}
	var out []IssueComment
	if err := a.get(ctx, host, path, "application/json", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RequiredContexts reads the repo's merge rules and returns the contexts
// that must be green to merge — the single definition of "CI green". A repo
// without branch protection has no required contexts (label alone proceeds).
func (a *RESTAPI) RequiredContexts(ctx context.Context, repo, branch string) ([]string, error) {
	host, err := ResolveHost(repo)
	if err != nil {
		return nil, err
	}
	switch host.Kind {
	case HostGitHub:
		var prot struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
			} `json:"required_status_checks"`
		}
		// 404 = no protection configured. 403 = protection exists but the
		// token cannot read it (fine-grained PAT / App without
		// Administration:read) — the repo HAS merge rules we cannot see.
		// Proceed on label alone: blocking review on token scope would
		// silently kill every review for such tokens (6h horizon standdown),
		// while the CI itself still gates the merge on the host side.
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/branches/%s/protection", host.RepoPath, branch), "application/vnd.github+json", &prot); err != nil {
			if err == errNotFound || isForbidden(err) {
				return nil, nil
			}
			return nil, err
		}
		return prot.RequiredStatusChecks.Contexts, nil
	default: // Forgejo
		var prot struct {
			StatusCheckContexts []string `json:"status_check_contexts"`
		}
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/branch_protections/%s", host.RepoPath, branch), "application/json", &prot); err != nil {
			if err == errNotFound || isForbidden(err) {
				return nil, nil
			}
			return nil, err
		}
		return prot.StatusCheckContexts, nil
	}
}

// ContextStates merges commit statuses and check-runs into a per-context
// normalized state: success | pending | failure. GitHub required contexts
// may be satisfied by either surface; Forgejo posts both.
func (a *RESTAPI) ContextStates(ctx context.Context, repo, sha string) (map[string]string, error) {
	host, err := ResolveHost(repo)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}

	// Only errNotFound legitimately means "no results on this surface";
	// every other error (401/403/5xx) propagates so the gate's waiting
	// reason names the fault instead of masquerading as "ci pending".
	var firstErr error
	get := func(path, accept string, out any) {
		if err := a.get(ctx, host, path, accept, out); err != nil && err != errNotFound && firstErr == nil {
			firstErr = err
		}
	}

	switch host.Kind {
	case HostGitHub:
		var combined struct {
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		get(fmt.Sprintf("/repos/%s/commits/%s/status", host.RepoPath, sha), "application/json", &combined)
		// First-wins: both hosts list newest-first; superseded attempts must
		// not clobber the freshest state per context.
		for _, s := range combined.Statuses {
			if _, ok := states[s.Context]; !ok {
				states[s.Context] = normalizeStatusState(s.State)
			}
		}
		var checks struct {
			TotalCount int `json:"total_count"`
			CheckRuns  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		get(fmt.Sprintf("/repos/%s/commits/%s/check-runs", host.RepoPath, sha), "application/vnd.github+json", &checks)
		for _, c := range checks.CheckRuns {
			if _, ok := states[c.Name]; !ok {
				states[c.Name] = normalizeCheckRun(c.Status, c.Conclusion)
			}
		}
	default: // Forgejo
		var statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`  // GitHub field name
			StatusF string `json:"status"` // Forgejo/Gitea field name
		}
		get(fmt.Sprintf("/repos/%s/commits/%s/statuses", host.RepoPath, sha), "application/json", &statuses)
		// Forgejo returns NEWEST-FIRST with multiple entries per context
		// (superseded attempts linger below). FIRST entry wins — last-wins
		// let a stale pending clobber the fresh success, arming the gate
		// forever on green heads (observed live on rhesadox #1566). Field
		// name differs by host: GitHub sends `state`, Forgejo sends
		// `status` (a missing field parsing as "" classified as failure —
		// observed live: an all-green head read as red).
		for _, s := range statuses {
			v := s.State
			if v == "" {
				v = s.StatusF
			}
			if _, ok := states[s.Context]; !ok {
				states[s.Context] = normalizeStatusState(v)
			}
		}
		var checks struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		get(fmt.Sprintf("/repos/%s/commits/%s/check-runs", host.RepoPath, sha), "application/json", &checks)
		for _, c := range checks.CheckRuns {
			if _, ok := states[c.Name]; !ok {
				states[c.Name] = normalizeCheckRun(c.Status, c.Conclusion)
			}
		}
	}
	if firstErr != nil {
		return states, firstErr
	}
	return states, nil
}

func normalizeStatusState(s string) string {
	switch s {
	case "success":
		return "success"
	case "skipped":
		// Host merge-rule parity: Forgejo posts a `skipped` commit status
		// for guard-skipped jobs, and its branch protection counts a
		// required context whose latest status is `skipped` as SATISFIED
		// (verified live: PR mergeable=true with skipped decode contexts;
		// GitHub behaves the same for neutral/skipped checks). Mapping it
		// to failure made the gate stricter than the merge rules.
		return "success"
	case "pending":
		return "pending"
	default: // error, failure
		return "failure"
	}
}

func normalizeCheckRun(status, conclusion string) string {
	switch status {
	case "queued", "in_progress", "waiting", "pending", "running", "blocked":
		return "pending"
	}
	switch conclusion {
	case "success", "skipped", "neutral":
		// skipped/neutral SATISFY the requirement — host merge-rule parity:
		// GitHub branch protection counts a skipped/neutral required check
		// as satisfied. Mapping them to pending made the gate stricter than
		// the merge rules (mergeable PRs stood down at the horizon).
		return "success"
	case "failure", "cancelled", "timed_out", "action_required":
		return "failure"
	default: // stale, "" (still running)
		return "pending"
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// Params is everything one gate evaluation needs. Armed state is passed in
// and returned so the caller can persist it on the Workflow status.
type Params struct {
	Repo    string // host/owner/name of the trigger/target PR
	PR      int
	Label   string
	Horizon time.Duration
	Now     time.Time
	// ArmedAt is when the gate armed at the current head (zero → arm now).
	ArmedAt time.Time
	// ArmedSha is the SHA the gate last saw ("" → arm at the current head).
	ArmedSha string

	// WakeSHA is the trigger-revision from the wake event (the webhook's
	// head SHA); used to arm on a fresh wake whose first PR fetch fails.
	WakeSHA string
	// DispatchedAt marks an armed review the gate already proceeded on
	// (zero → not dispatched): the agent run is (or was) in flight and the
	// verdict hasn't been consumed. Sweeps must not re-dispatch it — the
	// verdict window is the consume signal, mirroring the label-absent
	// path — until DispatchTimeout presumes the run dead (#248: no live
	// run can outlive the consumer's 30m one-shot bound).
	DispatchedAt time.Time
	// DispatchTimeout is the dispatch liveness bound (#248): past it,
	// a dispatched review without a verdict stands down as presumed dead
	// and the backlog pass re-arms on the next sweep. Zero disables the
	// bound (the Horizon remains the release).
	DispatchTimeout time.Duration
	// DisarmHint is set when the wake event already implies stand-down
	// (e.g. action=closed).
	DisarmHint bool
}

// Result is the evaluation plus updated armed-state bookkeeping.
type Result struct {
	Evaluation
	// NewArmedSha/NewArmedAt: updated armed state (empty NewArmedSha → disarm).
	NewArmedSha string
	NewArmedAt  time.Time
}

// Evaluate performs one Review-Ready decision.
func Evaluate(ctx context.Context, api API, p Params) Result {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	if p.DisarmHint {
		return Result{Evaluation: standdown("pull request closed"), NewArmedSha: ""}
	}

	pr, err := api.GetPullRequest(ctx, p.Repo, p.PR)
	if err != nil {
		// Transient API failure: keep armed, retry next cycle. On a FRESH
		// wake (no prior armed state) we must ARM here — an empty
		// NewArmedSha would write the disarm branch, switching the
		// controller's armed carve-out off and silently losing this wake's
		// review until the next PR event. The wake's trigger revision is
		// the best-known head; the next evaluation refreshes it.
		keepSha := p.ArmedSha
		if keepSha == "" {
			keepSha = p.WakeSHA
		}
		return Result{Evaluation: waiting("pr fetch failed: " + err.Error()), NewArmedSha: keepSha, NewArmedAt: armTime(p.ArmedAt, now)}
	}

	if pr.State != "open" {
		return Result{Evaluation: standdown("pull request " + stateWord(pr.State)), NewArmedSha: ""}
	}

	if !hasLabel(pr.Labels, p.Label) {
		// Label absent is a real stand-down ONLY once a verdict exists: the
		// deploy plugin removes the label after posting the verdict trailer
		// — that trailer is the durable consume signal. Without a verdict the
		// absence is ambiguous: this evaluation runs on one instantaneous PR
		// fetch, and an 'unlabeled' wake racing a newer 'labeled' arm (label
		// rm+add cycles from the trigger helpers) or a wake lost to a host API
		// outage can observe label-absent while the label is actually present
		// — observed live on rhesadox #1635: standdown "label absent" while
		// the label was present and all contexts green, and nothing woke the
		// gate again (the schedule tick only re-evaluates the ARMED state).
		// Stay armed and wait: the next tick re-fetches the PR and converges
		// (label present → the normal path; horizon exceeded → standdown).
		// The verdict scan is time-windowed (armedAt − slack): a consume
		// trailer is posted after the gate armed, so [since, now] must carry
		// it — regardless of the hosts' oldest-first comment ordering (no
		// sort param exists; page 1 of a >100-comment PR holds the OLDEST
		// comments, not the fresh verdict). Old verdicts from earlier review
		// cycles on the same PR fall outside the window and cannot consume a
		// fresh request.
		since := armTime(p.ArmedAt, now).Add(-verdictSinceSlack)
		comments, err := api.ListComments(ctx, p.Repo, p.PR, since)
		if err != nil {
			keepSha := p.ArmedSha
			if keepSha == "" {
				keepSha = pr.HeadSHA
			}
			return Result{Evaluation: waiting("label absent; verdict check failed: " + err.Error()), NewArmedSha: keepSha, NewArmedAt: armTime(p.ArmedAt, now)}
		}
		if hasVerdict(comments) {
			return Result{Evaluation: standdown("label absent (verdict posted — consumed)"), NewArmedSha: ""}
		}
		armedAt := armTime(p.ArmedAt, now)
		// Head moved during the ambiguity window: reset the horizon clock,
		// consistent with the label-present head-moved path below — a head
		// that moved inherits a fresh horizon, not the stale armed clock
		// (which could stand down prematurely).
		if p.ArmedSha != "" && p.ArmedSha != pr.HeadSHA {
			armedAt = now
		}
		if now.Sub(armedAt) > p.Horizon {
			return Result{Evaluation: standdown("horizon exceeded (label absent, no verdict; pending > " + p.Horizon.String() + ")"), NewArmedSha: ""}
		}
		return Result{Evaluation: waiting("label absent, no verdict — ingress may be lost, staying armed"), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	}

	// Head moved since arming: re-arm at the new head (reset the horizon).
	armedAt := armTime(p.ArmedAt, now)
	if p.ArmedSha != "" && p.ArmedSha != pr.HeadSHA {
		armedAt = now
	}

	if now.Sub(armedAt) > p.Horizon {
		return Result{Evaluation: standdown("horizon exceeded (CI pending > " + p.Horizon.String() + ")"), NewArmedSha: ""}
	}

	required, err := api.RequiredContexts(ctx, p.Repo, pr.Base)
	if err != nil {
		return Result{Evaluation: waiting("merge-rules fetch failed: " + err.Error()), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	}
	if len(required) == 0 {
		// No merge-rule contexts defined: the label is the whole contract.
		return proceed(p, pr, nil, nil)
	}

	states, err := api.ContextStates(ctx, p.Repo, pr.HeadSHA)
	if err != nil {
		return Result{Evaluation: waiting("contexts fetch failed: " + err.Error()), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	}

	var pending, red, green []string
	for _, ctx := range required {
		switch states[ctx] {
		case "success":
			green = append(green, ctx)
		case "failure":
			red = append(red, ctx)
		default: // pending or missing entirely (run not started)
			pending = append(pending, ctx)
		}
	}

	// In-flight discrimination (#250 r2) + liveness bound (#248): a
	// dispatched review keeps the label until the deploy plugin removes it
	// after posting the verdict. Green+label alone would re-proceed on every
	// sweep — duplicate dispatch. The verdict window (armedAt − slack) is
	// the durable consume signal, the same one the label-absent path uses.
	// Past DispatchTimeout with no verdict the run is provably dead — the
	// consumer wraps every one-shot run in a 30m context — so the gate
	// stands down and the backlog pass re-arms the still-labeled PR on the
	// next sweep (recovery without any external label toggle, #248).
	if !p.DispatchedAt.IsZero() {
		since := armTime(p.ArmedAt, now).Add(-verdictSinceSlack)
		comments, err := api.ListComments(ctx, p.Repo, p.PR, since)
		if err != nil {
			return Result{Evaluation: waiting("in-flight verdict check failed: " + err.Error()), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
		}
		if hasVerdict(comments) {
			return Result{Evaluation: standdown("verdict posted — consumed"), NewArmedSha: ""}
		}
		if p.DispatchTimeout > 0 && now.Sub(p.DispatchedAt) >= p.DispatchTimeout {
			return Result{Evaluation: standdown(fmt.Sprintf("dispatch presumed dead (no verdict after %s; run bound %s) — backlog will re-arm", p.DispatchTimeout, v1alpha1.OneShotRunBound)), NewArmedSha: ""}
		}
		return Result{Evaluation: waiting("review in flight — dispatched, verdict pending"), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	}

	switch {
	case len(red) > 0:
		// Red CI is a silent non-event: the dev already sees red CI; a
		// REQUEST_CHANGES verdict would be noise. Stay armed — the next
		// push (synchronize) re-arms at the new head.
		return Result{Evaluation: waiting("ci red at head (" + strings.Join(red, ", ") + ") — staying armed"), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	case len(pending) > 0:
		return Result{Evaluation: waiting("ci pending (" + strings.Join(pending, ", ") + ")"), NewArmedSha: pr.HeadSHA, NewArmedAt: armedAt}
	default:
		return proceed(p, pr, required, green)
	}
}

func proceed(p Params, pr *PullRequest, required, green []string) Result {
	return Result{
		Evaluation: Evaluation{
			Decision: DecisionProceed,
			Reason:   "label present, all required contexts green at head",
			Envelope: &Envelope{
				Repo:             p.Repo,
				PR:               p.PR,
				HeadSHA:          pr.HeadSHA,
				Base:             pr.Base,
				Label:            p.Label,
				RequiredContexts: required,
				GreenContexts:    green,
			},
		},
		// Stay armed at the dispatched head: the worker runs the review, the
		// deploy plugin posts the verdict and removes the label. Until the
		// verdict lands, sweeps re-check the in-flight window (DispatchedAt)
		// instead of re-dispatching — the backlog pass made re-dispatch a
		// every-sweep certainty otherwise. Consume clears the armed slot
		// (label-absent + verdict, the in-flight verdict check, or the
		// dispatch liveness bound standing down a dead run).
		NewArmedSha: pr.HeadSHA,
		NewArmedAt:  armTime(p.ArmedAt, nowFrom(p)),
	}
}

// nowFrom centralizes Params.Now defaulting for helpers that need it.
func nowFrom(p Params) time.Time {
	if !p.Now.IsZero() {
		return p.Now
	}
	return time.Now()
}

func waiting(reason string) Evaluation { return Evaluation{Decision: DecisionWaiting, Reason: reason} }
func standdown(reason string) Evaluation {
	return Evaluation{Decision: DecisionStanddown, Reason: reason}
}

// hasVerdict reports whether any conversation comment carries a pr-review
// verdict trailer (the skill's output contract — the merge currency). A
// verdict at ANY sha counts: a fresh review request re-adds the label (a new
// 'labeled' wake), so verdict-present + label-absent is always the
// post-review cleanup, never a pending request.
func hasVerdict(comments []IssueComment) bool {
	for _, c := range comments {
		if strings.Contains(c.Body, "<!-- pr-review: ") {
			return true
		}
	}
	return false
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func armTime(prev, now time.Time) time.Time {
	if !prev.IsZero() {
		return prev
	}
	return now
}

func stateWord(s string) string {
	if s == "closed" {
		return "closed"
	}
	return s
}

// ListLabeledOpenPulls returns the repo's open PRs carrying the given label,
// oldest first (Forgejo: sort=oldest; GitHub: sort=created&direction=asc —
// both stable "least recently touched wins" for a starved backlog). The
// gate's backlog pass (#249) uses it to arm the oldest starved labeled PR
// when its armed slot frees up: labels added by
// hand or automation while the gate was busy produce no further events, so a
// sweep must discover them.
func (a *RESTAPI) ListLabeledOpenPulls(ctx context.Context, repo, label string) ([]PullRequest, error) {
	host, err := ResolveHost(repo)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number    int    `json:"number"`
		UpdatedAt string `json:"updated_at"`
		Labels    []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	// One page of 50 open PRs is far beyond any labeled backlog in practice,
	// oldest-first so the most-starved PR is first without client-side
	// sorting. Param dialects differ per host kind (same reasoning as
	// ListComments): GitHub has no sort=oldest (created|updated|popularity|
	// long-running + direction=asc) and reads per_page, not limit — the
	// shared form would return newest-first capped at 30 there, arming the
	// newest and hiding the starved beyond 30 (#250 r1).
	q := "state=open&sort=oldest&limit=50"
	if host.Kind == HostGitHub {
		q = "state=open&sort=created&direction=asc&per_page=50"
	}
	if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/pulls?%s", host.RepoPath, q), "application/json", &raw); err != nil {
		return nil, err
	}
	var out []PullRequest
	for _, p := range raw {
		has := false
		for _, l := range p.Labels {
			if l.Name == label {
				has = true
				break
			}
		}
		if has {
			out = append(out, PullRequest{Number: p.Number})
		}
	}
	return out, nil
}
