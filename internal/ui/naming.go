package ui

import (
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strings"

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
//   - gate       = the gate plugin name (wiki-lint, review-validate,
//     fork-resolved, noop). The gate IS the workflow archetype — it
//     determines the entire structure.
//   - targetSlug = a DNS-safe slug derived from the repo the workflow
//     operates on. For wiki-lint this is the source code repo being
//     documented; for review-validate, the repo whose PRs are reviewed;
//     for fork-resolved, the fork being synced.
//
// Examples:
//
//	wiki-lint-harmostes       — harmostes → llm-wiki docs
//	review-validate-harmostes — review PRs on harmostes
//	fork-resolved-dapr        — sync the dapr fork
//
// This convention makes the template visible in the name itself, preventing
// the "mixed-up" problem where `harmostes` and `pr-review-harmostes` look
// unrelated despite targeting the same repo under different templates.

// slugRe strips everything that is not a lowercase alphanumeric or hyphen.
var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// maxSlugLen caps the target slug so the full name stays under the 63-char
// k8s DNS limit even with the longest gate prefix ("review-validate-" = 16).
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
// The target depends on the gate archetype:
//   - review-validate: the repo in spec.prepare.config.repos[0] (the repo
//     whose PRs are reviewed). Falls back to spec.source.repo.
//   - all others: spec.source.repo (the monitored source), falling back to
//     spec.workspaceRepo.url.
//
// Returns a raw URL/path string suitable for repoSlug().
func workflowTarget(wf v1alpha1.WorkflowSpec) string {
	// review-validate: target is in the prepare config repos list.
	if wf.Agent.Gate.Plugin.Name == "review-validate" {
		if wf.Prepare.Plugin.Name == "pr-fetch" || wf.Prepare.Plugin.Name == "" {
			if repos := extractPrepareRepos(wf.Prepare.Config); len(repos) > 0 {
				return repos[0]
			}
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
