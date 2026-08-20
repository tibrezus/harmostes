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
//	standdown  — label removed, PR closed/merged, or horizon exceeded:
//	             disarm (silent non-event).
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
	RequiredContexts(ctx context.Context, repo, branch string) ([]string, error)
	ContextStates(ctx context.Context, repo, sha string) (map[string]string, error)
}

// PullRequest is the normalized PR view the gate needs.
type PullRequest struct {
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
		return fmt.Errorf("review: %s %s: %s", http.MethodGet, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

var errNotFound = fmt.Errorf("review: not found")

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
		// 403 (insufficient scope) and 404 (no protection) both mean "no
		// required contexts defined we can read" — proceed on label alone
		// rather than blocking review on API permissions.
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/branches/%s/protection", host.RepoPath, branch), "application/vnd.github+json", &prot); err != nil {
			if err == errNotFound {
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
			if err == errNotFound {
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

	switch host.Kind {
	case HostGitHub:
		var combined struct {
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/commits/%s/status", host.RepoPath, sha), "application/json", &combined); err == nil {
			for _, s := range combined.Statuses {
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
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/commits/%s/check-runs", host.RepoPath, sha), "application/vnd.github+json", &checks); err == nil {
			for _, c := range checks.CheckRuns {
				states[c.Name] = normalizeCheckRun(c.Status, c.Conclusion)
			}
		}
	default: // Forgejo
		var statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		}
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/commits/%s/statuses", host.RepoPath, sha), "application/json", &statuses); err == nil {
			for _, s := range statuses {
				states[s.Context] = normalizeStatusState(s.State)
			}
		}
		var checks struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := a.get(ctx, host, fmt.Sprintf("/repos/%s/commits/%s/check-runs", host.RepoPath, sha), "application/json", &checks); err == nil {
			for _, c := range checks.CheckRuns {
				states[c.Name] = normalizeCheckRun(c.Status, c.Conclusion)
			}
		}
	}
	return states, nil
}

func normalizeStatusState(s string) string {
	switch s {
	case "success":
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
	case "success":
		return "success"
	case "failure", "cancelled", "timed_out", "action_required":
		return "failure"
	default: // skipped, neutral, stale, "" (still running)
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
		// Transient API failure: keep armed, retry next cycle.
		return Result{Evaluation: waiting("pr fetch failed: " + err.Error()), NewArmedSha: p.ArmedSha, NewArmedAt: armTime(p.ArmedAt, now)}
	}

	if pr.State != "open" {
		return Result{Evaluation: standdown("pull request " + stateWord(pr.State)), NewArmedSha: ""}
	}

	if !hasLabel(pr.Labels, p.Label) {
		// The label is the only ingress; its absence disarms. (The deploy
		// plugin removes the label after posting a verdict — this is also
		// the post-review cleanup path.)
		return Result{Evaluation: standdown("label absent"), NewArmedSha: ""}
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
		NewArmedSha: "", // consumed: the deploy plugin posts the verdict and
		// removes the label; the next evaluation sees label-absent and
		// stands down idle.
	}
}

func waiting(reason string) Evaluation { return Evaluation{Decision: DecisionWaiting, Reason: reason} }
func standdown(reason string) Evaluation {
	return Evaluation{Decision: DecisionStanddown, Reason: reason}
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
