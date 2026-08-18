package ui

import (
	"encoding/json"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestRepoSlug(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		// HTTPS URLs
		{"https github with .git", "https://github.com/tibrezus/harmostes.git", "harmostes"},
		{"https github no .git", "https://github.com/tibrezus/harmostes", "harmostes"},
		{"https codeberg", "https://codeberg.org/forgejo/forgejo", "forgejo"},
		{"https forgejo subpath", "https://git.rezus.cloud/tibrez/rhesadox.git", "rhesadox"},
		// SSH URLs
		{"ssh github", "git@github.com:tibrezus/harmostes.git", "harmostes"},
		{"ssh gitlab", "git@gitlab.com:rezusnet/operations/k8s-config.git", "k8s-config"},
		// Short paths
		{"short path", "github.com/tibrezus/harmostes", "harmostes"},
		{"bare name", "harmostes", "harmostes"},
		// Edge cases
		{"uppercase normalised", "https://github.com/Tibrezus/Harmostes.git", "harmostes"},
		{"trailing slash", "https://github.com/tibrezus/harmostes/", "harmostes"},
		{"empty", "", ""},
		{"only domain", "https://github.com", "githubcom"},
		// Special characters stripped
		{"underscores to hyphens then stripped", "https://github.com/org/my_repo.git", "myrepo"},
		{"dots in name preserved", "https://github.com/org/config.v2.git", "configv2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoSlug(tt.url)
			if got != tt.want {
				t.Errorf("repoSlug(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestDeterministicWorkflowName(t *testing.T) {
	tests := []struct {
		gateName string
		repoURL  string
		want     string
	}{
		{"wiki-lint", "https://github.com/tibrezus/harmostes.git", "wiki-lint-harmostes"},
		{"pr-review", "https://github.com/tibrezus/harmostes.git", "pr-review-harmostes"},
		{"fork-maintenance", "https://github.com/rezuscloud/dapr.git", "fork-maintenance-dapr"},
		{"noop", "smoke", "noop-smoke"},
		// Empty inputs
		{"", "https://github.com/x/y.git", ""},
		{"wiki-lint", "", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.gateName+"_"+tt.repoURL, func(t *testing.T) {
			got := deterministicWorkflowName(tt.gateName, tt.repoURL)
			if got != tt.want {
				t.Errorf("deterministicWorkflowName(%q, %q) = %q, want %q",
					tt.gateName, tt.repoURL, got, tt.want)
			}
		})
	}
}

func TestDeterministicWorkflowName_DNSSafe(t *testing.T) {
	// Full name must be DNS-safe (lowercase, hyphens, ≤63 chars).
	name := deterministicWorkflowName("pr-review", "https://github.com/some-org/a-very-long-repository-name-that-could-exceed-limits.git")
	if !workflowNameRe.MatchString(name) {
		t.Errorf("name %q is not DNS-safe", name)
	}
	if len(name) > maxWorkflowNameLen {
		t.Errorf("name %q exceeds %d chars (got %d)", name, maxWorkflowNameLen, len(name))
	}
}

func TestWorkflowTarget_SourceRepo(t *testing.T) {
	wf := v1alpha1.WorkflowSpec{
		Source: v1alpha1.SourceSpec{
			Repo: "https://github.com/tibrezus/harmostes.git",
		},
	}
	got := workflowTarget(wf)
	if got != "https://github.com/tibrezus/harmostes.git" {
		t.Errorf("workflowTarget() = %q, want the source repo URL", got)
	}
}

func TestWorkflowTarget_ReviewValidate_PrepareRepos(t *testing.T) {
	config, _ := json.Marshal(map[string]any{
		"repos": []string{"github.com/tibrezus/harmostes"},
		"label": "needs-review",
	})
	wf := v1alpha1.WorkflowSpec{
		Source: v1alpha1.SourceSpec{Repo: ""}, // no source repo
		Prepare: v1alpha1.PrepareSpec{
			Plugin: v1alpha1.PluginRef{Name: "pr-fetch"},
			Config: config,
		},
		Agent: v1alpha1.AgentSpec{
			Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review"}},
		},
	}
	got := workflowTarget(wf)
	if got != "github.com/tibrezus/harmostes" {
		t.Errorf("workflowTarget() = %q, want the prepare repos entry", got)
	}
}

func TestWorkflowTarget_WorkspaceRepoFallback(t *testing.T) {
	wf := v1alpha1.WorkflowSpec{
		Source: v1alpha1.SourceSpec{Repo: ""},
		WorkspaceRepo: &v1alpha1.WorkspaceRepoSpec{
			URL: "https://github.com/rezuscloud/llm-wiki.git",
		},
	}
	got := workflowTarget(wf)
	if got != "https://github.com/rezuscloud/llm-wiki.git" {
		t.Errorf("workflowTarget() = %q, want the workspace repo URL", got)
	}
}

func TestWorkflowTarget_EmptySpec(t *testing.T) {
	wf := v1alpha1.WorkflowSpec{}
	got := workflowTarget(wf)
	if got != "" {
		t.Errorf("workflowTarget() = %q, want empty", got)
	}
}

func TestExtractPrepareRepos(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		config, _ := json.Marshal(map[string]any{
			"repos": []string{"github.com/a/b", "github.com/c/d"},
		})
		repos := extractPrepareRepos(config)
		if len(repos) != 2 {
			t.Fatalf("got %d repos, want 2", len(repos))
		}
		if repos[0] != "github.com/a/b" {
			t.Errorf("repos[0] = %q", repos[0])
		}
	})
	t.Run("empty config", func(t *testing.T) {
		repos := extractPrepareRepos(nil)
		if repos != nil {
			t.Errorf("expected nil, got %v", repos)
		}
	})
	t.Run("no repos key", func(t *testing.T) {
		config, _ := json.Marshal(map[string]any{"label": "needs-review"})
		repos := extractPrepareRepos(config)
		if len(repos) != 0 {
			t.Errorf("expected empty, got %v", repos)
		}
	})
}

// TestDeterministicNameForExistingWorkflows verifies the convention produces
// the expected names for every workflow currently deployed in production.
func TestDeterministicNameForExistingWorkflows(t *testing.T) {
	tests := []struct {
		gate string
		repo string
		want string
	}{
		{"wiki-lint", "https://github.com/tibrezus/harmostes.git", "wiki-lint-harmostes"},
		{"wiki-lint", "https://github.com/rezuscloud/platform-website", "wiki-lint-platform-website"},
		{"wiki-lint", "https://github.com/rezuscloud/rezuscloud.git", "wiki-lint-rezuscloud"},
		{"wiki-lint", "https://github.com/rezuscloud/signoz.git", "wiki-lint-signoz"},
		{"wiki-lint", "https://codeberg.org/forgejo/forgejo", "wiki-lint-forgejo"},
		{"pr-review", "github.com/tibrezus/harmostes", "pr-review-harmostes"},
		{"pr-review", "github.com/rezuscloud/llm-wiki", "pr-review-llm-wiki"},
		{"pr-review", "github.com/tibrez/rhesadox", "pr-review-rhesadox"},
		{"noop", "smoke", "noop-smoke"},
	}
	for _, tt := range tests {
		got := deterministicWorkflowName(tt.gate, tt.repo)
		if got != tt.want {
			t.Errorf("deterministicWorkflowName(%q, %q) = %q, want %q",
				tt.gate, tt.repo, got, tt.want)
		}
	}
}

func TestTruncateMiddle(t *testing.T) {
	if got := TruncateMiddle("abcdefghij", 7); got != "abc…ij" {
		t.Errorf("TruncateMiddle = %q, want %q", got, "abc…ij")
	}
	if got := TruncateMiddle("short", 10); got != "short" {
		t.Errorf("short string should pass through, got %q", got)
	}
}
