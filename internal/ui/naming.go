package ui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Deterministic Workflow Naming
// ---------------------------------------------------------------------------
//
// Every Workflow CR name follows one convention:
//
//	{gate}-{targetSlug}
//
// where:
//   - gate       = the gate plugin name (wiki-lint, pr-review,
//     fork-maintenance, noop). The gate IS the workflow archetype — it
//     determines the entire structure.
//   - targetSlug = a DNS-safe slug derived from the repo the workflow
//     operates on. For wiki-lint this is the source code repo being
//     documented; for pr-review, the repo whose PRs are reviewed;
//     for fork-maintenance, the fork being synced.
//
// Examples:
//
//	wiki-lint-harmostes       — harmostes → llm-wiki docs
//	pr-review-harmostes — review PRs on harmostes
//	fork-maintenance-dapr     — sync the dapr fork
//
// This convention makes the template visible in the name itself, preventing
// the "mixed-up" problem where `harmostes` and `pr-review-harmostes` look
// unrelated despite targeting the same repo under different templates.

// slugRe strips everything that is not a lowercase alphanumeric or hyphen.
var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// maxSlugLen caps the target slug so the full name stays under the 63-char
// k8s DNS limit even with the longest gate prefix ("fork-maintenance-" = 17).
const maxSlugLen = 40

// repoSlug extracts a DNS-safe slug from a git repo URL or short path.
//
// Accepts:
//   - full HTTPS/SSH URLs: "https://github.com/tibrezus/harmostes.git",
//     "git@github.com:tibrezus/harmostes.git"
//   - short paths: "github.com/tibrezus/harmostes"
//   - bare names: "harmostes"
//
// The slug is the final path component, lowercased, with a .git suffix
// stripped. Organisation prefixes are dropped — the slug identifies the
// *repo*, not the owner (owners vary, repos are the stable target).
// workflowCRName strips the platform prefix from an Attempt's WorkflowRef
// ("harmostes/pr-review-rhesadox" → "pr-review-rhesadox"). Attempt refs are
// platform-qualified but UI routes address Workflow CRs by bare name; a raw
// ref in an href produces an un routable extra path segment.
func workflowCRName(workflowRef string) string {
	if i := strings.LastIndex(workflowRef, "/"); i >= 0 {
		return workflowRef[i+1:]
	}
	return workflowRef
}

func repoSlug(repoURL string) string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return ""
	}

	// Handle SSH-style: git@host:org/repo.git
	if strings.Contains(repoURL, "://") {
		if u, err := url.Parse(repoURL); err == nil && u.Path != "" {
			repoURL = u.Path
		}
	} else if i := strings.Index(repoURL, ":"); i > 0 && !strings.Contains(repoURL[:i], "/") {
		// git@host:org/repo → org/repo
		repoURL = repoURL[i+1:]
	}

	// Take the last path component.
	base := path.Base(repoURL)
	base = strings.TrimSuffix(base, ".git")
	base = strings.ToLower(base)
	base = slugRe.ReplaceAllString(base, "")
	base = strings.Trim(base, "-")

	if len(base) > maxSlugLen {
		base = base[:maxSlugLen]
		base = strings.Trim(base, "-")
	}
	return base
}

// deterministicWorkflowName builds the canonical name: {gate}-{slug}.
//
// Returns "" if either argument is empty.
func deterministicWorkflowName(gateName, repoURL string) string {
	gateName = strings.TrimSpace(gateName)
	slug := repoSlug(repoURL)
	if gateName == "" || slug == "" {
		return ""
	}
	return gateName + "-" + slug
}

// workflowTarget extracts the target repo from a Workflow CR spec.
//
// The target depends on the gate archetype (catalog-driven, no string
// matching):
//   - gates with TargetFromPrepareRepos=true (pr-review): the repo in
//     spec.prepare.config.repos[0] (the repo whose PRs are reviewed). Falls
//     back to spec.source.repo.
//   - all others: spec.source.repo (the monitored source), falling back to
//     spec.workspaceRepo.url.
//
// Returns a raw URL/path string suitable for repoSlug().
func workflowTarget(wf v1alpha1.WorkflowSpec) string {
	// PR-review workflows name their target from the first scoped repo (the
	// prepare plugin is pr-fetch or resolved from a pr-review template).
	if wf.Prepare.Plugin.Name == "pr-fetch" || wf.Prepare.Plugin.Name == "" {
		if repos := extractPrepareRepos(wf.Prepare.Config); len(repos) > 0 {
			return repos[0]
		}
	}

	// Default: source.repo is the monitored repo.
	if wf.Source.Repo != "" {
		return wf.Source.Repo
	}
	// Fallback: workspace repo URL.
	if wf.WorkspaceRepo != nil {
		return wf.WorkspaceRepo.URL
	}
	return ""
}

// extractPrepareRepos reads the "repos" array from a prepare plugin's raw
// JSON config. Returns nil if absent or unparseable.
func extractPrepareRepos(rawConfig json.RawMessage) []string {
	if len(rawConfig) == 0 {
		return nil
	}
	var cfg struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(rawConfig, &cfg); err != nil {
		return nil
	}
	return cfg.Repos
}

// parseGitTarget extracts the host and owner/repo from a git URL.
// "https://github.com/rezuscloud/signoz.git" → ("github.com", "rezuscloud/signoz")
// "git@github.com:rezuscloud/signoz.git" → ("github.com", "rezuscloud/signoz")
func parseGitURL(rawURL string) (host, object string) {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return "", ""
	}
	// SSH form: git@host:path
	if i := strings.Index(s, ":"); i > 0 && strings.Contains(s[:i], "@") {
		host = s[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		object = strings.TrimSuffix(s[i+1:], ".git")
		return host, object
	}
	// HTTPS form
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host, strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	}
	return "", s
}

// formatDuration renders a duration for UI display (compact, one decimal).
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%.1fm", d.Minutes())
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}
