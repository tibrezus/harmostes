package worker

// TestPostReviewGateClassifier runs the GATE-CLASSIFIER block extracted
// VERBATIM from plugins/post-review/post-review.sh (the shipped heredoc,
// not a copy) against golden fixtures: fixture comments JSON on stdin →
// expected decision on stdout. This is the guard the r26 review demanded
// (P8: two consecutive rounds shipped gate-logic bugs with zero shell
// coverage — S1 trailer, S2 host dialect, the r24 dead conjunct).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ghResolvedOnly     = `[{"id":21,"path":"r.go","line":3,"commit_id":"OLD","in_reply_to":null,"resolved":true}]`
	ghUnresolvedMarked = `[{"id":31,"path":"u.go","line":7,"commit_id":"OLD","in_reply_to":null,"resolved":false}]`
	ghNullCommit       = `[{"id":41,"path":"n.go","line":2,"commit_id":null,"in_reply_to":null}]`
	ghOpenPrior        = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null},{"id":2,"path":"b.go","line":20,"commit_id":"OLD","in_reply_to":1}]`
	ghUnreplied        = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null}]`
	ghOpenCur          = `[{"id":1,"path":"a.go","line":10,"commit_id":"CUR","in_reply_to":null}]`
	forgejoDialect     = `[{"id":7,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":null},{"id":8,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":7}]`
	gitlabOpen         = `[{"id":12,"path":"z.go","line":6,"commit_id":"OLD","resolvable":true,"resolved":false}]`
	gitlabResolved     = `[{"id":11,"path":"y.go","line":5,"commit_id":"OLD","resolvable":true,"resolved":true}]`
)

// wantOpen mirrors the classifier's stdout count line (printed BEFORE the
// decision): the downgrade verdict line must quote THIS number — two
// sources for "how many prior-round threads are open" is how the verdict
// starts lying (r7 review of 2e16f0c5: "0 blocking findings" on a
// downgrade).

func extractClassifier(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatalf("read post-review.sh: %v", err)
	}
	script := string(raw)
	start := strings.Index(script, "# GATE-CLASSIFIER-START")
	end := strings.Index(script, "# GATE-CLASSIFIER-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("GATE-CLASSIFIER markers not found in plugins/post-review/post-review.sh — keep the classifier between the markers so the golden test runs the shipped code")
	}
	return script[start:end]
}

func TestPostReviewGateClassifier(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		sha      string
		want     string
		wantOpen string // the stdout count line ("" = don't assert)
	}{
		{"github: replied prior thread is closed", ghOpenPrior, "CUR", "APPROVE", "0"},
		{"github: unreplied prior thread downgrades", ghUnreplied, "CUR", "REQUEST_CHANGES", "1"},
		{"github: current-round thread does not downgrade", ghOpenCur, "CUR", "APPROVE", "0"},
		{"forgejo in_reply_to dialect closes threads", forgejoDialect, "OLD", "APPROVE", "0"},
		{"gitlab: unresolved prior discussion downgrades", gitlabOpen, "CUR", "REQUEST_CHANGES", "1"},
		{"gitlab: resolved prior discussion passes", gitlabResolved, "CUR", "APPROVE", "0"},
		{"github: resolved-but-unreplied thread is CLOSED (C1)", ghResolvedOnly, "CUR", "APPROVE", "0"},
		{"github: marked-unresolved unreplied thread downgrades", ghUnresolvedMarked, "CUR", "REQUEST_CHANGES", "1"},
		{"github: commit_id-less root never downgrades (C4)", ghNullCommit, "CUR", "APPROVE", "0"},
		{"null payload (fetch failed) fails open", `null`, "CUR", "APPROVE", "0"},
	}
	// r29 P4-1 regression lock: the production invocation must feed the
	// classifier its program via -c and the JSON via stdin. The old wiring
	// (`python3 - <<EOF` heredoc) clobbered the pipe's stdin and crashed the
	// gate on every APPROVE round while all stdin-wired goldens stayed green.
	t.Run("production wiring: program via -c, JSON via stdin", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("python3 missing in CI — the gate classifier is the merge currency and must be tested")
			}
			t.Skip("python3 not available locally")
		}
		raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
		if err != nil {
			t.Fatal(err)
		}
		script := string(raw)
		// The CLASSIFIER line must pipe JSON into `python3 -c` — never into
		// `python3 -` with a heredoc (the heredoc IS the program; the pipe's
		// stdin would be clobbered — r29 P4-1). The fetch heredoc is fine:
		// it reads the API, not stdin. (The wiring was renamed for the count
		// line — CLASS_OUT feeds both the decision and the open-thread count
		// — r7 review of 2e16f0c5; the PROPERTY locked here is unchanged.)
		inv := `CLASS_OUT=$(printf '%s' "$CS_JSON" | python3 -c "$CLASSIFIER_PY")`
		if !strings.Contains(script, inv) {
			t.Fatalf("classifier invocation drifted from the safe wiring (%q) — re-check the stdin wiring", inv)
		}
		if strings.Contains(script, "$CS_JSON\" | python3 - <<") {
			t.Fatal("classifier must not be invoked via a heredoc — the heredoc clobbers the piped stdin (r29 P4-1)")
		}
		cmd := exec.Command("bash", "-c",
			`printf '%s' "$CS_JSON" | python3 -c "$(sed -n '/GATE-CLASSIFIER-START/,/GATE-CLASSIFIER-END/p' "$0")"`,
			filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
		cmd.Stdin = strings.NewReader(ghOpenPrior)
		cmd.Env = append(os.Environ(), "CS_JSON="+ghOpenPrior, "REVIEWED_SHA=CUR")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("production wiring crashed: %v — %s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if lines[len(lines)-1] != "APPROVE" {
			t.Errorf("production wiring decision = %q, want APPROVE", lines[len(lines)-1])
		}
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			py, err := exec.LookPath("python3")
			if err != nil {
				if os.Getenv("CI") != "" {
					t.Fatalf("python3 missing in CI — the gate classifier is the merge currency and must be tested")
				}
				t.Skip("python3 not available locally")
			}
			script := filepath.Join(t.TempDir(), "classifier.py")
			if err := os.WriteFile(script, []byte(extractClassifier(t)), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(py, script)
			cmd.Stdin = strings.NewReader(tc.fixture)
			cmd.Env = append(os.Environ(), "REVIEWED_SHA="+tc.sha)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("classifier exited %v: %s", err, out)
			}
			// stdout: count line FIRST, decision LAST (diagnostics to stderr).
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if got := lines[len(lines)-1]; got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
			if tc.wantOpen != "" && lines[0] != tc.wantOpen {
				t.Errorf("open-thread count = %q, want %q (the downgrade verdict quotes this)", lines[0], tc.wantOpen)
			}
		})
	}
}
