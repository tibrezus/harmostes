package worker

// #725: the research layer's writer is the post-review deploy node —
// the agent stages orientation notes at WORKDIR/research-out.md and the
// pipeline owns the store write (provenance frontmatter, atomic
// rename, <PR>-<sha8>.md unique per head). This golden test extracts
// the SHIPPED publish program between RESEARCH-PUBLISH-START/END
// markers and runs it exactly the way post-review.sh wires it — the
// same pattern as TestPostReviewCoverageLedger, so the block cannot
// drift from what runs in production.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractResearchPublish pulls the PYRES heredoc body out of the
// RESEARCH-PUBLISH block in plugins/post-review/post-review.sh.
func extractResearchPublish(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatalf("read post-review.sh: %v", err)
	}
	script := string(raw)
	start := strings.Index(script, "# RESEARCH-PUBLISH-START")
	end := strings.Index(script, "# RESEARCH-PUBLISH-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("RESEARCH-PUBLISH markers not found in plugins/post-review/post-review.sh — keep the research publish block between the markers so the golden test runs the shipped code")
	}
	seg := script[start:end]
	pyStart := strings.Index(seg, "<< 'PYRES'")
	pyEnd := strings.LastIndex(seg, "\nPYRES")
	if pyStart < 0 || pyEnd < 0 || pyEnd <= pyStart {
		t.Fatal("research publish block lost its PYRES heredoc shape")
	}
	bodyStart := strings.Index(seg[pyStart:], "\n")
	if bodyStart < 0 {
		t.Fatal("PYRES opener line not newline-terminated")
	}
	return seg[pyStart+bodyStart+1 : pyEnd+1]
}

// runResearchPublish runs the extracted program the way the script
// wires it: argv = research-out.md, store leaf, PR, workflow, repo —
// with review.json sitting BESIDE research-out.md (the program reads
// the round's head from the review artifact).
func runResearchPublish(t *testing.T, workDir, storeLeaf, prNum, workflow, repo string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 missing in CI — the research publish is the layer's writer and must be tested")
		}
		t.Skip("python3 not available locally")
	}
	script := filepath.Join(workDir, "researchpub.py")
	if err := os.WriteFile(script, []byte(extractResearchPublish(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(workDir, "research-out.md")
	cmd := exec.Command(py, script, outPath, storeLeaf, prNum, workflow, repo)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("research publish crashed: %v — %s", err, out)
	}
	return string(out)
}

func writeRound(t *testing.T, dir, sha, notes string) {
	t.Helper()
	if sha != "" {
		if err := os.WriteFile(filepath.Join(dir, "review.json"), []byte(`{"decision":"REQUEST_CHANGES","reviewed_sha":"`+sha+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(dir, "review.json"), []byte(`{"decision":"APPROVE"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "research-out.md"), []byte(notes), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPostReviewResearchPublish(t *testing.T) {
	const sha40 = "0cff1b2aaabbccddeeff00112233445566778899"

	t.Run("notes published with provenance at the PR+head name", func(t *testing.T) {
		dir := t.TempDir()
		store := t.TempDir()
		leaf := filepath.Join(store, "_shared", "research")
		writeRound(t, dir, sha40, "# Orientation\n- harness lives in performance.yml\n")
		out := runResearchPublish(t, dir, leaf, "2832", "pr-review-rhesadox", "git.rezus.cloud/tibrez/rhesadox")
		if !strings.Contains(out, "research published") {
			t.Fatalf("publish must log its target, got: %s", out)
		}
		target := filepath.Join(leaf, "2832-0cff1b2a.md")
		b, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("store note missing: %v", err)
		}
		s := string(b)
		for _, needle := range []string{
			"repo: git.rezus.cloud/tibrez/rhesadox",
			"workflow: pr-review-rhesadox",
			`pr: "2832"`,
			"head_sha: " + sha40,
			"written_at: 20",
			"harness lives in performance.yml",
			strings.Repeat("-", 3), // frontmatter fences
		} {
			if !strings.Contains(s, needle) {
				t.Errorf("published note lost %q:\n%s", needle, s)
			}
		}
		// no temp leftovers: atomic means either the file or nothing
		ents, _ := os.ReadDir(leaf)
		if len(ents) != 1 {
			t.Fatalf("exactly one store artifact expected, got %d", len(ents))
		}
	})

	t.Run("idempotent same-head re-arm overwrites its own file", func(t *testing.T) {
		dir := t.TempDir()
		leaf := filepath.Join(t.TempDir(), "_shared", "research")
		writeRound(t, dir, sha40, "first take\n")
		runResearchPublish(t, dir, leaf, "2832", "w", "r")
		writeRound(t, dir, sha40, "second take — refreshed notes\n")
		runResearchPublish(t, dir, leaf, "2832", "w", "r")
		b, err := os.ReadFile(filepath.Join(leaf, "2832-0cff1b2a.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "second take") || strings.Contains(string(b), "first take") {
			t.Fatalf("same-head re-arm must replace its own note, got:\n%s", b)
		}
		ents, _ := os.ReadDir(leaf)
		if len(ents) != 1 {
			t.Fatalf("overwrite must not accumulate files, got %d", len(ents))
		}
	})

	t.Run("different heads append — the layer accumulates history", func(t *testing.T) {
		dir := t.TempDir()
		leaf := filepath.Join(t.TempDir(), "_shared", "research")
		writeRound(t, dir, "1111111111111111111111111111111111111111", "old head notes\n")
		runResearchPublish(t, dir, leaf, "2831", "w", "r")
		writeRound(t, dir, sha40, "new head notes\n")
		runResearchPublish(t, dir, leaf, "2832", "w", "r")
		for _, name := range []string{"2831-11111111.md", "2832-0cff1b2a.md"} {
			if _, err := os.Stat(filepath.Join(leaf, name)); err != nil {
				t.Errorf("expected %s to exist: %v", name, err)
			}
		}
	})

	t.Run("no head sha falls back to the nosha name, never crashes", func(t *testing.T) {
		dir := t.TempDir()
		leaf := filepath.Join(t.TempDir(), "_shared", "research")
		writeRound(t, dir, "", "notes without a head\n")
		runResearchPublish(t, dir, leaf, "9", "w", "r")
		if _, err := os.Stat(filepath.Join(leaf, "9-nosha.md")); err != nil {
			t.Fatalf("nosha fallback name missing: %v", err)
		}
	})

	t.Run("empty or whitespace notes publish nothing", func(t *testing.T) {
		dir := t.TempDir()
		leaf := filepath.Join(t.TempDir(), "_shared", "research")
		writeRound(t, dir, sha40, "   \n\t\n")
		out := runResearchPublish(t, dir, leaf, "2832", "w", "r")
		if strings.Contains(out, "research published") {
			t.Fatal("vacuous notes must not publish")
		}
		if ents, _ := os.ReadDir(leaf); len(ents) != 0 {
			t.Fatalf("vacuous notes must leave the store empty, got %d", len(ents))
		}
	})
}

// The reader-side pin: workspace.sh must surface the store's
// _shared/research into the workspace (the task text teaches
// /workspace/research — if the link block drifts or is dropped, the
// text teaches a path that does not exist).
func TestWorkspaceResearchLink(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "workspace", "workspace.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "# RESEARCH-LINK-START")
	end := strings.Index(script, "# RESEARCH-LINK-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("RESEARCH-LINK markers not found in plugins/workspace/workspace.sh — keep the research link block between the markers")
	}

	runBlock := func(t *testing.T, workDir, store string) string {
		t.Helper()
		bash, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("bash not available")
		}
		block := "set -euo pipefail\nlog() { echo \"[workspace] $*\"; }\nWORKDIR=\"${HARMOSTES_WORKDIR:-/workspace}\"\n" + script[start:end] + "\n"
		cmd := exec.Command(bash, "-c", block)
		cmd.Env = append(os.Environ(),
			"HARMOSTES_WORKDIR="+workDir,
			"HARMOSTES_SOL_PI_STORE="+store,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("research link block failed: %v — %s", err, out)
		}
		return string(out)
	}

	t.Run("store declared: leaf created, workspace linked at it", func(t *testing.T) {
		work := t.TempDir()
		store := t.TempDir()
		if err := os.WriteFile(filepath.Join(store, "somefile"), []byte("x"), 0o644); err != nil {
			t.Fatal(err) // store exists but NO _shared yet — the block must create it
		}
		out := runBlock(t, work, store)
		target, err := os.Readlink(filepath.Join(work, "research"))
		if err != nil {
			t.Fatalf("/workspace/research not linked: %v (%s)", err, out)
		}
		if want := filepath.Join(store, "_shared", "research"); target != want {
			t.Fatalf("link = %q, want %q", target, want)
		}
		if !strings.Contains(out, "research layer linked") {
			t.Errorf("the arm must log the link (config facts never drop silently): %s", out)
		}
	})

	t.Run("no store env: cold round, no link, no crash", func(t *testing.T) {
		work := t.TempDir()
		block := "set -euo pipefail\nlog() { echo \"[workspace] $*\"; }\nWORKDIR=\"${HARMOSTES_WORKDIR:-/workspace}\"\n" + script[start:end] + "\n"
		cmd := exec.Command("bash", "-c", block)
		cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+work)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("undeclared store must be a clean no-op: %v — %s", err, out)
		}
		if _, err := os.Lstat(filepath.Join(work, "research")); !os.IsNotExist(err) {
			t.Fatal("undeclared store must not create the link")
		}
	})

	// The staging path is cleaned per round: a stale research-out.md
	// from a previous round must never republish (source pin — the
	// cleanup line runs before the block re-links).
	if !strings.Contains(script, `"$WORKDIR/research-out.md"`) {
		t.Fatal("workspace.sh must clear research-out.md per round — the publish reads only THIS round's notes")
	}
}
