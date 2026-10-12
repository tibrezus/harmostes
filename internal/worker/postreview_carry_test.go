package worker

// The thread-closure mandate's CARRY step, tested like the gate classifier:
// the CARRY-CLASSIFIER block is extracted VERBATIM from
// plugins/post-review/post-review.sh (between the markers) and run against
// a fake forge — the shipped code, not a copy.
//
// Contract under test:
//   - a ledger todo with disposition=architecture-carry and thread_ref:
//     mints ONE issue (title-deduped), assigns the newest open milestone,
//     replies the issue reference on the thread, RESOLVES the thread, and
//     reports ledger_carried so the publisher never posts it.
//   - an OPEN issue with the identical title is REFERENCED, never re-minted.
//   - an agent-posted **Architecture-carry** thread discovered in the
//     listing is carried the same way (body #N reference wins over mint).
//   - a failed mint leaves the entry out of ledger_carried (the gate then
//     counts it open — a failed carry downgrades, never loses the thread).
//   - resolve failures are spoken in notes, not fatal.

import (
	"encoding/json"
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
	mu         sync.Mutex
	issues     []map[string]any
	milestones []map[string]any
	minted     int
	resolved   []string
	replies    []map[string]any
	forceMintF bool // force issue-mint failure
	forceResF  bool // force resolution failure
}

func (c *carryForge) mux(t *testing.T) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tibrez/rhesadox/milestones", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		out, _ := json.Marshal(c.milestones)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/issues", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(c.issues)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case http.MethodPost:
			if c.forceMintF {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"title is reserved"}`))
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.minted++
			body["number"] = 400 + c.minted
			c.issues = append(c.issues, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.replies = append(c.replies, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/repos/tibrez/rhesadox/pulls/99/comments/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resolutions") && r.Method == http.MethodPut {
			c.mu.Lock()
			if c.forceResF {
				c.mu.Unlock()
				w.WriteHeader(http.StatusForbidden)
				return
			}
			c.resolved = append(c.resolved, r.URL.Path)
			c.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func runCarryStep(t *testing.T, srv *httptest.Server, listing string, carryEnv string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	carryFile := filepath.Join(dir, "carry.json")
	script := filepath.Join(t.TempDir(), "carry.py")
	if err := os.WriteFile(script, []byte(extractCarryStep(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script)
	cmd.Stdin = strings.NewReader(listing)
	cmd.Env = append(os.Environ(),
		"CARRY_ENV="+carryEnv,
		"CARRY_JSON_FILE="+carryFile,
		"API_BASE="+srv.URL,
		"TOKEN=fake",
		"REPO=tibrez/rhesadox",
		"PR_NUM=99",
	)
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
	carryEnv := `[{"path":"c.md","line":3,"body":"proof registry split deserves its own issue","disposition":"architecture-carry","thread_ref":"902"}]`
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
	if ms, _ := f.issues[0]["milestone"].(float64); int(ms) != 7 {
		t.Errorf("minted issue must carry the NEWEST open milestone (id 7), got %v", f.issues[0]["milestone"])
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

	carryEnv := `[{"path":"c.md","line":3,"body":"proof registry split deserves its own issue","disposition":"architecture-carry"}]`
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

	carryEnv := `[{"path":"c.md","line":3,"body":"the carry must never lose the thread","disposition":"architecture-carry"}]`
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

	carryEnv := `[{"path":"c.md","line":3,"body":"resolve rejected","disposition":"architecture-carry","thread_ref":"902"}]`
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
