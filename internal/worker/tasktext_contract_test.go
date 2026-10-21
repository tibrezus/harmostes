package worker

// #714/#716 r1: the reviewer's task text (chart/values.yaml
// tasks."pr-review.txt") teaches contracts the plugins enforce — and
// nothing pinned the text to the code, which is how stale wording
// (milestone-assigned carries, empty-declaration remediation) shipped
// while CI stayed green. This test reads the SHIPPED text and the
// SHIPPED plugins and pins the pair: the vocabulary the text names must
// exist in the code, the refusals the text promises must fire from the
// shipped gate, and neither surface may teach a shape the other
// contradicts.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// taskText extracts tasks."pr-review.txt" from chart/values.yaml (a
// literal block scalar at two-space indent, content at four).
func taskText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "chart", "values.yaml"))
	if err != nil {
		t.Fatalf("read chart/values.yaml: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "  pr-review.txt: |") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatal("tasks.\"pr-review.txt\" block not found in chart/values.yaml")
	}
	var out []string
	for _, ln := range lines[start:] {
		if strings.TrimSpace(ln) == "" {
			out = append(out, "")
			continue
		}
		if !strings.HasPrefix(ln, "    ") {
			break
		}
		out = append(out, strings.TrimPrefix(ln, "    "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func pluginSource(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGateScript(t *testing.T, dir string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestTaskTextTeachesTheShippedCoverageContract(t *testing.T) {
	text := taskText(t)
	if len(text) < 500 {
		t.Fatalf("task text extraction collapsed (%d bytes) — the extractor drifted from chart/values.yaml's shape", len(text))
	}

	t.Run("the vocabulary the text names exists in the shipped code", func(t *testing.T) {
		for _, needle := range []string{"coverage-ledger.json", "review-delta.patch", "touched"} {
			if !strings.Contains(text, needle) {
				t.Errorf("task text names %q — it must (the agent reads only this)", needle)
			}
		}
		carry := pluginSource(t, "plugins/workspace/coverage_carry.py")
		for _, needle := range []string{"coverage-ledger.json", "review-delta.patch", "touched_unknown"} {
			if !strings.Contains(carry, needle) {
				t.Errorf("coverage_carry.py lost %q — the text teaches it", needle)
			}
		}
	})

	t.Run("the empty declaration: the text and the deploy must agree it does nothing", func(t *testing.T) {
		if !strings.Contains(text, "name at least one path") && !strings.Contains(text, "an empty block does nothing") {
			t.Errorf("the text must teach that the declaration names a real path (the empty shape is read-only on the ledger)")
		}
		deploy := pluginSource(t, "plugins/post-review/post-review.sh")
		if !strings.Contains(deploy, "declared = cov_raw is not None and bool(covered_now or frontier_now)") {
			t.Errorf("the deploy's read-only gate for the empty shape drifted — the text's 'does nothing' claim is now false")
		}
		gate := pluginSource(t, "plugins/pr-review/pr-review.sh")
		if strings.Contains(gate, "write coverage {covered:[], frontier:[]}") {
			t.Errorf("the gate's remediation string must not teach the empty shape (the deploy treats it as no statement)")
		}
	})

	t.Run("the carries paragraph matches the shipped mint semantics (#712)", func(t *testing.T) {
		if strings.Contains(text, "milestone-assigned") {
			t.Error("the text must not promise milestone-assigned carries — #712 mints unmilestoned (post-review.sh: 'deliberately NOT fetched or assigned')")
		}
		deploy := pluginSource(t, "plugins/post-review/post-review.sh")
		if !strings.Contains(deploy, "milestone=None") {
			t.Errorf("post-review.sh drifted from unmilestoned mints — revisit the text with it")
		}
		for _, want := range []string{"EXCEPTION", "mint:"} {
			if !strings.Contains(text, want) {
				t.Errorf("the carries paragraph must carry the exception-gated mint semantics (#71/#712): missing %q", want)
			}
		}
	})

	t.Run("the refusals the text promises fire from the shipped gate", func(t *testing.T) {
		if !strings.Contains(text, "the gate refuses an undeclared round") {
			t.Errorf("the text must teach the undeclared-after-carry refusal (the contract it exists for)")
		}
		// every APPROVE refusal the text teaches is paired needle-for-needle
		// to the shipped gate's behavior over a carried ctx — deleting the
		// sentence from the text (or the guard from the gate) must turn
		// this test red, or the pair has drifted.
		pairs := []struct{ sentence, why string }{
			// the extractor dedents the block scalar — needles match the DEDENTED text
			{"claims the WHOLE surface: every changed\nfile must be covered or frontiered", "the completeness refusal (files_changed outside covered union frontier)"},
			{"and a declared frontier refuses\nthe APPROVE", "the frontier refusal (an APPROVE cannot inherit not-examined)"},
		}
		for _, p := range pairs {
			if !strings.Contains(text, p.sentence) {
				t.Errorf("the text dropped a promised refusal (%s): %q", p.why, p.sentence)
			}
		}
		// the frontier's self-carry semantics: the text teaches it, the
		// deploy implements it (per-path merge + files_changed retirement)
		// — both sides pinned.
		if !strings.Contains(text, "carries itself forward") {
			t.Errorf("the text must teach that the carried frontier persists until a round covers those paths (the same-head erasure hole)")
		}
		if strings.Contains(text, "would reset the ledger") || strings.Contains(text, "resets the ledger") {
			t.Errorf("the text claims an undeclared round resets the ledger — the shipped merge is READ-ONLY for undeclared rounds (r1); the rationale is 'advances nothing', not 'resets'")
		}
		deploy := pluginSource(t, "plugins/post-review/post-review.sh")
		if !strings.Contains(deploy, "declared_covered") || !strings.Contains(deploy, "per-path") {
			t.Errorf("the deploy lost the per-path frontier merge — the text's self-carry claim is false")
		}
		if !strings.Contains(deploy, "changed_surface") {
			t.Errorf("the deploy lost the files_changed retirement — the honest exit for a same-head round is gone")
		}
		// the empty-surface inversion: files_changed [] is UNKNOWABLE, not
		// "nothing on the surface" — reading it as authoritative retires
		// the whole frontier. The guard line is the behavior's home.
		if !strings.Contains(deploy, "if not changed_surface:") {
			t.Errorf("the deploy lost the empty-surface guard — a degraded files_changed would erase the whole frontier")
		}
		if !strings.Contains(text, "carries itself forward") || !strings.Contains(text, "until a round covers it") {
			t.Errorf("the self-carry sentence must name the retirement condition's shape")
		}
		// shipped-gate run 1: APPROVE over a carried ctx with an uncovered
		// changed file → refused
		dir1 := t.TempDir()
		writeJSON(t, filepath.Join(dir1, "review.json"), map[string]any{
			"decision": "APPROVE", "reviewed_sha": sha40, "findings": []any{},
			"coverage": map[string]any{"covered": []any{map[string]any{"path": "a.go"}}, "frontier": []any{}},
		})
		writeJSON(t, filepath.Join(dir1, "pr-context.json"), map[string]any{
			"head_sha": sha40, "coverage": map[string]any{"last_reviewed_sha": "1111", "rounds": 1},
			"files_changed": []any{map[string]any{"filename": "a.go"}, map[string]any{"filename": "b.go"}},
		})
		if out, err := runGateScript(t, dir1); err == nil {
			t.Fatalf("completeness: the shipped gate accepted an APPROVE with an uncovered changed file (%s) — the text's promise is false", out)
		}
		// shipped-gate run 2: APPROVE with a declared frontier → refused
		dir2 := t.TempDir()
		writeJSON(t, filepath.Join(dir2, "review.json"), map[string]any{
			"decision": "APPROVE", "reviewed_sha": sha40, "findings": []any{},
			"coverage": map[string]any{"covered": []any{map[string]any{"path": "a.go"}}, "frontier": []any{map[string]any{"path": "b.go"}}},
		})
		writeJSON(t, filepath.Join(dir2, "pr-context.json"), map[string]any{
			"head_sha": sha40, "coverage": map[string]any{"last_reviewed_sha": "1111", "rounds": 1},
			"files_changed": []any{map[string]any{"filename": "a.go"}, map[string]any{"filename": "b.go"}},
		})
		if out, err := runGateScript(t, dir2); err == nil {
			t.Fatalf("frontier: the shipped gate accepted an APPROVE with a declared frontier (%s) — the text's promise is false", out)
		}
		carried := map[string]any{"head_sha": sha40, "coverage": map[string]any{"last_reviewed_sha": "1111", "rounds": 2}}
		out, code := runReviewGate(t, map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
		}, sha40)
		_ = out
		// runReviewGate's fixture ctx has no coverage block — the refusal
		// needs the carried ctx; drive the shipped script directly:
		dir := t.TempDir()
		write := func(name string, v any) {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("review.json", map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
		})
		write("pr-context.json", carried)
		script, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
		cmd := exec.Command("bash", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
		gateOut, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("the shipped gate no longer refuses an undeclared round after carry — the text's promise is false (%s)", gateOut)
		}
		if code != 0 {
			t.Errorf("sanity: the legacy fixture (no carried ctx) must pass the gate — out=%s", gateOut)
		}
	})
}

// #726: the task text teaches the repository wiki — the reviewed repo's
// OWN architecture docs at /workspace/repo-wiki (distinct from
// /workspace/wiki, the cross-repo llm-wiki). The text promises a
// directory the prepare must clone (config.repoWiki) and a judging rule
// (toward-TOBE complies); this pins text↔code both ways.
func TestTaskTextTeachesTheShippedRepoWikiContract(t *testing.T) {
	text := taskText(t)

	t.Run("the text names the dir, the standard docs and the direction rule", func(t *testing.T) {
		for _, needle := range []string{
			"/workspace/repo-wiki",
			"DESIGN.md", "TOBE.md", "ARCHITECTURE.md",
			"read the standard docs",     // consult-first ordering
			"TOWARD the documented TOBE", // context-not-gospel: direction beats AS-IS divergence
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("task text lost %q — the agent reads only this", needle)
			}
		}
		if strings.Contains(text, "the docs override the diff") || strings.Contains(text, "reject any PR that deviates from the docs") {
			t.Error("the text must not teach docs-as-veto — toward-TOBE compliance is the rule")
		}
	})

	t.Run("the prepare clones what the text promises", func(t *testing.T) {
		ws := pluginSource(t, "plugins/workspace/workspace.sh")
		for _, needle := range []string{
			"REPO-WIKI-START",
			"config.repoWiki", // the declarative field
			"repo-wiki",       // the promised directory
			"host::https_url", // credential normalization beside the main clone's chain
		} {
			if !strings.Contains(ws, needle) {
				t.Errorf("workspace.sh lost %q — the text teaches /workspace/repo-wiki", needle)
			}
		}
		lib := pluginSource(t, "plugins/lib/git-host.sh")
		for _, needle := range []string{"ssh://git@*", "scp"} {
			if !strings.Contains(lib, needle) {
				t.Errorf("git-host.sh lost %q — declarative URLs arrive in operator shapes, not canonical https", needle)
			}
		}
	})
}
