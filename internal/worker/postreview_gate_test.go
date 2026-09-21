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
	// A host shape the classifier cannot fully subscript (missing id —
	// the r7 review of 90a24e63 mutation-probed `KeyError: 'id'` on the
	// shipped block): must DEGRADE, never exit 1 — a crash here aborts
	// the deploy under set -e and wedges the head (#328). A readable
	// prior-round comment stays an open thread (C4); only unreadable
	// SHAPES (non-list, non-dict) are skipped entirely.
	ghTruncated = `[{"path":"a.go","line":1,"commit_id":"OLD"}]`
	ghOpenPrior = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null},{"id":2,"path":"b.go","line":20,"commit_id":"OLD","in_reply_to":1}]`
	ghUnreplied = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to":null}]`
	ghOpenCur   = `[{"id":1,"path":"a.go","line":10,"commit_id":"CUR","in_reply_to":null}]`
	// GitHub REST names the reply linkage in_reply_to_id (the live #467
	// loop: replies never closed threads under the in_reply_to-only read,
	// so every APPROVE downgraded on phantom open threads).
	ghRepliedRestField = `[{"id":1,"path":"a.go","line":10,"commit_id":"OLD","in_reply_to_id":2},{"id":2,"path":"b.go","line":20,"commit_id":"OLD","in_reply_to_id":null}]`
	forgejoDialect     = `[{"id":7,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":null},{"id":8,"path":"x.go","line":1,"commit_id":"OLD","in_reply_to":7}]`
	// #572 fork-gap addressal marker: the pr-review skill's documented
	// Forgejo protocol — a LATER same-anchor comment whose body references
	// the original comment id. Real shape from the rhesadox#2359 burn: the
	// fork serializes line=null with position=<diff anchor> for review
	// comments, and REST create-review cannot set in_reply_to at all.
	fjMarkerAddressed = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40305,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"w.yml:433 (comment 40268) — RESOLVED at abc: the coupled key re-armed"}]`
	// Same marker shape but anchored on a DIFFERENT conversation — must
	// not close (the anchor is part of the marker, not decoration).
	fjMarkerWrongAnchor = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40306,"path":"w.yml","line":null,"position":370,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"w.yml:370 (comment 40268) — reply for another thread"}]`
	// Same anchor, later, but the body never names the original id — a
	// same-anchor comment that does not identify its thread must not close
	// it (the id reference is the discriminator).
	fjMarkerNoIdRef = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40307,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"fixed at abc — the coupled key re-armed"}]`
	// #573 review: sibling findings at one anchor cross-referencing each
	// other ("see comment N") — one review batch, one commit_id, the
	// reviewer itself emits this shape. A mid-body id reference must NOT
	// close the sibling NOR erase the referencing finding from the count.
	// Expectation: BOTH stay open (2) — strictly conservative; the review
	// text sketched "1" under cross-reference-closes-A semantics, but the
	// leading-form rule treats cross-references as closure of NOTHING,
	// which still pins the blocking property (never 0/APPROVE) and never
	// undercounts unanswered findings.
	fjSiblingFindings = `[{"id":1,"path":"a.go","line":9,"commit_id":"OLD","in_reply_to":null,"created_at":"2026-09-21T01:00:00Z","body":"finding A"},{"id":2,"path":"a.go","line":9,"commit_id":"OLD","in_reply_to":null,"created_at":"2026-09-21T02:00:00Z","body":"finding B, see comment 1"}]`
	// The id appears mid-body (not the leading enumeration form) with the
	// leading form ABSENT — must not close via the marker even though the
	// id string is present somewhere in the body.
	fjMarkerMidBodyId = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40309,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"note: related to comment 40268 but this is a new finding"}]`
	// #573 review round 1 (CRITICAL): prefix-related ids at one anchor —
	// the real rhesadox#2359 shape (dense growing ids). A marker naming
	// 40268 must NOT close 4026 ("4026" is a substring of "40268" under
	// an id-anywhere predicate; the leading form's bounded token +
	// equality is the fix). Expected: 40268 closed + the marker consumed,
	// 4026 stays open → 1 / REQUEST_CHANGES.
	fjPrefixIdCollision = `[{"id":4026,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:00:00Z"},{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T02:00:00Z"},{"id":40305,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:00:00Z","body":"w.yml:433 (comment 40268) — fixed at abc"}]`
	// #573 review round 3: protocol-conformant shapes beyond the
	// parenthesized lead — the id as a bounded token anywhere in a body
	// that LEADS with the anchor's path:line (markdown emphasis tolerated).
	fjMarkerIdMidBodyLead = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40311,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"w.yml:433 — fixed at abc (see comment 40268)"}]`
	fjMarkerMarkdownLead  = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40312,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"**w.yml:433** (comment 40268) fixed"}]`
	// A path:line lead with NO id: cannot be attributed to one thread at a
	// shared anchor — must not close (documented divergence from the
	// reviewer's probe: id-required is what keeps the prefix-collision and
	// sibling-findings holes closed; the emitter's grammar is one regex,
	// stated in the classifier block and the skill).
	fjMarkerNoId = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z"},{"id":40313,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z","body":"w.yml:433 → fixed at abc1234"}]`
	// #573 review round 1 (latent): a +hh:mm offset must never sort
	// "later" lexically — 14:00+02:00 is EARLIER than 13:00Z but sorts
	// after it as text. Non-Z timestamps never close via the marker (both
	// lead shapes pinned).
	fjOffsetTimestamp  = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T13:00:00Z"},{"id":40310,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T14:00:00+02:00","body":"w.yml:433 (comment 40268) — fixed at abc"}]`
	fjOffsetTimestamp2 = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T13:00:00Z"},{"id":40314,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T14:00:00+02:00","body":"w.yml:433 — fixed at abc (comment 40268)"}]`
	// Marker EARLIER than the thread (created_at before) — a thread cannot
	// be addressed by something written before it existed.
	fjMarkerEarlier = `[{"id":40268,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T04:17:30Z"},{"id":40308,"path":"w.yml","line":null,"position":433,"commit_id":"OLD","in_reply_to":0,"created_at":"2026-09-21T01:39:33Z","body":"w.yml:433 (comment 40268) — resolved"}]`
	gitlabOpen      = `[{"id":12,"path":"z.go","line":6,"commit_id":"OLD","resolvable":true,"resolved":false}]`
	gitlabResolved  = `[{"id":11,"path":"y.go","line":5,"commit_id":"OLD","resolvable":true,"resolved":true}]`
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
		{"github: REST in_reply_to_id reply closes the thread", ghRepliedRestField, "CUR", "APPROVE", "0"},
		{"github: unreplied prior thread downgrades", ghUnreplied, "CUR", "REQUEST_CHANGES", "1"},
		{"github: current-round thread does not downgrade", ghOpenCur, "CUR", "APPROVE", "0"},
		{"forgejo in_reply_to dialect closes threads", forgejoDialect, "OLD", "APPROVE", "0"},
		{"forgejo: same-anchor id-referencing marker closes the thread (#572)", fjMarkerAddressed, "CUR", "APPROVE", "0"},
		{"forgejo: marker on a different anchor does not close — and itself stays an open comment (#572)", fjMarkerWrongAnchor, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: same-anchor reply without the id reference does not close — and itself stays an open comment (#572)", fjMarkerNoIdRef, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: sibling findings cross-referencing each other both stay open (#573 review)", fjSiblingFindings, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: mid-body id reference without the leading form does not close (#573 review)", fjMarkerMidBodyId, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: id mid-body under a path:line lead closes (#573 round 3)", fjMarkerIdMidBodyLead, "CUR", "APPROVE", "0"},
		{"forgejo: markdown emphasis on the path:line lead closes (#573 round 3)", fjMarkerMarkdownLead, "CUR", "APPROVE", "0"},
		{"forgejo: path:line lead with no id does not close (#573 round 3, documented divergence)", fjMarkerNoId, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: prefix-related ids at one anchor — marker closes only its named thread (#573 review)", fjPrefixIdCollision, "CUR", "REQUEST_CHANGES", "1"},
		{"forgejo: non-Z offset timestamps never close via the marker (#573 review)", fjOffsetTimestamp, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: non-Z offset timestamps never close via the marker — mid-body id variant (#573 review)", fjOffsetTimestamp2, "CUR", "REQUEST_CHANGES", "2"},
		{"forgejo: marker predating the thread does not close — and itself stays an open comment (#572)", fjMarkerEarlier, "CUR", "REQUEST_CHANGES", "2"},
		{"gitlab: unresolved prior discussion downgrades", gitlabOpen, "CUR", "REQUEST_CHANGES", "1"},
		{"gitlab: resolved prior discussion passes", gitlabResolved, "CUR", "APPROVE", "0"},
		{"github: resolved-but-unreplied thread is CLOSED (C1)", ghResolvedOnly, "CUR", "APPROVE", "0"},
		{"github: marked-unresolved unreplied thread downgrades", ghUnresolvedMarked, "CUR", "REQUEST_CHANGES", "1"},
		{"github: commit_id-less root never downgrades (C4)", ghNullCommit, "CUR", "APPROVE", "0"},
		{"github: field-truncated entry degrades, not dies", ghTruncated, "CUR", "REQUEST_CHANGES", "1"},
		{"github: non-list payload degrades, not dies", `{"message":"bad credentials"}`, "CUR", "APPROVE", "0"},
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
