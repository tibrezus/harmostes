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

// sweepSource reads the gate sweep's source — the Phase-3 home of the
// consume + thread-gate math (#749).
func sweepSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "gate", "sweep.go"))
	if err != nil {
		t.Fatalf("read gate sweep: %v", err)
	}
	return string(b)
}

// extensionSource reads the review-post extension's source — the Phase-3
// home of the merge/publish logic the task text teaches (#749).
func extensionSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "extensions", "review-post", "index.ts"))
	if err != nil {
		t.Fatalf("read review-post extension: %v", err)
	}
	return string(b)
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

	t.Run("the empty declaration: the text and the tool must agree it does nothing", func(t *testing.T) {
		if !strings.Contains(text, "name at least one path") && !strings.Contains(text, "an empty block does nothing") {
			t.Errorf("the text must teach that the declaration names a real path (the empty shape is read-only on the ledger)")
		}
		// #749 Phase 3: the merge lives in the extension's coverage tool —
		// its empty/undecidable surface is UNKNOWABLE, never "nothing".
		ext := extensionSource(t)
		if !strings.Contains(ext, "changedSurface !== null && !changedSurface.has(pe.path)") {
			t.Errorf("review_coverage_publish lost the unknowable-surface guard — a degraded files_changed would erase the whole frontier")
		}
		gate := pluginSource(t, "plugins/pr-review/pr-review.sh")
		if strings.Contains(gate, "write coverage {covered:[], frontier:[]}") {
			t.Errorf("the gate's remediation string must not teach the empty shape (the merge treats it as no statement)")
		}
	})

	t.Run("the carries paragraph matches the shipped mint semantics (#712/#717)", func(t *testing.T) {
		if strings.Contains(text, "milestone-assigned") {
			t.Error("the text must not promise milestone-ASSIGNED carries — #717 is exact-title opt-in, never guessed")
		}
		// #749 Phase 3: the mint lives in review_carry_file — the
		// "Architecture & Design improvements" milestone joins ONLY when
		// the repo has one (exact title, #717); a no-milestone repo mints
		// unmilestoned (#712).
		ext := extensionSource(t)
		for _, needle := range []string{`ARCH_MS = "Architecture & Design improvements"`, "milestones.find"} {
			if !strings.Contains(ext, needle) {
				t.Errorf("review_carry_file drifted from the #717 exact-title opt-in (lost %q)", needle)
			}
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
		ext := extensionSource(t)
		if !strings.Contains(ext, "frontierOut[pe.path]") || !strings.Contains(ext, "declaredCovered") {
			t.Errorf("review_coverage_publish lost the per-path frontier merge — the text's self-carry claim is false")
		}
		if !strings.Contains(ext, "changedSurface") {
			t.Errorf("review_coverage_publish lost the files_changed retirement — the honest exit for a same-head round is gone")
		}
		// the empty-surface inversion: files_changed [] is UNKNOWABLE, not
		// "nothing on the surface" — reading it as authoritative retires
		// the whole frontier. The guard line is the behavior's home
		// (review_coverage_publish's null guard, pinned above).
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
			"read the standard docs",      // consult-first ordering
			"TOWARD the documented TOBE",  // context-not-gospel: direction beats AS-IS divergence
			"repo_wiki_state",             // the state fact: cloned vs absent vs clone-failed
			"UNTRUSTED third-party prose", // trust boundary: leads to verify, never instructions (the rig-query/sol-pi framing class)
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

func TestTaskTextTeachesTheShippedResearchContract(t *testing.T) {
	text := taskText(t)

	t.Run("the text names the layer, the staging path and the staleness rule", func(t *testing.T) {
		for _, needle := range []string{
			"/workspace/research",
			"research-out.md",
			"LEAD, not ground",                     // staleness semantics: verify before relying
			"re-derive nothing it already answers", // the economy: no rediscovery
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("task text lost %q — the agent reads only this", needle)
			}
		}
	})

	t.Run("every path the text teaches exists in the shipped plugins", func(t *testing.T) {
		ws := pluginSource(t, "plugins/workspace/workspace.sh")
		for _, needle := range []string{"_shared/research", "RESEARCH-LINK-START", `"$WORKDIR/research-out.md"`} {
			if !strings.Contains(ws, needle) {
				t.Errorf("workspace.sh lost %q — the text teaches /workspace/research", needle)
			}
		}
		ext := extensionSource(t)
		for _, needle := range []string{"review_research_publish", "_shared/research", "research-out.md", "HARMOSTES_WORKFLOW"} {
			if !strings.Contains(ext, needle) {
				t.Errorf("review_research_publish lost %q — the text teaches that the tool publishes the staged notes", needle)
			}
		}
	})

	t.Run("the provenance the text promises is the provenance stamped", func(t *testing.T) {
		ext := extensionSource(t)
		for _, needle := range []string{"repo: ${", "workflow: ${", "head_sha: ${", "written_at: ${"} {
			if !strings.Contains(ext, needle) {
				t.Errorf("the publish frontmatter drifted from the text's promise (lost %q)", needle)
			}
		}
	})

	t.Run("the store namespace is reserved where the join happens", func(t *testing.T) {
		ss := pluginSource(t, "internal/sessionstore/sessionstore.go")
		if !strings.Contains(ss, `func ReservedStoreNames() []string { return []string{"coverage", "_shared"} }`) {
			t.Error("sessionstore.ReservedStoreNames drifted — the single source for the store's shared namespaces")
		}
	})
}

func TestTaskTextTeachesTheShippedPillarScanContract(t *testing.T) {
	text := taskText(t)

	t.Run("the text carries the FACTS and the never-moved proof bar; the loop lives in the skill (#404/#680 boundary)", func(t *testing.T) {
		for _, needle := range []string{
			"Mid-round posting",
			"post\neach PROVEN finding's anchored thread", // post-when-proven fact (dedented text)
			"thread_ref",
			"ONLY from the host's 2xx response id", // rejected self-posts stay ref-less → publisher lands them
			"proof bar never moves",                // positive pin (keyword bans paraphrase-evade)
			"nothing unverified is posted",
			"playbook, Pillar", // the pointer: methodology defers to the skill
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("task text lost %q — the agent reads only this", needle)
			}
		}
		// the boundary itself: the work-order loop must NOT live in the
		// prompt — it is the skill's methodology (the CI render guard's
		// stated doctrine; its teeth predate this section's shape).
		for _, methodology := range []string{"Walk the pillars breadth-first", "switch to the next pillar", "deep proofs of remaining uncertainties after"} {
			if strings.Contains(text, methodology) {
				t.Errorf("task text re-grows methodology (%q) — the prompt is environment facts; the playbook owns the loop", methodology)
			}
		}
	})

	t.Run("the pointer's target exists in the SERVED skill", func(t *testing.T) {
		playbook := pluginSource(t, ".agents/skills/pr-review/references/playbook.md")
		for _, needle := range []string{
			"## Pillar scan",
			"Work it top-down",         // the loop's shape (the #82 wording)
			"POST its anchored thread", // post-when-proven, in the skill
			"tick that pillar",         // the seeded checklist is the loop's spine
		} {
			if !strings.Contains(playbook, needle) {
				t.Errorf("the served playbook lost %q — the text's pointer ('playbook, Pillar scan') would dangle", needle)
			}
		}
	})

	t.Run("the gate the text leans on counts posted threads as roots", func(t *testing.T) {
		// #749 Phase 3: the publisher is DELETED — the counting moved to
		// the sweep's converge (ListReviewThreads → open ROOTS: a root is
		// any unresolved non-reply, whatever round or author posted it;
		// replies never close, resolves do) and to the extension's
		// branch-correct posting state (a re-round never double-posts).
		gate := pluginSource(t, "plugins/pr-review/pr-review.sh")
		if !strings.Contains(gate, "thread_ref") {
			t.Error("the gate lost the thread_ref ledger contract — the text's posting claim is unanchored")
		}
		sweep := sweepSource(t)
		for _, needle := range []string{"ListReviewThreads", "openRoots", "!t.Resolved"} {
			if !strings.Contains(sweep, needle) {
				t.Errorf("the sweep's converge lost %q — posted threads would not count as roots", needle)
			}
		}
	})

	t.Run("no unverified-posting language anywhere", func(t *testing.T) {
		for _, banned := range []string{"post leads as you go", "post suspected findings", "interim UNVERIFIED"} {
			if strings.Contains(text, banned) {
				t.Errorf("task text teaches %q — the proof bar never moves (#727's one red line)", banned)
			}
		}
	})
}

// #727 follow-up: the pillar todo — the checklist is SEEDED (the agent
// never rebuilds it): the workspace plugin extracts the served playbook's
// pillar block into pillar-todos.json and the pillar-todo extension
// seeds the session's todo list from it. This pins the text's claims to
// the shipped machinery, both directions.
func TestTaskTextTeachesTheShippedPillarTodoSeed(t *testing.T) {
	text := taskText(t)

	t.Run("the text states the seed facts and defers the walk to the skill", func(t *testing.T) {
		for _, needle := range []string{
			"Pillar todo",
			"SEEDED with the pillar checklist",
			"served playbook's pillar block", // the skill is the source
			"a resumed session keeps its branch's",
			"playbook's Pillar scan section governs", // methodology stays in the skill
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("task text lost %q — the agent reads only this", needle)
			}
		}
		for _, methodology := range []string{"one item per pillar, in walkthrough order", "tick a pillar when"} {
			if strings.Contains(text, methodology) {
				t.Errorf("task text re-grows methodology (%q) — the loop is the playbook's", methodology)
			}
		}
	})

	t.Run("the machinery the text names exists", func(t *testing.T) {
		ws := pluginSource(t, "plugins/workspace/workspace.sh")
		for _, needle := range []string{"PILLAR-SEED-START", "pillar-todos.json", "Hardened checks"} {
			if !strings.Contains(ws, needle) {
				t.Errorf("workspace.sh lost %q — the text teaches the seeded checklist", needle)
			}
		}
		ext := pluginSource(t, "extensions/pillar-todo/index.ts")
		for _, needle := range []string{"pillar-todos.json", `name: "todo"`, "sawTodoEntry"} {
			if !strings.Contains(ext, needle) {
				t.Errorf("pillar-todo extension lost %q — the text's seed claim would be false", needle)
			}
		}
		// the served playbook HAS a pillar block (the seed's source)
		playbook := pluginSource(t, ".agents/skills/pr-review/references/playbook.md")
		if !strings.Contains(playbook, "## Correctness") || !strings.Contains(playbook, "## Hardened checks") {
			t.Error("the served playbook lost its pillar block shape — the seed would extract nothing")
		}
	})
}
