package worker

// The thread-closure mandate's CARRY step, tested like the gate classifier:
// the CARRY-CLASSIFIER block is extracted VERBATIM from
// plugins/post-review/post-review.sh (between the markers) and run against
// a fake forge — the shipped code, not a copy.
//
// Contract under test:
//   - a ledger todo with disposition=architecture-carry and thread_ref:
//     mints ONE issue (title-deduped), joins the repo's architecture
//     tracker milestone when declared (#717), else NO milestone (#712 —
//     replies the issue reference on the thread, RESOLVES the thread, and
//     reports ledger_carried so the publisher never posts it.
//   - an open OR closed issue whose normalized title matches (class
//     prefixes / round annotations stripped, token+bigram similarity) is
//     REFERENCED, never re-minted — reworded rounds dedup too (#700).
//   - minting is EXCEPTION-gated (#700): an unmatched carry only mints
//     when the todo/thread body is severe/structural (severe|security|
//     data loss|outage|corruption, or an explicit "mint:" marker);
//     otherwise it resolves as a review-record carry with NO new issue.
//     Generated artifacts (arch-sync paths/commits) never mint at all.
//   - an agent-posted **Architecture-carry** thread discovered in the
//     listing is carried the same way (body #N reference wins over mint).
//   - a failed mint leaves the entry out of ledger_carried (the gate then
//     counts it open — a failed carry downgrades, never loses the thread).
//   - resolve failures are spoken in notes, not fatal.
//   - ledger_carried indices are FULL-LEDGER (#685 r1: a filtered-space
//     index inverts mixed ledgers — the carried item published, the
//     fix-in-pr item dropped).
//   - the GitHub dialect resolves via GraphQL resolveReviewThread, replies
//     via /comments/{id}/replies, milestone exact-title opt-in, dedups via
//     per_page — the fork verbs 404/422 on GitHub (#685 r1).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func extractCarryStep(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatalf("read post-review.sh: %v", err)
	}
	script := string(raw)
	start := strings.Index(script, "# CARRY-CLASSIFIER-START")
	end := strings.Index(script, "# CARRY-CLASSIFIER-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("CARRY-CLASSIFIER markers not found in plugins/post-review/post-review.sh — keep the carry step between the markers so the golden test runs the shipped code")
	}
	return script[start:end]
}

type carryForge struct {
	mu            sync.Mutex
	issues        []map[string]any
	milestones    []map[string]any
	minted        int
	resolved      []string
	replies       []map[string]any
	resolvedGH    []string // GraphQL resolveReviewThread thread ids (GitHub dialect)
	repliesGH     []string // /comments/{id}/replies paths (GitHub dialect)
	issuesQuery   string   // the dedup listing's RawQuery (dialect shape)
	mintPayloads  []map[string]any
	forceMintF    bool   // force issue-mint failure
	forceResF     bool   // force resolution failure
	ghThreadMap   string // reviewThreads map for the GitHub resolve path
	forceResGH404 bool   // GraphQL mutation answers 200-but-empty (resolve lost)
	msCalls       int    // /milestones endpoint hits (#712: must stay 0)
}

func (f *carryForge) mux(t *testing.T) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tibrez/rhesadox/milestones", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.msCalls++
		out, _ := json.Marshal(f.milestones)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			f.issuesQuery = r.URL.RawQuery
			out, _ := json.Marshal(f.issues)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case http.MethodPost:
			if f.forceMintF {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"title is reserved"}`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.minted++
			f.mintPayloads = append(f.mintPayloads, body)
			body["number"] = 400 + f.minted
			f.issues = append(f.issues, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(fmt.Sprint(body["query"]), "resolveReviewThread") {
			vars, _ := body["variables"].(map[string]any)
			tid, _ := vars["t"].(string)
			f.resolvedGH = append(f.resolvedGH, tid)
			if f.forceResGH404 {
				_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":null}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`))
			return
		}
		if f.ghThreadMap == "" {
			f.ghThreadMap = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`
		}
		_, _ = w.Write([]byte(f.ghThreadMap))
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.replies = append(f.replies, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/pulls/99/comments/", func(w http.ResponseWriter, r *http.Request) {
		// GitHub dialect: the documented reply route — in_reply_to is a
		// not-permitted key there (422, probed live #685 r1).
		if strings.HasSuffix(r.URL.Path, "/replies") && r.Method == http.MethodPost {
			f.mu.Lock()
			f.repliesGH = append(f.repliesGH, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/resolutions") && r.Method == http.MethodPut {
			f.mu.Lock()
			if f.forceResF {
				f.mu.Unlock()
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.resolved = append(f.resolved, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func runCarryStep(t *testing.T, srv *httptest.Server, listing string, carryEnv string, extraEnv ...string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	carryFile := filepath.Join(dir, "carry.json")
	script := filepath.Join(t.TempDir(), "carry.py")
	if err := os.WriteFile(script, []byte(extractCarryStep(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script)
	cmd.Stdin = strings.NewReader(listing)
	// IS_FJ=true is the fixture default (fork dialect); GitHub-dialect
	// tests override it with "IS_FJ=false" in extraEnv.
	cmd.Env = append(os.Environ(),
		"CARRY_ENV="+carryEnv,
		"CARRY_JSON_FILE="+carryFile,
		"API_BASE="+srv.URL,
		"TOKEN=fake",
		"REPO=tibrez/rhesadox",
		"PR_NUM=99",
		"IS_FJ=true",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("carry step crashed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(carryFile)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("carry report unparsable: %v (%s)", err, raw)
	}
	return report
}

func TestPostReviewCarryMintsDedupedAndResolves(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}, {"id": 5, "title": "backlog"}}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	listing := `[{"id":900,"path":"a.md","in_reply_to":0,"resolved":null,"body":"**Architecture-carry** — the proof router needs an ADR (#512)"},`
	listing += `{"id":901,"path":"b.md","in_reply_to":0,"resolved":null,"body":"unrelated open finding"}]`
	carryEnv := `[[0,{"path":"c.md","line":3,"body":"mint: proof registry split deserves its own issue","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, listing, carryEnv)

	if ids, _ := report["resolved_ids"].([]any); len(ids) != 2 {
		t.Fatalf("carry must resolve the discovered thread 900 and the ledger thread 902, got %v", report["resolved_ids"])
	}
	minted, _ := report["minted"].([]any)
	if len(minted) != 1 {
		t.Fatalf("exactly ONE issue is minted (the ledger todo; the #512-referencing thread references), got %v", minted)
	}
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("the ledger todo is carried, got %v", report["ledger_carried"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minted != 1 {
		t.Fatalf("exactly one mint call, got %d", f.minted)
	}
	if len(f.issues) != 1 {
		t.Fatalf("the open-issue list gained exactly the minted issue, got %d", len(f.issues))
	}
	if ms, ok := f.issues[0]["milestone"]; ok {
		t.Errorf("minted issue must carry NO milestone (#712), got %v", ms)
	}
	if !strings.HasPrefix(f.issues[0]["title"].(string), "architecture: ") {
		t.Errorf("minted issue title must open with the architecture: prefix, got %v", f.issues[0]["title"])
	}
}

func TestPostReviewCarryReferencesExistingIssue(t *testing.T) {
	f := &carryForge{
		milestones: []map[string]any{{"id": 7, "title": "v3.2"}},
		issues: []map[string]any{{
			"title":  "architecture: proof registry split deserves its own issue",
			"number": 388,
		}},
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"proof registry split deserves its own issue","disposition":"architecture-carry"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minted != 0 {
		t.Fatalf("an OPEN issue with the identical title is REFERENCED, never re-minted — minted %d", f.minted)
	}
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("the entry is still carried (tracked by the existing issue), got %v", report["ledger_carried"])
	}
}

func TestPostReviewCarryFailedMintStaysOpen(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}}, forceMintF: true}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"mint: the carry must never lose the thread","disposition":"architecture-carry"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	if lc, _ := report["ledger_carried"].([]any); len(lc) != 0 {
		t.Fatalf("a failed mint leaves the entry UNcarried — the gate counts it open, got %v", report["ledger_carried"])
	}
	notes, _ := report["notes"].([]any)
	if len(notes) == 0 {
		t.Fatal("the failed mint must be SPOKEN in notes")
	}
}

func TestPostReviewCarryResolveFailureSpeaks(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}}, forceResF: true}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"resolve rejected","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	if ids, _ := report["resolved_ids"].([]any); len(ids) != 0 {
		t.Fatalf("a failed resolve must NOT count as resolved (the gate would then approve over an open thread), got %v", report["resolved_ids"])
	}
	// the entry IS ledger-carried in the limited sense that the issue exists —
	// but the unresolved thread keeps the gate honest; the note says so.
	joined := strings.Join(mapToStrings(report["notes"]), " ")
	if !strings.Contains(joined, "resolve FAILED") {
		t.Fatalf("the resolve failure must be spoken, got %v", report["notes"])
	}
}

// #700: the flood's mechanism, pinned. A reworded round ("(6th round,
// narrowed but still open)" + first-line rewording) of a finding that
// already has an issue — even with a mint: marker present — must
// REFERENCE, never re-mint: exact-title dedup could never match this.
func TestPostReviewCarryRewordedRoundReferencesNotMints(t *testing.T) {
	f := &carryForge{
		milestones: []map[string]any{{"id": 7, "title": "v3.2"}},
		issues: []map[string]any{{
			"title":  "architecture: the row is still not a plan fact — pfChunkWidth is not a field of MemoryPlan",
			"number": 2813,
			"state":  "open",
		}},
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"memory_profiler.zig","line":9,"body":"mint: (6th round, narrowed but still open): the width now derives from measured capacity — still not a plan fact","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	if f.minted != 0 {
		t.Fatalf("a reworded round of an existing finding REFERENCES #2813, never re-mints — minted %d", f.minted)
	}
	defer f.mu.Unlock()
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("the entry is still carried (tracked by the referenced issue), got %v", report["ledger_carried"])
	}
	joined := strings.Join(mapToStrings(report["notes"]), " ")
	if !strings.Contains(joined, "#2813") {
		t.Fatalf("the reference must be spoken in notes, got %v", report["notes"])
	}
}

// #700: CLOSED issues are dedup hits too — the state=all listing sees a
// problem minted (and closed) in an earlier round.
func TestPostReviewCarryClosedIssueIsDedupHit(t *testing.T) {
	f := &carryForge{
		milestones: []map[string]any{{"id": 7, "title": "v3.2"}},
		issues: []map[string]any{{
			"title":  "architecture: graph-tier FF_PLAN fence consolidation",
			"number": 2820,
			"state":  "closed",
		}},
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"exec_seq.zig","line":4,"body":"mint: the graph-tier FF_PLAN fence consolidation still has two re-arm points","disposition":"architecture-carry"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minted != 0 {
		t.Fatalf("a closed issue covering the finding is REFERENCED, never re-minted — minted %d", f.minted)
	}
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("carried via the closed issue's reference, got %v", report["ledger_carried"])
	}
}

// #700: the exception gate. An unmatched, unmarked carry resolves as a
// review-record carry — thread resolved, todo consumed, NO new issue.
func TestPostReviewCarryUnmarkedSkipsMintButCarries(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"an ordinary structural nit that outlives the diff","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	if f.minted != 0 {
		t.Fatalf("an unmarked carry must NOT mint (minting is the exception), minted %d", f.minted)
	}
	defer f.mu.Unlock()
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("the todo is still consumed (review-record carry), got %v", report["ledger_carried"])
	}
	if ids, _ := report["resolved_ids"].([]any); len(ids) != 1 {
		t.Fatalf("the thread still resolves (review-record carry), got %v", report["resolved_ids"])
	}
	joined := strings.Join(mapToStrings(report["notes"]), " ")
	if !strings.Contains(joined, "unmarked") {
		t.Fatalf("the skip must be spoken, got %v", report["notes"])
	}
}

// #700: the gate OPENS for severity — a severe finding with no existing
// issue mints exactly one.
func TestPostReviewCarrySevereMarkerMints(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"auth.zig","line":3,"body":"severe: the token scope check bypass lets a forged review post as another user (security)","disposition":"architecture-carry"}]]`
	runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minted != 1 {
		t.Fatalf("a severe unmatched carry mints exactly one issue, got %d", f.minted)
	}
	if !strings.HasPrefix(f.issues[0]["title"].(string), "architecture: ") {
		t.Errorf("minted title keeps the architecture: prefix, got %v", f.issues[0]["title"])
	}
}

// #700: generated artifacts (arch-sync output) never mint — even with a
// mint: marker and no dedup hit. The generator owns that content.
func TestPostReviewCarryGeneratedNeverMints(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "title": "v3.2"}}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"wiki/entities/rhesadox.md","line":1,"body":"mint: the raw/arch/rig.json component graph is missing an edge for the new prefill module","disposition":"architecture-carry"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	f.mu.Lock()
	if f.minted != 0 {
		t.Fatalf("a generated-artifact finding never mints, minted %d", f.minted)
	}
	defer f.mu.Unlock()
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 {
		t.Fatalf("the todo is consumed (review-record carry), got %v", report["ledger_carried"])
	}
	joined := strings.Join(mapToStrings(report["notes"]), " ")
	if !strings.Contains(joined, "generated") {
		t.Fatalf("the generated skip must be spoken, got %v", report["notes"])
	}
}

func mapToStrings(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// TestPostReviewCarryGitHubDialect pins the GitHub half of the dialect
// branch (#685 r1 blocking): the fork verbs (PUT /resolutions,
// in_reply_to replies, milestone id, type=issues&limit dedup) 404/422 on
// GitHub — without the branch every carry ends as an unclosable open
// thread and the mandate's own close-out is unreachable.
func TestPostReviewCarryGitHubDialect(t *testing.T) {
	f := &carryForge{
		milestones: []map[string]any{{"id": 7, "number": 2, "title": "v3.2"}},
		issues: []map[string]any{{
			// a same-title PR mixed into /issues (GitHub has no type filter)
			// must NOT be a dedup hit: it is skipped client-side.
			"title":        "architecture: proof registry split deserves its own issue",
			"number":       388,
			"pull_request": map[string]any{"url": "x"},
		}},
		ghThreadMap: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` +
			`{"id":"RT_902","comments":{"nodes":[{"databaseId":902}]}},` +
			`{"id":"RT_900","comments":{"nodes":[{"databaseId":900}]}}]}}}}}`,
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	listing := `[{"id":900,"path":"a.md","in_reply_to":0,"resolved":null,"body":"**Architecture-carry** — the proof router needs an ADR (#512)"}]`
	carryEnv := `[[1,{"path":"c.md","line":3,"body":"mint: proof registry split deserves its own issue","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, listing, carryEnv, "IS_FJ=false")

	// resolve went through GraphQL resolveReviewThread (thread ids mapped
	// from databaseIds) — NOT the fork's PUT /resolutions (404 on GitHub).
	if ids, _ := report["resolved_ids"].([]any); len(ids) != 2 {
		t.Fatalf("both threads resolve via the GraphQL mutation, got %v", report["resolved_ids"])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resolvedGH) != 2 {
		t.Fatalf("two resolveReviewThread mutations (RT_902, RT_900), got %v", f.resolvedGH)
	}
	if len(f.resolved) != 0 {
		t.Fatalf("the fork /resolutions route must never be hit on GitHub, got %v", f.resolved)
	}
	if len(f.repliesGH) != 2 {
		t.Fatalf("replies land on the documented /comments/{id}/replies route, got %v", f.repliesGH)
	}
	if len(f.replies) != 0 {
		t.Fatalf("no in_reply_to payloads on GitHub (422 not-permitted-key), got %v", f.replies)
	}
	// dedup listing: per_page, never type=issues/limit; the same-title PR
	// is skipped client-side so the tracker IS minted fresh.
	if strings.Contains(f.issuesQuery, "type=issues") || strings.Contains(f.issuesQuery, "limit=") {
		t.Fatalf("GitHub dedup query must use per_page (no type=issues, no limit), got %q", f.issuesQuery)
	}
	if !strings.Contains(f.issuesQuery, "per_page=") {
		t.Fatalf("GitHub dedup query must page with per_page, got %q", f.issuesQuery)
	}
	if f.minted != 1 {
		t.Fatalf("the ledger carry mints despite the same-title PR in /issues (PRs are not dedup hits), got %d", f.minted)
	}
	// milestone: NONE — a minted carry is never scoped into a milestone
	// (#712); "newest open" tracked the running arc, not the finding.
	for i, mp := range f.mintPayloads {
		if ms, ok := mp["milestone"]; ok {
			t.Errorf("mint %d must carry NO milestone (#712), got %v", i, ms)
		}
	}
	// ledger identity: the pair index survives — full-space [1].
	if lc, _ := report["ledger_carried"].([]any); len(lc) != 1 || lc[0] != float64(1) {
		t.Fatalf("ledger_carried must carry the FULL-ledger index [1], got %v", report["ledger_carried"])
	}
}

// TestPostReviewCarryMixedLedgerFullIndices pins the #685 r1 index-space
// contract: ledger_carried is FULL-ledger. With a ledger [fix-in-pr,
// architecture-carry], the carry (run on the pair [1, todo]) must report
// [1] — a filtered-space index ([0]) would make the publisher post the
// CARRIED item as an open TODO and drop the fix-in-pr one.
func TestPostReviewCarryMixedLedgerFullIndices(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{{"id": 7, "number": 2, "title": "v3.2"}}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	// the shell producer enumerates the FULL ledger and filters inside:
	// index 1 is the architecture-carry item, index 0 the fix-in-pr one.
	carryEnv := `[[1,{"path":"c.md","line":9,"body":"split deserves its own issue","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv)

	lc, _ := report["ledger_carried"].([]any)
	if len(lc) != 1 {
		t.Fatalf("exactly the carried entry is reported, got %v", report["ledger_carried"])
	}
	if lc[0] != float64(1) {
		t.Fatalf("ledger_carried[0] = %v, want 1 — the FULL-ledger index (a filtered-space 0 inverts the publish set)", lc[0])
	}
}

// TestPostReviewCarryGitHubResolveLostSpeaks: a GraphQL resolve that the
// host acknowledges but does not perform (data.resolveReviewThread null)
// must not count as resolved — the thread stays open for the gate.
func TestPostReviewCarryGitHubResolveLostSpeaks(t *testing.T) {
	f := &carryForge{
		milestones:    []map[string]any{{"id": 7, "number": 2, "title": "v3.2"}},
		ghThreadMap:   `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"RT_902","comments":{"nodes":[{"databaseId":902}]}}]}}}}}`,
		forceResGH404: true,
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"resolve lost","disposition":"architecture-carry","thread_ref":"902"}]]`
	report := runCarryStep(t, srv, `[]`, carryEnv, "IS_FJ=false")

	if ids, _ := report["resolved_ids"].([]any); len(ids) != 0 {
		t.Fatalf("an unperformed resolve must NOT count as resolved, got %v", report["resolved_ids"])
	}
	joined := strings.Join(mapToStrings(report["notes"]), " ")
	if !strings.Contains(joined, "resolve FAILED") {
		t.Fatalf("the lost resolve must be spoken, got %v", report["notes"])
	}
}

// TestPostReviewCarryMintNoTrackerUnmilestoned pins #712/#717: with open
// milestones but NO "Architecture & Design improvements" among them, the
// mint stays unmilestoned — the old #685-r2 rule ("newest-created open")
// tracked whatever arc was running when the review minted (rhesadox: 56
// carries in the Zig 0.17 milestone) and is never coming back.
func TestPostReviewCarryMintNoTrackerUnmilestoned(t *testing.T) {
	// the NEWEST-created milestone is an arc milestone, not the tracker —
	// it must be ignored.
	f := &carryForge{milestones: []map[string]any{
		{"id": 9, "number": 4, "title": "v0.6.0", "created_at": "2026-07-16T10:00:00Z"},
		{"id": 5, "number": 6, "title": "Zig 0.17 migration", "created_at": "2026-10-07T10:00:00Z"},
	}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"mint: milestone choice","disposition":"architecture-carry"}]]`
	runCarryStep(t, srv, `[]`, carryEnv, "IS_FJ=false")

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mintPayloads) != 1 {
		t.Fatalf("one mint, got %d", len(f.mintPayloads))
	}
	if ms, ok := f.mintPayloads[0]["milestone"]; ok {
		t.Fatalf("without a declared tracker the mint must carry NO milestone, got %v", ms)
	}
}

// TestPostReviewCarryMintIntoTrackerMilestone pins #717: a declared
// "Architecture & Design improvements" milestone receives the mint —
// even when it is NOT the newest-created open milestone (the tracker is
// matched by exact title, never by recency).
func TestPostReviewCarryMintIntoTrackerMilestone(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{
		{"id": 12, "number": 9, "title": "Architecture & Design improvements", "created_at": "2026-07-16T10:00:00Z"},
		{"id": 5, "number": 6, "title": "Zig 0.17 migration", "created_at": "2026-10-07T10:00:00Z"},
	}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"mint: milestone choice","disposition":"architecture-carry"}]]`
	runCarryStep(t, srv, `[]`, carryEnv, "IS_FJ=false")

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mintPayloads) != 1 {
		t.Fatalf("one mint, got %d", len(f.mintPayloads))
	}
	if ms, _ := f.mintPayloads[0]["milestone"].(float64); int(ms) != 9 {
		t.Fatalf("milestone = %v, want the tracker's GitHub NUMBER 9 (not the id 12, not the newer arc milestone)", f.mintPayloads[0]["milestone"])
	}
}

// TestPostReviewCarryMintTrackerForgejoId is the fork dialect: the same
// exact-title match mints the milestone by .id, not .number.
func TestPostReviewCarryMintTrackerForgejoId(t *testing.T) {
	f := &carryForge{milestones: []map[string]any{
		{"id": 51, "title": "Architecture & Design improvements", "created_at": "2026-07-16T10:00:00Z"},
		{"id": 50, "title": "Zig 0.17 migration", "created_at": "2026-10-07T10:00:00Z"},
	}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	carryEnv := `[[0,{"path":"c.md","line":3,"body":"mint: milestone choice","disposition":"architecture-carry"}]]`
	runCarryStep(t, srv, `[]`, carryEnv, "IS_FJ=true")

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mintPayloads) != 1 {
		t.Fatalf("one mint, got %d", len(f.mintPayloads))
	}
	if ms, _ := f.mintPayloads[0]["milestone"].(float64); int(ms) != 51 {
		t.Fatalf("milestone = %v, want the tracker's Forgejo ID 51", f.mintPayloads[0]["milestone"])
	}
}
