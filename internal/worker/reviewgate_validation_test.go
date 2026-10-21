package worker

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #431-r7: the pr-review GATE validates review.json before the agent's
// feedback loop can continue. Its contract is the NEW one — no "body"
// field (the deploy builds the one-line verdict) — and it must tolerate a
// legacy body while enforcing decision/comments/sha. Found live: the old
// gate required a body the new contract forbids, stranding the rhesadox
// agent in a 4-attempt gate loop ("agent failed after 4 attempts").
//
// These tests run the real plugin script against fixture review.json files.
func runReviewGate(t *testing.T, review map[string]any, headSHA string) (string, int) {
	t.Helper()
	if _, err := os.ReadFile(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rj, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := map[string]any{"head_sha": headSHA}
	cj, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HARMOSTES_WORKDIR="+dir,
		"HARMOSTES_PR_HEAD_SHA="+headSHA,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("gate rejected: %s", out)
	}
	return string(out), exitCode(err)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

const sha40 = "deadbeef1234567890deadbeef1234567890dead"

func TestReviewGateAcceptsNewContract(t *testing.T) {
	// The #680 LEDGER contract: findings[] canonical (thread_ref optional —
	// absent = the deploy publishes the remainder). NO body field.
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"findings": []any{
			map[string]any{"path": "a.go", "line": 7, "body": "Blocking: MAJOR evidence"},
			map[string]any{"path": "b.go", "line": 9, "body": "Blocking: CRITICAL evidence",
				"thread_ref": "4242449509"},
		},
	}, sha40)
	if code != 0 {
		t.Fatalf("ledger review.json must pass the gate, rc=%d out=%s", code, out)
	}
}

func TestReviewGateAcceptsLegacyCommentsAlias(t *testing.T) {
	// comments[] = the pre-#680 alias; the gate normalizes it to findings.
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"comments": []any{
			map[string]any{"path": "a.go", "line": 7, "body": "Blocking: MAJOR evidence"},
		},
	}, sha40)
	if code != 0 {
		t.Fatalf("legacy alias must pass, rc=%d out=%s", code, out)
	}
	if !strings.Contains(out, "legacy comments") {
		t.Fatalf("legacy alias must WARN: %s", out)
	}
}

func TestReviewGateApproveWithoutCommentsPasses(t *testing.T) {
	out, code := runReviewGate(t, map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": sha40,
		"findings":     []any{},
	}, sha40)
	if code != 0 {
		t.Fatalf("clean APPROVE must pass, rc=%d out=%s", code, out)
	}
}

func TestReviewGateToleratesLegacyBody(t *testing.T) {
	// An old-habit agent writes a body + trailer: tolerated (ignored — the
	// deploy builds the one-line verdict), never a gate failure.
	out, code := runReviewGate(t, map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": sha40,
		"body":         "## Adversarial Review\n<!-- pr-review: APPROVE @ " + sha40 + " -->",
		"comments":     []any{},
	}, sha40)
	if code != 0 {
		t.Fatalf("legacy body must be tolerated, rc=%d out=%s", code, out)
	}
}

func TestReviewGateRejectsRequestChangesWithoutFindings(t *testing.T) {
	// Threads are the review: a REQUEST_CHANGES with no blocking findings
	// posts nothing and blocks nothing — it must not pass the gate.
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"comments":     []any{},
	}, sha40)
	if code == 0 {
		t.Fatalf("REQUEST_CHANGES without findings must be rejected, out=%s", out)
	}
	if !strings.Contains(out, "at least one blocking finding") {
		t.Errorf("rejection must name the missing findings, out=%s", out)
	}
}

func TestReviewGateRejectsSHAMismatch(t *testing.T) {
	out, code := runReviewGate(t, map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "cafebabe",
		"comments":     []any{},
	}, sha40)
	if code == 0 {
		t.Fatalf("stale reviewed_sha must be rejected, out=%s", out)
	}
}

func TestReviewGateAcceptsDispositionContract(t *testing.T) {
	// todos[] may carry disposition (fix-in-pr | architecture-carry) and an
	// optional string issue reference — the carry contract.
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"findings":     []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: x"}},
		"todos": []any{
			map[string]any{"path": "b.go", "line": 2, "body": "missing test scope"},
			map[string]any{"path": "c.go", "line": 3, "body": "router needs an ADR", "disposition": "architecture-carry", "issue": "#431"},
			map[string]any{"path": "d.go", "line": 4, "body": "proof registry split", "disposition": "fix-in-pr"},
		},
	}, sha40)
	if code != 0 {
		t.Fatalf("gate must accept the disposition contract, got exit %d:\n%s", code, out)
	}
}

func TestReviewGateRejectsBadDisposition(t *testing.T) {
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"findings":     []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: x"}},
		"todos": []any{
			map[string]any{"path": "b.go", "line": 2, "body": "x", "disposition": "carry-please"},
		},
	}, sha40)
	if code == 0 {
		t.Fatal("gate must reject an unknown disposition — the carry step dispatches on this field")
	}
	if !strings.Contains(out, "disposition") {
		t.Errorf("rejection must name the field, got:\n%s", out)
	}
}

func TestReviewGateRejectsNonStringIssue(t *testing.T) {
	out, code := runReviewGate(t, map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": sha40,
		"findings":     []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: x"}},
		"todos": []any{
			map[string]any{"path": "b.go", "line": 2, "body": "x", "disposition": "architecture-carry", "issue": 431},
		},
	}, sha40)
	if code == 0 {
		t.Fatal("gate must reject a non-string issue reference — the carry parses it as text")
	}
	_ = out
}

// #704: the coverage block is optional (legacy rounds pass untouched);
// present, it is shape-checked and it gates APPROVE — an approval over a
// declared frontier claims a full-surface review that the round itself
// denies. Symmetrically, a frontier-only REQUEST_CHANGES (no findings) is
// legitimate: the unfinished coverage IS the blocking statement.
func TestReviewGateCoverageContract(t *testing.T) {
	coverage := func(cov any) map[string]any {
		return map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage":     cov,
		}
	}

	t.Run("APPROVE with a declared frontier is refused", func(t *testing.T) {
		out, code := runReviewGate(t, coverage(map[string]any{
			"covered":  []any{map[string]any{"path": "a.go"}},
			"frontier": []any{map[string]any{"path": "b.go", "reason": "wall reached"}},
		}), sha40)
		if code == 0 {
			t.Fatal("APPROVE with a non-empty frontier must be refused — the surface was not fully reviewed")
		}
		if !strings.Contains(out, "frontier") {
			t.Errorf("refusal must name the frontier, got:\n%s", out)
		}
	})

	t.Run("APPROVE with complete coverage passes", func(t *testing.T) {
		out, code := runReviewGate(t, coverage(map[string]any{
			"covered":  []any{map[string]any{"path": "a.go"}, map[string]any{"path": "b.go"}},
			"frontier": []any{},
		}), sha40)
		if code != 0 {
			t.Fatalf("APPROVE over complete coverage must pass, rc=%d out=%s", code, out)
		}
	})

	t.Run("no coverage block passes (legacy shape)", func(t *testing.T) {
		out, code := runReviewGate(t, map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": sha40,
			"findings":     []any{},
		}, sha40)
		if code != 0 {
			t.Fatalf("legacy rounds must pass untouched, rc=%d out=%s", code, out)
		}
	})

	t.Run("frontier-only REQUEST_CHANGES is legitimate", func(t *testing.T) {
		dir := t.TempDir()
		review := map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}},
				"frontier": []any{map[string]any{"path": "b.go", "reason": "never reached"}},
			},
		}
		rj, _ := json.Marshal(review)
		if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
			t.Fatal(err)
		}
		cj, _ := json.Marshal(map[string]any{"head_sha": sha40})
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
			t.Fatal(err)
		}
		script, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
		cmd := exec.Command("bash", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("frontier-only REQUEST_CHANGES must pass, rc out=%s", out)
		}
		// the coverage block rides through the gate's rewrite (the deploy
		// and the coverage-merge both read it from review.json)
		b, _ := os.ReadFile(filepath.Join(dir, "review.json"))
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got["coverage"] == nil {
			t.Fatal("coverage block must survive the gate's rewrite (the deploy reads it)")
		}
	})

	t.Run("REQUEST_CHANGES with neither findings nor frontier is still refused", func(t *testing.T) {
		out, code := runReviewGate(t, map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage":     map[string]any{"covered": []any{map[string]any{"path": "a.go"}}, "frontier": []any{}},
		}, sha40)
		if code == 0 {
			t.Fatal("a REQUEST_CHANGES with nothing blocking and no frontier states nothing — refused")
		}
		_ = out
	})

	t.Run("malformed coverage entries are refused", func(t *testing.T) {
		for name, cov := range map[string]any{
			"non-object":                 "covered a.go",
			"covered entry without path": map[string]any{"covered": []any{map[string]any{"note": "x"}}},
			"frontier entry empty path":  map[string]any{"frontier": []any{map[string]any{"path": "  "}}},
			"covered not a list":         map[string]any{"covered": "a.go"},
		} {
			out, code := runReviewGate(t, coverage(cov), sha40)
			if code == 0 {
				t.Errorf("%s: malformed coverage must be refused (out=%s)", name, out)
			}
		}
	})
}

// #704 r1: two gate additions that make "APPROVE = coverage complete" and
// the ledger's integrity mechanical — (1) an APPROVE's coverage must
// account for the WHOLE changed surface (files_changed ⊆ covered ∪
// frontier); (2) once the workspace carried a ledger (pr-context carries
// the coverage fact block), a round that declares NOTHING would reset it
// in the deploy's merge — the declaration is mandatory exactly there.
func TestReviewGateCoverageCompletenessAndCarryMandatory(t *testing.T) {
	ctxWith := func(files ...string) map[string]any {
		fs := make([]any, len(files))
		for i, f := range files {
			fs[i] = map[string]any{"filename": f, "status": "modified"}
		}
		return map[string]any{"head_sha": sha40, "files_changed": fs}
	}
	run := func(review map[string]any, ctx map[string]any) (string, int) {
		dir := t.TempDir()
		rj, _ := json.Marshal(review)
		if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
			t.Fatal(err)
		}
		cj, _ := json.Marshal(ctx)
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
			t.Fatal(err)
		}
		script, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
		cmd := exec.Command("bash", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("rejected: %s", out)
		}
		return string(out), exitCode(err)
	}

	t.Run("APPROVE over a partial coverage declaration is refused", func(t *testing.T) {
		out, code := run(map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}},
				"frontier": []any{},
			},
		}, ctxWith("a.go", "b.go", "c.go"))
		if code == 0 {
			t.Fatal("APPROVE with 1 of 3 changed files declared must be refused")
		}
		if !strings.Contains(out, "2 changed file(s) neither covered nor frontiered") || !strings.Contains(out, "b.go") {
			t.Errorf("refusal must name the count and the missing files, got:\n%s", out)
		}
	})

	t.Run("APPROVE over the complete surface passes", func(t *testing.T) {
		out, code := run(map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}, map[string]any{"path": "b.go"}},
				"frontier": []any{map[string]any{"path": "c.go", "reason": "docs-only, skimmed"}},
			},
		}, ctxWith("a.go", "b.go", "c.go"))
		if code == 0 {
			t.Fatalf("the complete declaration must pass, rc=%d out=%s", code, out)
		}
	})

	t.Run("REQUEST_CHANGES may declare partial coverage honestly", func(t *testing.T) {
		_, code := run(map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}},
				"frontier": []any{},
			},
		}, ctxWith("a.go", "b.go"))
		if code != 0 {
			t.Fatal("partial coverage under REQUEST_CHANGES is honest — passes")
		}
	})

	t.Run("carry happened + no declaration is refused (the ledger-reset window)", func(t *testing.T) {
		ctx := ctxWith("a.go")
		ctx["coverage"] = map[string]any{"last_reviewed_sha": "1111", "rounds": 2}
		out, code := run(map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
		}, ctx)
		if code == 0 {
			t.Fatal("a carried round that declares nothing resets the ledger — refused")
		}
		if !strings.Contains(out, "declared no coverage block") {
			t.Errorf("refusal must name the missing declaration, got:\n%s", out)
		}
	})

	t.Run("carry happened + declared passes", func(t *testing.T) {
		ctx := ctxWith("a.go")
		ctx["coverage"] = map[string]any{"last_reviewed_sha": "1111", "rounds": 2}
		out, code := run(map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{},
			"coverage":     map[string]any{"covered": []any{}, "frontier": []any{map[string]any{"path": "a.go"}}},
		}, ctx)
		if code != 0 {
			t.Fatalf("a declared round over a carried ledger passes, rc=%d out=%s", code, out)
		}
	})

	t.Run("no carry + no declaration is the legacy shape (passes)", func(t *testing.T) {
		out, code := run(map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": sha40,
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
		}, ctxWith("a.go"))
		if code != 0 {
			t.Fatalf("legacy rounds (no carried ledger) are untouched, rc=%d out=%s", code, out)
		}
	})
}

// #704 r2: accumulated coverage counts as declared. A delta round that
// examines the frontier and the fix-diff must be able to APPROVE without
// re-declaring paths covered at older shas — the carried ledger's covered
// entries are declared by construction (staleness is the touched flag's
// contract). The carried FRONTIER is never inherited: "not examined" is
// not a state an APPROVE can inherit.
func TestReviewGateCoverageCountsCarriedLedger(t *testing.T) {
	dir := t.TempDir()
	// the carried ledger: a.go + b.go covered at older shas, c.go frontier
	lb, _ := json.Marshal(map[string]any{
		"version": 1, "rounds": 2, "last_reviewed_sha": "1111",
		"covered":  []any{map[string]any{"path": "a.go", "sha": "1111"}, map[string]any{"path": "b.go", "sha": "1111"}},
		"frontier": []any{map[string]any{"path": "c.go", "reason": "wall reached"}},
	})
	if err := os.WriteFile(filepath.Join(dir, "coverage-ledger.json"), lb, 0o644); err != nil {
		t.Fatal(err)
	}
	review := map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": sha40,
		"findings":     []any{},
		"coverage":     map[string]any{"covered": []any{map[string]any{"path": "c.go"}}, "frontier": []any{}},
	}
	rj, _ := json.Marshal(review)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	cj, _ := json.Marshal(map[string]any{
		"head_sha":      sha40,
		"files_changed": []any{map[string]any{"filename": "a.go"}, map[string]any{"filename": "b.go"}, map[string]any{"filename": "c.go"}},
	})
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugins", "pr-review", "pr-review.sh"))
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the delta-round APPROVE must pass: this round covered the old frontier (c.go); a.go/b.go are declared by the carried ledger — rc out=%s", out)
	}

	// the carried FRONTIER does not count as declared: drop this round's
	// c.go declaration → refused (an APPROVE cannot inherit "not examined")
	review["coverage"] = map[string]any{"covered": []any{}, "frontier": []any{}}
	rj, _ = json.Marshal(review)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir, "HARMOSTES_PR_HEAD_SHA="+sha40)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("a frontier path must be examined THIS round — an inherited 'not examined' cannot stand under an APPROVE")
	}
	if !strings.Contains(string(out), "c.go") {
		t.Errorf("refusal must name the frontier path, got:\n%s", out)
	}
}
