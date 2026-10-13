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
