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
	// A host shape the classifier cannot fully subscript (missing id):
	// must DEGRADE (skip), never exit 1 — a crash here aborts the deploy
	// under set -e and wedges the head. Unreadable shapes are skipped; a
	// false APPROVE re-reviews next round.
	ghTruncated = `[{"path":"a.go","line":1,"commit_id":"OLD"}]`
	// TOTAL counting (#2744 hole): a reply is NOT a close-out — the
	// resolve is. A replied-but-unresolved root stays OPEN no matter which
	// field name the host uses for the linkage.
	ghOpenPrior = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null},{"id":2,"path":"b.go","line":20,"commit_id":"OLD","in_reply_to":1}]`
	ghUnreplied = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null}]`
	// Same-round visibility (#2745 hole): the round's OWN open thread
	// counts — commit_id is irrelevant under total counting.
	ghOpenCur = `[{"id":1,"path":"a.go","line":10,"commit_id":"CUR","in_reply_to":null}]`
	// commit_id-less roots count like any root (round identity no longer
	// scopes anything).
	ghNullCommit       = `[{"id":41,"path":"n.go","line":2,"commit_id":null,"in_reply_to":null}]`
	ghRepliedRestField = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to_id":2},{"id":2,"path":"b.go","line":20,"commit_id":"OLD","in_reply_to_id":null}]`
	forgejoDialect     = `[{"id":7,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":null},{"id":8,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":7}]`
	// The retired marker grammar, kept as ONE shape: a protocol-shaped
	// addressal (path:line lead, id in body, different author, later) is a
	// REPLY — evidence on the record — and closes nothing. Only the
	// resolve closes.
	fjMarkerAddressed = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z","user":{"login":"harmostes-bot"}},{"id":40305,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","user":{"login":"tibrez"},"body":"w.yml:433 (comment 40268) — RESOLVED at abc: the coupled key re-armed"}]`
	fjSiblingFindings = `[{"id":1,"path":"a.go","line":9,"commit_id":"OLD","in_reply_to":null,"created_at":"2026-09-21T01:00:00Z","body":"finding A"},{"id":2,"path":"a.go","line":9,"commit_id":"OLD","in_reply_to":null,"created_at":"2026-09-21T02:00:00Z","body":"finding B, see comment 1"}]`
	// #579 REVERSED (the #2744 burn): stale leftovers from OLDER rounds DO
	// count under total counting — an answered-only-by-round-identity
	// thread is still open until someone resolves it. The old fixture
	// pinned newest-prior-round scoping; the scoping is what let round-1
	// blockers merge unanswered.
	fjStaleOlderRounds = `[{"id":1,"path":"a.yml","line":null,"position":10,"commit_id":"OLD1","in_reply_to":0,"created_at":"2026-09-20T01:00:00Z"},{"id":2,"path":"b.yml","line":null,"position":20,"commit_id":"OLD2","in_reply_to":0,"created_at":"2026-09-22T07:55:00Z","resolved":true}]`
	fjNewestPriorOpen  = `[{"id":1,"path":"a.yml","line":null,"position":10,"commit_id":"OLD1","in_reply_to":0,"created_at":"2026-09-20T01:00:00Z"},{"id":2,"path":"b.yml","line":null,"position":20,"commit_id":"OLD2","in_reply_to":0,"created_at":"2026-09-22T07:55:00Z"}]`
	// LIVE #2744 (10-11/12 October): round-1 BLOCKING findings replied to
	// via the web UI (in_reply_to-linked) but never resolved, newest round
	// clean — under resolve-only total counting the verdict that merged
	// this PR is impossible: 2 open roots downgrade it.
	live2744 = `[{"id":55286,"path":"rhesadox/proof/ownership.zig","line":null,"commit_id":"d4e41da551","in_reply_to":0,"resolver":null,"body":"Blocking (correctness / performance — unguarded Linux-only assumption)"},{"id":55287,"path":"rhesadox/proof/scoping.zig","line":null,"commit_id":"d4e41da551","in_reply_to":0,"resolver":null,"body":"Blocking (design intent / DRY — the new scoping lever is only half-wired)"},{"id":55418,"path":"rhesadox/proof/ownership.zig","line":null,"commit_id":"875127724a","in_reply_to":55286,"body":"Fixed at 87512772 — the finding was right"},{"id":55419,"path":"rhesadox/proof/scoping.zig","line":null,"commit_id":"875127724a","in_reply_to":55287,"body":"Residual re-scoped at 87512772"}]`
	// LIVE #2745: TODO threads born in the APPROVING round itself —
	// invisible to the old gate (listing ran pre-publish + commit_id
	// exclusion); under total counting they downgrade.
	live2745 = `[{"id":55537,"path":"rhesadox/ci/test-is-main-block.sh","line":null,"commit_id":"a1477bfb5b","in_reply_to":0,"resolver":null,"body":"**TODO** — missing complement; fix in this PR, reply+resolve: witness artifact"},{"id":55539,"path":"rhesadox/ci/main_matrix.issues","line":null,"commit_id":"a1477bfb5b","in_reply_to":0,"resolver":null,"body":"**TODO** — missing complement; fix in this PR, reply+resolve: matrix row varies the payload ref"}]`
	// The carry exemption: an open Architecture-carry root whose id the
	// carry step resolved this run (CARRY_RESOLVED) is closed — carried
	// items gate nothing.
	fjCarriedRoot  = `[{"id":900,"path":"docs/adr/0012.md","line":null,"commit_id":"OLD","in_reply_to":0,"resolver":null,"body":"**Architecture-carry** — the proof router needs an ADR before v2 (#512)"}]`
	gitlabOpen     = `[{"id":12,"path":"z.go","line":6,"commit_id":"OLD","resolvable":true,"resolved":false}]`
	gitlabResolved = `[{"id":11,"path":"y.go","line":5,"commit_id":"OLD","resolvable":true,"resolved":true}]`
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
		carried  string // CARRY_RESOLVED env (JSON id array); "" = unset
		want     string
		wantOpen string // the stdout count line ("" = don't assert)
	}{
		{"github: resolved thread is closed (resolve is the close-out)", ghResolvedOnly, "CUR", "", "APPROVE", "0"},
		{"github: unresolved root downgrades", ghUnresolvedMarked, "CUR", "", "REQUEST_CHANGES", "1"},
		{"github: reply does NOT close — replied root stays open (live #2744 shape)", ghOpenPrior, "CUR", "", "REQUEST_CHANGES", "1"},
		{"github: REST in_reply_to_id reply does NOT close either", ghRepliedRestField, "CUR", "", "REQUEST_CHANGES", "1"},
		{"github: unreplied prior thread downgrades", ghUnreplied, "CUR", "", "REQUEST_CHANGES", "1"},
		{"github: the round's OWN open thread counts (#2745 hole)", ghOpenCur, "CUR", "", "REQUEST_CHANGES", "1"},
		{"github: commit_id-less root counts (round identity is irrelevant)", ghNullCommit, "CUR", "", "REQUEST_CHANGES", "1"},
		{"forgejo dialect: linked reply does NOT close", forgejoDialect, "OLD", "", "REQUEST_CHANGES", "1"},
		// The fork cannot link replies via REST (the gap that birthed the marker
		// grammar): an addressal posts as a FLAT root. Under total counting
		// both the finding AND the unlinked addressal are open roots — each
		// needs its own resolve. Two listed, no magic consumption.
		{"forgejo: protocol-shaped addressal is an UNLINKED root — the finding and the addressal both stay open", fjMarkerAddressed, "CUR", "", "REQUEST_CHANGES", "2"},
		{"forgejo: sibling findings both stay open", fjSiblingFindings, "CUR", "", "REQUEST_CHANGES", "2"},
		{"forgejo: stale older-round leftovers DO count (#579 reversed — the #2744 burn)", fjStaleOlderRounds, "CUR", "", "REQUEST_CHANGES", "1"},
		{"forgejo: two open roots across rounds count as two", fjNewestPriorOpen, "CUR", "", "REQUEST_CHANGES", "2"},
		{"live #2744: round-1 blockers replied-but-unresolved downgrade the clean newest round", live2744, "7cf37b5c0c", "", "REQUEST_CHANGES", "2"},
		{"live #2745: TODOs born in the approving round downgrade it", live2745, "a1477bfb5b", "", "REQUEST_CHANGES", "2"},
		{"carry: a root resolved by this run's carry step is closed", fjCarriedRoot, "CUR", `["900"]`, "APPROVE", "0"},
		{"gitlab: unresolved prior discussion downgrades", gitlabOpen, "CUR", "", "REQUEST_CHANGES", "1"},
		{"gitlab: resolved prior discussion passes", gitlabResolved, "CUR", "", "APPROVE", "0"},
		{"github: field-truncated entry degrades, not dies", ghTruncated, "CUR", "", "APPROVE", "0"},
		{"github: non-list payload degrades, not dies", `{"message":"bad credentials"}`, "CUR", "", "APPROVE", "0"},
		{"null payload (fetch failed) fails open", `null`, "CUR", "", "APPROVE", "0"},
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
		inv := `CLASS_OUT=$(printf '%s' "$CS_JSON" | CARRY_RESOLVED="$CARRY_RESOLVED" python3 -c "$CLASSIFIER_PY")`
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
		if lines[len(lines)-1] != "REQUEST_CHANGES" {
			t.Errorf("production wiring decision = %q, want REQUEST_CHANGES (the replied-but-unresolved root is open under resolve-only)", lines[len(lines)-1])
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
			cmd.Env = append(os.Environ(), "REVIEWED_SHA="+tc.sha, "CARRY_RESOLVED="+tc.carried)
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
