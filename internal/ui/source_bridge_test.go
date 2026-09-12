package ui

import (
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The surgical values edit is the bridge's heart: everything except the one
// template key must survive byte-for-byte in spirit — comments, ordering,
// quoting, and unrelated templates included. A reformat-the-world edit
// would bury every review diff (and betray the "structured edits, never
// text diffs" principle at the file level).

const fixtureValuesFile = `# Chart values — the archetype registry the UI discovers.
# Change a template here, release the chart, Flux reconciles.
namespace: harmostes

workflowTemplates:
  # Documentation sync — carefully worded comment that must survive.
  wiki-lint:
    description: Documentation sync
    deploy:
      plugin:
        name: git-push
  # PR review (speed-primary role, user directive 2026-09-08).
  pr-review:
    description: PR review
    agent:
      model: mistral-small-latest
      gate:
        plugin:
          name: pr-review
  fork-maintenance:
    description: Fork sync
    agent:
      skill: /skills/fork/SKILL.md
`

func TestSpliceTemplateIntoValues_ReplacesOnlyTheTarget(t *testing.T) {
	newSpec := v1alpha1.WorkflowTemplateSpec{}
	newSpec.Description = "PR review (edited)"
	newSpec.Agent.Model = "llama3:8b"

	out, oldSpec, newSpecText, err := spliceTemplateIntoValues(fixtureValuesFile, "workflowTemplates", "pr-review", newSpec)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}

	// The target changed.
	if !strings.Contains(out, "llama3:8b") {
		t.Error("spliced file lacks the new model")
	}
	if strings.Contains(out, "mistral-small-latest") {
		t.Error("spliced file still carries the old model")
	}

	// Untouched regions survive verbatim: the header comment, the wiki-lint
	// comment and body, the fork-maintenance block.
	for _, want := range []string{
		"# Chart values — the archetype registry the UI discovers.",
		"# Documentation sync — carefully worded comment that must survive.",
		"wiki-lint:",
		"git-push",
		"fork-maintenance:",
		"/skills/fork/SKILL.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("spliced file lost %q", want)
		}
	}

	// The old and new canonical specs came back for the MR-body diff.
	if !strings.Contains(oldSpec, "mistral-small-latest") {
		t.Errorf("old spec = %q, want the committed model", oldSpec)
	}
	if !strings.Contains(newSpecText, "llama3:8b") {
		t.Errorf("new canonical spec = %q, want the edited model", newSpecText)
	}
	// Sparse: the canonical form carries no zero-value noise.
	if strings.Contains(newSpecText, `""`) {
		t.Errorf("new canonical spec carries empty-string noise: %q", newSpecText)
	}

	// Still valid YAML, still carries the other templates as mappings.
	if !strings.Contains(out, "workflowTemplates:") {
		t.Error("spliced file lost the values key")
	}
}

func TestSpliceTemplateIntoValues_Rejections(t *testing.T) {
	spec := v1alpha1.WorkflowTemplateSpec{}
	if _, _, _, err := spliceTemplateIntoValues(fixtureValuesFile, "workflowTemplates", "ghost", spec); err == nil {
		t.Error("unknown template: want drift error")
	}
	if _, _, _, err := spliceTemplateIntoValues(fixtureValuesFile, "otherKey", "pr-review", spec); err == nil {
		t.Error("missing values key: want error")
	}
	if _, _, _, err := spliceTemplateIntoValues("not: [valid", "workflowTemplates", "pr-review", spec); err == nil {
		t.Error("broken source file: want error")
	}
	if _, _, _, err := spliceTemplateIntoValues("- a\n- b\n", "workflowTemplates", "pr-review", spec); err == nil {
		t.Error("non-mapping source: want error")
	}
}

func TestParseTemplateSource(t *testing.T) {
	if ts, err := ParseTemplateSource(""); err != nil || ts != nil {
		t.Errorf("empty env: (%v, %v), want (nil, nil) — absent surface, never an error", ts, err)
	}
	ts, err := ParseTemplateSource(`{"host":"github","owner":"o","repo":"r","path":"p.yaml","valuesKey":"workflowTemplates"}`)
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if ts.BaseBranch != "main" {
		t.Errorf("BaseBranch default = %q, want main", ts.BaseBranch)
	}
	for _, bad := range []string{
		`{"host":"github","owner":"o"}`,                                 // missing repo/path/valuesKey
		`{"host":"","owner":"o","repo":"r","path":"p","valuesKey":"k"}`, // missing host
		`{not json}`,
	} {
		if _, err := ParseTemplateSource(bad); err == nil {
			t.Errorf("config %q: want error", bad)
		}
	}
}

func TestDiffBody_MarksChanges(t *testing.T) {
	body := diffBody("model: a\nskill: x\n", "model: b\nskill: x\n")
	if !strings.Contains(body, "- model: a") || !strings.Contains(body, "+ model: b") {
		t.Errorf("diff body = %q, want -/+ marked change", body)
	}
	if !strings.Contains(body, "  skill: x") {
		t.Errorf("diff body = %q, want context line", body)
	}
}
