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

image:
  controller: ghcr.io/tibrezus/harmostes-controller:0.6.0   # padded inline comment
  worker: ghcr.io/tibrezus/harmostes-worker:0.4.2
controller:
  resources:
    requests: { cpu: 100m, memory: 128Mi }

workflowTemplates:
  # Documentation sync — carefully worded comment that must survive.
  wiki-lint:
    description: Documentation sync
    deploy:
      plugin:
        name: git-push
  # PR review (speed-primary role, user directive 2026-09-08).
  # ROLLED BACK 01:5x — diagnosed: mtplx speed STALLS on long contexts.
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

tasks:
  arch-sync.txt: |
    You are working in a wiki repository (your current directory).

    These are DETERMINISTIC. Do NOT modify them.

      cat raw/arch/<project>/rig.json

    ## Step 5: Commit

      git add -A && git commit -m 'docs(arch-sync): <project>'
`

// TestSpliceTemplateIntoValues_ReplacesOnlyTheTarget pins the byte-range
// guarantee: everything OUTSIDE the edited entry survives VERBATIM — blank
// lines, padded inline comments, flow mappings, and block scalars included
// (a yaml.v3 tree re-marshal is lossy for all of these; caught live on
// harmostes-dev where the first proposal reformatted the whole file).
func TestSpliceTemplateIntoValues_ReplacesOnlyTheTarget(t *testing.T) {
	newSpec := v1alpha1.WorkflowTemplateSpec{}
	newSpec.Description = "PR review (edited)"
	newSpec.Agent.Model = "llama3:8b"
	newSpec.Agent.MaxFixes = 3

	out, oldSpec, newSpecText, err := spliceTemplateIntoValues(fixtureValuesFile, "workflowTemplates", "pr-review", newSpec)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}

	// The edit landed inside the entry.
	if !strings.Contains(out, "llama3:8b") || strings.Contains(out, "mistral-small-latest") {
		t.Error("the entry was not replaced")
	}
	if !strings.Contains(newSpecText, "llama3:8b") || strings.Contains(newSpecText, `"""`) {
		t.Errorf("new canonical spec wrong or noisy: %q", newSpecText)
	}
	if !strings.Contains(oldSpec, "mistral-small-latest") {
		t.Errorf("old canonical spec = %q, want the committed model", oldSpec)
	}

	// BYTE-IDENTITY of untouched regions: every line before the entry and
	// every line from the next entry on must be unchanged. Assert the
	// strongest form available: the untouched features appear EXACTLY as
	// in the source (same spacing, same blank lines, same block scalar).
	untouched := []string{
		"# Chart values — the archetype registry the UI discovers.",
		"  controller: ghcr.io/tibrezus/harmostes-controller:0.6.0   # padded inline comment",
		"    requests: { cpu: 100m, memory: 128Mi }",
		"  # Documentation sync — carefully worded comment that must survive.",
		"  # ROLLED BACK 01:5x — diagnosed: mtplx speed STALLS on long contexts.",
		"  fork-maintenance:",
		"      skill: /skills/fork/SKILL.md",
		"tasks:",
		"  arch-sync.txt: |",
		"    You are working in a wiki repository (your current directory).",
		"",
		"    These are DETERMINISTIC. Do NOT modify them.",
	}
	for _, want := range untouched {
		if !strings.Contains(out, want) {
			t.Errorf("untouched region lost or reformatted: %q", want)
		}
	}
	// yaml.v3 re-marshal damage markers must NOT appear.
	for _, damage := range []string{
		"{cpu: 100m}",                      // flow-map spacing normalized
		"arch-sync.txt: \"You are working", // block scalar collapsed to a quoted string
		"0.6.0 # padded",                   // comment padding squeezed
	} {
		if strings.Contains(out, damage) {
			t.Errorf("untouched region was reformatted: %q appeared", damage)
		}
	}
	// Structural sanity: the new entry sits between wiki-lint and
	// fork-maintenance, properly indented under workflowTemplates.
	wikiIdx := strings.Index(out, "  wiki-lint:")
	prIdx := strings.Index(out, "  pr-review:")
	forkIdx := strings.Index(out, "  fork-maintenance:")
	if wikiIdx < 0 || wikiIdx >= prIdx || prIdx >= forkIdx {
		t.Errorf("entry ordering broken: wiki@%d pr@%d fork@%d", wikiIdx, prIdx, forkIdx)
	}
	if !strings.Contains(out, "  pr-review:\n    agent:") {
		t.Errorf("new entry not at the file's 2-space entry indent:\n%s", out)
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
