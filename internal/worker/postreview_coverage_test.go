package worker

// #704 coverage ledger: the DEPLOY-side merge runs between
// COVERAGE-MERGE-START/END markers in plugins/post-review/post-review.sh
// and is extracted VERBATIM — the shipped code, not a copy (the same
// golden discipline as the carry and gate classifiers).
//
// Contract under test:
//   - the round's coverage block merges into the per-PR ledger in the
//     shared store: covered accumulates by path (this round's declaration
//     wins; older entries for undeclared paths survive), the frontier is
//     REPLACED wholesale (the freshest statement of what remains),
//     rounds increments, last_reviewed_sha/findings_open update.
//   - an APPROVE with a declared frontier is downgraded to
//     REQUEST_CHANGES: review.json's decision is REWRITTEN (the verdict
//     compose and the merge status both read it) and the projection
//     speaks downgraded:true.
//   - no store env → the feature is off: no ledger write, no crash, the
//     projection still carries the downgrade (the APPROVE gate is not
//     store-dependent).
//   - an unwritable store degrades: WARN spoken, round continues,
//     carried:false in the projection — the carry is an optimization,
//     never a gate.
//   - frontier entries without a reason default to "not examined" in the
//     ledger (the workspace copies the ledger verbatim to the agent).
//   - the verdict compose reads the projection (COV_FILE) and appends the
//     coverage block: the covered/frontier counts, the bounded frontier
//     list, and the downgrade note; the trailer carries the FINAL
//     decision.
import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func extractCoverageMerge(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatalf("read post-review.sh: %v", err)
	}
	script := string(raw)
	start := strings.Index(script, "# COVERAGE-MERGE-START")
	end := strings.Index(script, "# COVERAGE-MERGE-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("COVERAGE-MERGE markers not found in plugins/post-review/post-review.sh — keep the coverage-merge block between the markers so the golden test runs the shipped code")
	}
	seg := script[start:end]
	// the block ships as a bash heredoc invocation; the python program is
	// the `python3 - "$REVIEW" ... << 'PYCOV' ... PYCOV` body.
	pyStart := strings.Index(seg, "<< 'PYCOV'")
	pyEnd := strings.LastIndex(seg, "\nPYCOV")
	if pyStart < 0 || pyEnd < 0 || pyEnd <= pyStart {
		t.Fatalf("coverage-merge block lost its PYCOV heredoc shape")
	}
	body := seg[pyStart+len("<< 'PYCOV'")+1 : pyEnd+1]
	return body
}

// runCoverageMerge runs the extracted program with REVIEW/COV_FILE/store
// env the way the script wires it, and returns the projection JSON.
func runCoverageMerge(t *testing.T, review map[string]any, storeDir string, prNum string) (proj map[string]any) {
	t.Helper()
	dir := t.TempDir()
	rj, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	reviewPath := filepath.Join(dir, "review.json")
	if err := os.WriteFile(reviewPath, rj, 0o644); err != nil {
		t.Fatal(err)
	}
	projPath := filepath.Join(dir, ".coverage-carry.json")
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 missing in CI — the coverage merge is the ledger's writer and must be tested")
		}
		t.Skip("python3 not available locally")
	}
	script := filepath.Join(dir, "covmerge.py")
	if err := os.WriteFile(script, []byte(extractCoverageMerge(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, script, reviewPath, projPath, storeDir, prNum)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coverage merge crashed: %v — %s", err, out)
	}
	pb, err := os.ReadFile(projPath)
	if err != nil {
		t.Fatalf("projection file not written: %v (out=%s)", err, out)
	}
	if err := json.Unmarshal(pb, &proj); err != nil {
		t.Fatalf("projection invalid JSON: %v (%s)", err, pb)
	}
	return proj
}

func readStoreLedger(t *testing.T, storeDir, prNum string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(storeDir, "coverage", prNum+".json"))
	if err != nil {
		t.Fatalf("store ledger missing: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("store ledger invalid JSON: %v", err)
	}
	return m
}

func TestPostReviewCoverageLedger(t *testing.T) {
	t.Run("merges into an existing ledger: accumulation + frontier replacement", func(t *testing.T) {
		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		// round 1: covered a.go; frontier c.go; last sha 1111
		r1 := map[string]any{
			"version": 1, "pr": 691, "rounds": 1, "last_reviewed_sha": "1111111111111111111111111111111111111111",
			"covered":       []any{map[string]any{"path": "a.go", "sha": "1111111111111111111111111111111111111111", "round": 1}},
			"frontier":      []any{map[string]any{"path": "c.go", "reason": "never reached"}},
			"findings_open": 2,
		}
		b, _ := json.Marshal(r1)
		if err := os.WriteFile(filepath.Join(store, "coverage", "691.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		// round 2: covers c.go (the old frontier) + redeclares a.go at the
		// new sha; new frontier d.go; findings resolved down to 1.
		proj := runCoverageMerge(t, map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": "2222222222222222222222222222222222222222",
			"findings":     []any{map[string]any{"path": "d.go", "body": "Blocking: x"}},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}, map[string]any{"path": "c.go", "note": "the old frontier, now read"}},
				"frontier": []any{map[string]any{"path": "d.go", "reason": "wall reached"}},
			},
		}, store, "691")
		if proj["carried"] != true || proj["rounds"] != float64(2) {
			t.Fatalf("projection must speak carried+rounds=2, got %v", proj)
		}
		l := readStoreLedger(t, store, "691")
		if l["last_reviewed_sha"] != "2222222222222222222222222222222222222222" {
			t.Errorf("last_reviewed_sha must move to the round's head, got %v", l["last_reviewed_sha"])
		}
		if l["findings_open"] != float64(1) {
			t.Errorf("findings_open must track the round's ledger, got %v", l["findings_open"])
		}
		covered, _ := l["covered"].([]any)
		paths := map[string]map[string]any{}
		for _, e := range covered {
			m := e.(map[string]any)
			paths[m["path"].(string)] = m
		}
		if len(paths) != 2 {
			t.Fatalf("covered must be exactly this round's declarations {a.go, c.go}, got %d: %v", len(paths), paths)
		}
		if paths["a.go"]["sha"] != "2222222222222222222222222222222222222222" {
			t.Errorf("a redeclared path must carry the NEW sha, got %v", paths["a.go"]["sha"])
		}
		if paths["a.go"]["round"] != float64(2) || paths["c.go"]["round"] != float64(2) {
			t.Errorf("declared entries must carry the new round number, got %v", paths)
		}
		if paths["c.go"]["note"] != "the old frontier, now read" {
			t.Errorf("notes must ride through, got %v", paths["c.go"])
		}
		frontier, _ := l["frontier"].([]any)
		if len(frontier) != 1 || frontier[0].(map[string]any)["path"] != "d.go" {
			t.Errorf("frontier must be REPLACED by this round's declaration (c.go examined this round), got %v", frontier)
		}
	})

	t.Run("fresh ledger is created at rounds=1", func(t *testing.T) {
		store := t.TempDir()
		proj := runCoverageMerge(t, map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": "3333333333333333333333333333333333333333",
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}},
				"frontier": []any{map[string]any{"path": "b.go"}},
			},
		}, store, "700")
		if proj["rounds"] != float64(1) {
			t.Fatalf("first merged round = 1, got %v", proj)
		}
		l := readStoreLedger(t, store, "700")
		frontier, _ := l["frontier"].([]any)
		if len(frontier) != 1 {
			t.Fatalf("frontier must round-trip, got %v", frontier)
		}
		if got := frontier[0].(map[string]any)["reason"]; got != "not examined" {
			t.Errorf("a frontier entry without a reason must default to 'not examined', got %v", got)
		}
	})

	t.Run("APPROVE with a frontier is downgraded and review.json is rewritten", func(t *testing.T) {
		store := t.TempDir()
		dir := t.TempDir()
		reviewPath := filepath.Join(dir, "review.json")
		rj, _ := json.Marshal(map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": "4444444444444444444444444444444444444444",
			"findings":     []any{},
			"coverage": map[string]any{
				"covered":  []any{map[string]any{"path": "a.go"}},
				"frontier": []any{map[string]any{"path": "b.go", "reason": "skipped by scope class"}},
			},
		})
		if err := os.WriteFile(reviewPath, rj, 0o644); err != nil {
			t.Fatal(err)
		}
		py, _ := exec.LookPath("python3")
		script := filepath.Join(dir, "covmerge.py")
		if err := os.WriteFile(script, []byte(extractCoverageMerge(t)), 0o600); err != nil {
			t.Fatal(err)
		}
		projPath := filepath.Join(dir, ".coverage-carry.json")
		out, err := exec.Command(py, script, reviewPath, projPath, store, "701").CombinedOutput()
		if err != nil {
			t.Fatalf("merge crashed: %v — %s", err, out)
		}
		var review map[string]any
		b, _ := os.ReadFile(reviewPath)
		if err := json.Unmarshal(b, &review); err != nil {
			t.Fatal(err)
		}
		if review["decision"] != "REQUEST_CHANGES" {
			t.Fatalf("the APPROVE must be rewritten to REQUEST_CHANGES in review.json (verdict compose + merge status read it), got %v", review["decision"])
		}
		var proj map[string]any
		pb, _ := os.ReadFile(projPath)
		_ = json.Unmarshal(pb, &proj)
		if proj["downgraded"] != true {
			t.Fatalf("projection must speak downgraded:true, got %v", proj)
		}
	})

	t.Run("no store env: feature off, no crash, the APPROVE gate still fires", func(t *testing.T) {
		proj := runCoverageMerge(t, map[string]any{
			"decision":     "APPROVE",
			"reviewed_sha": "5555555555555555555555555555555555555555",
			"findings":     []any{},
			"coverage":     map[string]any{"covered": []any{}, "frontier": []any{map[string]any{"path": "x.go"}}},
		}, "", "702")
		if proj["carried"] != false {
			t.Fatalf("nothing may be carried without a store, got %v", proj)
		}
		if proj["downgraded"] != true {
			t.Fatalf("the downgrade is store-independent (the artifact must never lie), got %v", proj)
		}
	})

	t.Run("unwritable store degrades: WARN, round continues, carried:false", func(t *testing.T) {
		// a FILE where the store's coverage/ dir must be — mkdir fails
		store := t.TempDir()
		blocker := filepath.Join(store, "coverage")
		if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
			t.Fatal(err)
		}
		proj := runCoverageMerge(t, map[string]any{
			"decision":     "REQUEST_CHANGES",
			"reviewed_sha": "6666666666666666666666666666666666666666",
			"findings":     []any{map[string]any{"path": "a.go", "body": "Blocking: x"}},
			"coverage":     map[string]any{"covered": []any{map[string]any{"path": "a.go"}}, "frontier": []any{}},
		}, store, "703")
		if proj["carried"] != false {
			t.Fatalf("an unwritable store must degrade to carried:false, got %v", proj)
		}
	})
}

// extractVerdictCompose pulls the VERDICT_COMMENT python heredoc (the one
// the shell runs to build the posted body) so the coverage projection is
// tested against the shipped composer.
func extractVerdictCompose(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	const opener = "VERDICT_COMMENT=$(python3 - << 'PYEOF'\n"
	start := strings.Index(script, opener)
	if start < 0 {
		t.Fatal("VERDICT_COMMENT heredoc not found")
	}
	rest := script[start+len(opener):]
	end := strings.Index(rest, "\nPYEOF\n")
	if end < 0 {
		t.Fatal("verdict heredoc terminator not found")
	}
	return rest[:end+1]
}

func TestPostReviewVerdictComposeCoverage(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 missing in CI — the verdict compose is the round's published record")
		}
		t.Skip("python3 not available locally")
	}
	run := func(t *testing.T, review, proj, threads map[string]any) string {
		t.Helper()
		dir := t.TempDir()
		write := func(name string, m map[string]any) string {
			b, _ := json.Marshal(m)
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}
		script := filepath.Join(dir, "verdict.py")
		if err := os.WriteFile(script, []byte(extractVerdictCompose(t)), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, script)
		cmd.Env = append(os.Environ(),
			"REVIEW="+write("review.json", review),
			"COV_FILE="+write("cov.json", proj),
			"THREADS_FILE="+write("threads.json", threads),
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("verdict compose crashed: %v — %s", err, out)
		}
		// the composer prints {"body": ...} — decode it so assertions run
		// on the POSTED body, not on json.dumps escaping (\u2014 etc).
		var posted struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &posted); err != nil {
			t.Fatalf("compose output not a body object: %v (%s)", err, out)
		}
		return posted.Body
	}

	t.Run("frontier-only round: no findings, the frontier IS the blocking statement", func(t *testing.T) {
		out := run(t,
			map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "aaaa", "findings": []any{},
				"coverage": map[string]any{"frontier": []any{
					map[string]any{"path": "big.go", "reason": "wall reached"},
					map[string]any{"path": "huge.go"},
				}}},
			map[string]any{"downgraded": false, "covered_now": 3, "frontier_now": 2, "carried": true, "rounds": 2, "covered_total": 5},
			map[string]any{})
		if !strings.Contains(out, "no new blocking findings; coverage incomplete: 2 file(s)") {
			t.Errorf("frontier-only round must say coverage is the blocking statement: %s", out)
		}
		if strings.Contains(out, "close them") {
			t.Errorf("'close them' would lie on a round with zero threads: %s", out)
		}
		if !strings.Contains(out, "**big.go** — wall reached") || !strings.Contains(out, "**huge.go** — not examined") {
			t.Errorf("the frontier list with defaulted reason must ride the body: %s", out)
		}
		// r7 briefness: the counts ride THE LINE (before the body blocks);
		// only data lists earn body newlines.
		if !strings.Contains(out, "frontier 2\n\nFrontier") && !strings.Contains(out, "coverage: 3 file(s) covered at this head, frontier 2") {
			t.Errorf("the coverage counts must ride the one-line verdict: %s", out)
		}
		if !strings.Contains(out, "<!-- pr-review: REQUEST_CHANGES @ aaaa -->") {
			t.Errorf("trailer must carry the final decision: %s", out)
		}
	})

	t.Run("downgraded APPROVE: the note explains the driver was coverage", func(t *testing.T) {
		out := run(t,
			map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "bbbb", "findings": []any{},
				"coverage": map[string]any{"frontier": []any{map[string]any{"path": "x.go"}}}},
			map[string]any{"downgraded": true, "covered_now": 1, "frontier_now": 1, "carried": false, "rounds": 1, "covered_total": 1},
			map[string]any{})
		if !strings.Contains(out, "Downgraded from APPROVE") {
			t.Errorf("a coverage-downgraded verdict must explain itself: %s", out)
		}
	})

	t.Run("zero findings without a declared frontier keeps the legacy line", func(t *testing.T) {
		// the R7-downgrade shape: a thread-driven REQUEST_CHANGES with no
		// coverage block must NOT inherit the frontier wording (nothing
		// was declared; "coverage incomplete: 0" would be noise).
		out := run(t,
			map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "dddd", "findings": []any{}},
			map[string]any{},
			map[string]any{})
		if strings.Contains(out, "coverage incomplete") {
			t.Errorf("no declared coverage — the legacy zero-findings line must stand: %s", out)
		}
		if strings.Contains(out, "; coverage: 0 file(s)") {
			t.Errorf("cov_declared must gate the counts clause (a coverage-less round carries no coverage noise): %s", out)
		}
		if !strings.Contains(out, "0 blocking findings posted as review threads; close them") {
			t.Errorf("legacy zero-findings wording must be preserved: %s", out)
		}
	})

	t.Run("no coverage data: legacy body untouched", func(t *testing.T) {
		out := run(t,
			map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "cccc",
				"findings": []any{map[string]any{"path": "a.go", "body": "x"}}},
			map[string]any{},
			map[string]any{})
		if strings.Contains(out, "coverage (round") {
			t.Errorf("absent coverage must compose the legacy body: %s", out)
		}
		if !strings.Contains(out, "1 blocking finding posted as review threads") {
			t.Errorf("legacy line must be unchanged: %s", out)
		}
	})
}
