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
		name    string
		fixture string
		sha     string
		want    string
	}{
		{"github: replied prior thread is closed", ghOpenPrior, "CUR", "APPROVE"},
		{"github: unreplied prior thread downgrades", ghUnreplied, "CUR", "REQUEST_CHANGES"},
		{"github: current-round thread does not downgrade", ghOpenCur, "CUR", "APPROVE"},
		{"forgejo in_reply_to dialect closes threads", forgejoDialect, "OLD", "APPROVE"},
		{"gitlab: unresolved prior discussion downgrades", gitlabOpen, "CUR", "REQUEST_CHANGES"},
		{"gitlab: resolved prior discussion passes", gitlabResolved, "CUR", "APPROVE"},
		{"github: resolved-but-unreplied thread is CLOSED (C1)", ghResolvedOnly, "CUR", "APPROVE"},
		{"github: marked-unresolved unreplied thread downgrades", ghUnresolvedMarked, "CUR", "REQUEST_CHANGES"},
		{"github: commit_id-less root never downgrades (C4)", ghNullCommit, "CUR", "APPROVE"},
		{"null payload (fetch failed) fails open", `null`, "CUR", "APPROVE"},
	}
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
			// stdout's LAST line is the decision (diagnostics go to stderr).
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if got := lines[len(lines)-1]; got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}
