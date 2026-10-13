package worker

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

// #429: post-review must publish review.json's comments as NATIVE inline
// threads (GitHub: POST /pulls/N/comments with commit_id+path+line) — the
// agent never reliably self-posts (attempt 90f9fd63ad9a: 81 bash tools,
// zero review-api calls; #426/#427 rendered findings as prose). These tests
// run the REAL plugin script against a fake forge.
//
// r2 contract additions: the publish is NON-FATAL (a thread failure must
// never skip the label-removal consume step or the artifact JSON), the
// dedupe is keyed on the marker the publisher writes (visible on both the
// comments and the reviews listing), the Forgejo branch has a fixture, the
// descoped GitLab dialect gets a structured skip, and the artifact carries
// the posted/rejected/capped counts (#430 r1 P8: the success must be
// measurable from the event, not from plugin stdout).

const (
	fakeRepoPath  = "/repos/tibrezus/harmostes"
	threadMarker8 = "automated review of deadbeef"
)

type fakeForge struct {
	mu               sync.Mutex
	prState          string // "open" | "closed"
	hostDialect      string // "github" | "forgejo" — the POST /reviews validator enforces the host's event enum (#470 r2 t3)
	reviews          []map[string]any
	comments         []map[string]any
	verdictPost      bool
	verdictBody      string
	labelGone        bool
	comment422       map[string]bool  // path → force 422
	authHeaders      []string         // Authorization on native posts (#480 host-gating)
	statusBodies     []map[string]any // the pr-review/verdict merge-status posts (decoded — bodies do not survive the handler)
	issues           []map[string]any
	resolutions      []string // resolution PUT paths (the carry step's close-outs)
	resolvedGH       []string // GraphQL resolveReviewThread thread ids (GitHub dialect carry)
	repliesGH        []string // /comments/{id}/replies paths (GitHub dialect carry)
	graphqlBody      string   // the resolve-state GraphQL response (default: zero threads)
	forceStatus422   bool     // the merge-status POST answers 422 with a quoted JSON body
	forceComments500 bool     // the thread listing answers 500 (the gate-skip path)
	// forceCommentsQuoted hijacks the connection and answers with a QUOTED
	// REASON PHRASE (raw status line — legal HTTP TEXT): urllib propagates
	// it into str(HTTPError), so the quote REACHES thread_gate — the fixture
	// that makes the sanitize test able to fail (#685 r3: a plain 500 never
	// could; default reason phrases carry no quotes).
	forceCommentsQuoted bool
}

func (f *fakeForge) mux(t *testing.T) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		st := f.prState
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}, "state": st})
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if r.Method == http.MethodPost {
			// Record native review submissions — but only events the HOST
			// accepts (#470 r2 t3): a fake that 200s anything pins the
			// client's claim, not the host's contract. Request-event enums:
			//   GitHub  : APPROVE | REQUEST_CHANGES | COMMENT
			//   Forgejo : APPROVED | PENDING | COMMENT | REQUEST_CHANGES
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			ev, _ := body["event"].(string)
			f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
			valid := map[string]map[string]bool{
				"github":  {"APPROVE": true, "REQUEST_CHANGES": true, "COMMENT": true},
				"forgejo": {"APPROVED": true, "PENDING": true, "COMMENT": true, "REQUEST_CHANGES": true},
			}[f.hostDialect]
			if !valid[ev] {
				f.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"event not in the host's request-event enum"}`))
				return
			}
			f.reviews = append(f.reviews, body)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
			return
		}
		out, _ := json.Marshal(f.reviews)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			if f.forceCommentsQuoted {
				f.mu.Unlock()
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = buf.WriteString("HTTP/1.1 404 Not \"Found (quoted)\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
				_ = buf.Flush()
				return
			}
			if f.forceComments500 {
				f.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`server says "no" (quoted)`))
				return
			}
			cs := f.comments
			if cs == nil {
				cs = []map[string]any{}
			}
			out, _ := json.Marshal(cs)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		path, _ := body["path"].(string)
		f.mu.Lock()
		f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
		if f.comment422[path] {
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"line not in diff"}`))
			return
		}
		f.comments = append(f.comments, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.verdictPost = true
		f.verdictBody, _ = b["body"].(string)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.labelGone = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	// Resolve-state GraphQL (the GitHub listing's second half): the body
	// is a FIELD — tests set f.graphqlBody to seed resolve state instead of
	// re-registering the route (ServeMux panics on duplicate patterns).
	if f.graphqlBody == "" {
		f.graphqlBody = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`
	}
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		// The carry step's GitHub resolve (#685 r1): the mutation is answered
		// with a confirmed payload — the same route serves the listing's
		// reviewThreads query (f.graphqlBody).
		if strings.Contains(fmt.Sprint(body["query"]), "resolveReviewThread") {
			vars, _ := body["variables"].(map[string]any)
			tid, _ := vars["t"].(string)
			f.resolvedGH = append(f.resolvedGH, tid)
			_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`))
			return
		}
		body2 := f.graphqlBody
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body2))
	})
	// The thread-closure mandate's surfaces: the merge STATUS post (every
	// verdict round), and the carry step's milestones/issues/resolutions.
	mux.HandleFunc("/repos/tibrezus/harmostes/statuses/deadbeef123", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.statusBodies = append(f.statusBodies, body)
		force := f.forceStatus422
		f.mu.Unlock()
		if force {
			// A GitHub-shaped error body: quoted JSON with embedded quotes —
			// the exact shape that used to unbalance the artifact line (#685 r1).
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Status","code":"custom","message":"description is too long (maximum is 140 characters)"}]}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/milestones", func(w http.ResponseWriter, r *http.Request) {
		// id (fork) and number (GitHub) both present: each dialect reads its
		// own field and the mint payload pins the right one (#685 r1).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":7,"number":2,"title":"v3.2"},{"id":5,"number":1,"title":"backlog"}]`))
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(f.issues)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["number"] = 431
			f.issues = append(f.issues, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99/comments/", func(w http.ResponseWriter, r *http.Request) {
		// .../comments/{id}/replies — GitHub's documented reply route
		// (#685 r1: in_reply_to is a not-permitted key there).
		if strings.HasSuffix(r.URL.Path, "/replies") && r.Method == http.MethodPost {
			f.mu.Lock()
			f.repliesGH = append(f.repliesGH, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		// .../comments/{id}/resolutions — the fork's native resolve route.
		if strings.HasSuffix(r.URL.Path, "/resolutions") && r.Method == http.MethodPut {
			f.mu.Lock()
			f.resolutions = append(f.resolutions, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	// Fork-dialect mirror under the rhesadox key (runPluginEnv with fj=true
	// rewrites the context host+repo): same recording fields, so a single
	// test can assert cross-dialect behavior.
	fk := "/repos/git.rezus.cloud/tibrez/rhesadox"
	mux.HandleFunc(fk+"/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		st := f.prState
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}, "state": st})
	})
	mux.HandleFunc(fk+"/milestones", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":7,"number":2,"title":"v3.2"},{"id":5,"number":1,"title":"backlog"}]`))
	})
	mux.HandleFunc(fk+"/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(f.issues)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["number"] = 431
			f.issues = append(f.issues, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc(fk+"/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(f.reviews)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
			f.reviews = append(f.reviews, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	mux.HandleFunc(fk+"/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			cs := f.comments
			if cs == nil {
				cs = []map[string]any{}
			}
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cs)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.comments = append(f.comments, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc(fk+"/pulls/99/comments/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resolutions") && r.Method == http.MethodPut {
			f.mu.Lock()
			f.resolutions = append(f.resolutions, r.URL.Path)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc(fk+"/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.verdictPost = true
		f.verdictBody, _ = b["body"].(string)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id": 2}`))
	})
	mux.HandleFunc(fk+"/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.labelGone = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(fk+"/statuses/deadbeef123", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.statusBodies = append(f.statusBodies, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	return mux
}

// assertInlineThreads parses the plugin's final artifact JSON line and checks
// the thread counts (spacing-insensitive — r2 P8 asked for the signal, not a
// byte format).
func assertInlineThreads(t *testing.T, out string, posted, rejected, capped int) {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "inline_threads") {
			if err := json.Unmarshal([]byte(line), &last); err != nil {
				t.Fatalf("artifact line unparsable: %v (%s)", err, line)
			}
		}
	}
	if last == nil {
		t.Fatalf("no artifact JSON line found in:\n%s", out)
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	if it == nil {
		t.Fatalf("artifact event carries no inline_threads: %v", ev)
	}
	for k, want := range map[string]int{"posted": posted, "rejected": rejected, "capped": capped} {
		if got, _ := it[k].(float64); int(got) != want {
			t.Errorf("inline_threads.%s = %v, want %d", k, it[k], want)
		}
	}
}

// runPlugin executes the real post-review.sh with HARMOSTES_GITHUB_TOKEN and
// GITHUB_API_BASE pointed at the fake forge, returning combined output.
func runPlugin(t *testing.T, srv *httptest.Server, fj bool, review map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	rc := map[string]any{
		"host": "github.com", "repo": "tibrezus/harmostes", "number": 99,
		"head": map[string]string{"sha": "deadbeef123"},
	}
	rj, _ := json.Marshal(review)
	cj, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Read the script + lib so `go test` cache-tracks them: a plugin change
	// must re-run these tests (r4 P9: a cached pass hid a real mutation).
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join("..", "..", "plugins", "lib", "git-host.sh")); err != nil {
		t.Fatal(err)
	}
	fjFlag := "false"
	if fj {
		fjFlag = "true"
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = hermeticEnv(os.Environ(),
		"HARMOSTES_WORKDIR="+dir,
		"HARMOSTES_GITHUB_TOKEN=fake-token",
		"IS_FJ="+fjFlag,
		"HARMOSTES_TEST_GITHUB_API_BASE="+srv.URL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin run failed (non-fatal publish means the deploy itself must not exit 1 on thread errors): %v\n%s", err, out)
	}
	return string(out)
}

// hermeticEnv scrubs every ambient credential/test-seam variable the plugin
// (or the host lib's token chains) could resolve, then applies the fixtures.
// #480 r3 t6, reproduced live inside a dogfood attempt Job: without the
// scrub an ambient HARMOSTES_GIT_TOKEN outranks the fixture's
// HARMOSTES_GITHUB_TOKEN (host::token chains are first-non-empty-wins) and
// the security pins assert against a REAL credential. Never let os.Environ
// leak into a plugin test env unscrubbed.
var pluginEnvScrub = []string{
	"HARMOSTES_GIT_TOKEN", "HARMOSTES_GITHUB_TOKEN",
	"HARMOSTES_FORGEJO_TOKEN", "HARMOSTES_FORGEJO_BOT_TOKEN",
	"HARMOSTES_FORGEJO_BOT_HOST",
	"HARMOSTES_CODEBERG_TOKEN", "LLM_WIKI_CODEBERG_TOKEN",
	"HARMOSTES_RZC_USERNAME", "HARMOSTES_RZC_PASSWORD",
	"HARMOSTES_TEST_GITHUB_API_BASE", "HARMOSTES_TEST_FORGEJO_API_BASE",
	"HARMOSTES_TEST_CODEBERG_API_BASE",
}

func hermeticEnv(ambient []string, fixtures ...string) []string {
	skip := map[string]bool{}
	for _, k := range pluginEnvScrub {
		skip[k] = true
	}
	out := make([]string, 0, len(ambient)+len(fixtures))
	for _, kv := range ambient {
		if k, _, ok := strings.Cut(kv, "="); ok && skip[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, fixtures...)
}

// runPluginEnv is runPlugin with extra env (needed when a test pins env the
// plugin reads — e.g. HARMOSTES_FORGEJO_BOT_TOKEN, #480) and allows a plugin
// exit (the speak-on-failure paths still exit 0, but a forced-422 self-review
// run must be observable without failing the harness).
func runPluginEnv(t *testing.T, srv *httptest.Server, fj bool, review map[string]any, extraEnv ...string) string {
	t.Helper()
	return runPluginEnvCtx(t, srv, "", fj, review, extraEnv...)
}

// runPluginEnvCtx additionally overrides the pr-context host ("" = default).
// #480 r2 t3 pins the exact-host gate: a non-matching host must fall back to
// the primary token even with the bot credential present.
func runPluginEnvCtx(t *testing.T, srv *httptest.Server, hostOverride string, fj bool, review map[string]any, extraEnv ...string) string {
	t.Helper()
	if hostOverride == "codeberg.org" {
		// codeberg's primary chain is HARMOSTES_CODEBERG_TOKEN (git-host.sh)
		extraEnv = append(extraEnv,
			"HARMOSTES_TEST_CODEBERG_API_BASE="+srv.URL,
			"HARMOSTES_CODEBERG_TOKEN=fake-token",
		)
	}
	dir := t.TempDir()
	rc := map[string]any{
		"host": "github.com", "repo": "tibrezus/harmostes", "number": 99,
		"head": map[string]string{"sha": "deadbeef123"},
	}
	if fj {
		rc["host"] = "git.rezus.cloud"
		if hostOverride != "" {
			rc["host"] = hostOverride
		}
		rc["repo"] = "git.rezus.cloud/tibrez/rhesadox"
	}
	rj, _ := json.Marshal(review)
	cj, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// go-test cache tracking (r4 P9): a plugin/lib change must re-run these.
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join("..", "..", "plugins", "lib", "git-host.sh")); err != nil {
		t.Fatal(err)
	}
	apiBase := "HARMOSTES_TEST_GITHUB_API_BASE=" + srv.URL
	tokName := "HARMOSTES_GITHUB_TOKEN=fake-token"
	fjFlag := "false"
	if fj {
		fjFlag = "true"
		apiBase = "HARMOSTES_TEST_FORGEJO_API_BASE=" + srv.URL
		tokName = "HARMOSTES_FORGEJO_TOKEN=fake-token"
	}
	env := hermeticEnv(os.Environ(),
		"HARMOSTES_WORKDIR="+dir,
		tokName,
		"IS_FJ="+fjFlag,
		apiBase,
	)
	env = append(env, extraEnv...)
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin run failed: %v\n%s", err, out)
	}
	return string(out)
}

func baseReview(comments []any) map[string]any {
	// #680 LEDGER shape: findings[] canonical (comments = legacy alias,
	// normalized by the gate). NO top-level body field — the deploy builds
	// the verdict line from decision + sha.
	return map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"findings":     comments,
	}
}

func TestPostReviewSkipsAgentPostedRemainderFilter(t *testing.T) {
	// #680: findings carrying thread_ref were posted by the agent itself —
	// the deploy publishes ONLY remainders. Both findings below exist; the
	// carried one must not be re-published (no duplicate thread), the
	// remainder must land.
	f := &fakeForge{}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"findings": []any{
			map[string]any{"path": "carried.go", "line": 1, "body": "Blocking: carried",
				"thread_ref": "already-on-the-pr"},
			map[string]any{"path": "remainder.go", "line": 2, "body": "Blocking: remainder"},
		},
	}
	out := runPlugin(t, srv, false, review)
	assertInlineThreads(t, out, 1, 0, 0) // ONLY the remainder publishes
}

func TestPostReviewPublishesNativeInlineThreads(t *testing.T) {
	f := &fakeForge{}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	runPlugin(t, srv, false, baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "finding one"},
		map[string]any{"path": "b.go", "line": 12, "body": "finding two"},
	}))

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.verdictPost {
		t.Error("the verdict comment must still post (the gate's consume signal)")
	}
	if !f.labelGone {
		t.Error("the label must still be removed")
	}
	if len(f.comments) != 2 {
		t.Fatalf("expected 2 native thread posts, got %d", len(f.comments))
	}
	seen := map[string]bool{}
	for _, c := range f.comments {
		path, _ := c["path"].(string)
		body, _ := c["body"].(string)
		line, _ := c["line"].(float64)
		if c["commit_id"] != "deadbeef123" {
			t.Errorf("thread %s must anchor at the reviewed SHA, got %v", path, c["commit_id"])
		}
		if line != 7 && line != 12 {
			t.Errorf("thread %s anchored at unexpected line %v", path, line)
		}
		if !strings.Contains(body, threadMarker8) {
			t.Errorf("thread %s must carry the marker (the dedupe key), got %q", path, body)
		}
		seen[path+"~"+body] = true
	}
	if !strings.Contains(seenKey(seen, "a.go"), "finding one") || !strings.Contains(seenKey(seen, "b.go"), "finding two") {
		t.Fatalf("findings missing from the native threads: %v", seen)
	}
	// The verdict comment is ONE brief line + the trailer (r7, owner
	// directive): no pillar-structured body, no inline-findings prose —
	// the threads are the findings record.
	vb := f.verdictBody // still under the lock taken above (r7: no re-Lock — self-deadlock)
	if !strings.Contains(vb, "<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->") {
		t.Errorf("verdict must carry the trailer, got %q", vb)
	}
	if strings.Contains(vb, "Inline findings") || strings.Count(vb, "\n") > 2 {
		t.Errorf("verdict must be brief (one line + trailer), got %d chars/%d lines: %q",
			len(vb), strings.Count(vb, "\n"), vb)
	}
	if !strings.Contains(vb, "2 blocking findings posted as review threads") {
		t.Errorf("verdict must state the blocking count, got %q", vb)
	}
}

func seenKey(m map[string]bool, path string) string {
	for k := range m {
		if strings.HasPrefix(k, path+"~") {
			return k
		}
	}
	return ""
}

// r3 P5 blocker: the ZERO-FINDINGS path (a clean APPROVE — the common green
// case) must still yield a parsable artifact with an inline_threads object.
// The r2 implementation produced `"inline_threads":}` here — invalid JSON,
// which lastJSONLine zeroes into a lost artifact.
func TestPostReviewZeroFindingsArtifactValid(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	mux := f.mux(t)
	// The APPROVE gate maps resolved threads via GraphQL (reviewThreads) —
	// served by the shared fake's zero-threads default.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unmatched fake-forge request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	review := baseReview(nil)
	review["decision"] = "APPROVE"
	review["body"] = "## Adversarial Review\n<!-- pr-review: APPROVE @ deadbeef123 -->"
	out := runPlugin(t, srv, false, review)

	// GitHub host (#470 r2 t1/t2): the native approval is Forgejo-scoped —
	// GitHub's request-event enum has no APPROVED, so the zero-findings
	// branch must not fire at all (no post attempt, no rejection-shaped
	// metric on the common green case). GitHub's verdict surface stays the
	// comment + threads; see the IS_FJ test for the native-approval path.
	assertInlineThreads(t, out, 0, 0, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reviews) != 0 {
		t.Fatalf("github host must not record a native approval review, got %v", f.reviews)
	}
	if !f.verdictPost || !f.labelGone {
		t.Fatal("zero findings must still deliver the verdict and consume the label")
	}
}

// r3 P9c: both skip shapes carry the unified key set.
func TestPostReviewSkipShapeArtifact(t *testing.T) {
	// GitHub's dedupe keys on COMMENTS carrying the marker (standalone
	// comments attach no review object — r2 correction, kept in r3).
	f := &fakeForge{comments: []map[string]any{
		{"id": 77, "commit_id": "deadbeef123", "body": "_" + threadMarker8 + "_\n\nself-posted"},
	}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	out := runPlugin(t, srv, false, baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "dup"},
	}))
	assertInlineThreads(t, out, 0, 0, 0)
	if !strings.Contains(out, `"skipped":"already-posted"`) {
		t.Errorf("the already-posted skip must be named in the artifact, output:\n%s", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 {
		t.Fatalf("already-posted must not add threads, got %d", len(f.comments))
	}
}

// The deploy owns publishing and is IDEMPOTENT: a re-run at the same head
// (exactly what the r1 P5 exit-before-label-removal bug invited) must not
// duplicate threads — the marker-keyed guard sees the first run's posts.
func TestPostReviewRepublishIsIdempotent(t *testing.T) {
	f := &fakeForge{}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)
	review := baseReview([]any{map[string]any{"path": "a.go", "line": 7, "body": "finding one"}})

	runPlugin(t, srv, false, review)
	runPlugin(t, srv, false, review) // deploy re-run at an unchanged head

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 {
		t.Fatalf("re-publish must be a no-op, got %d threads (wanted 1)", len(f.comments))
	}
}

// r4 P5c: the dedupe pager must paginate correctly (>100 comments). Page 1
// is 100 other people's threads; our marker sits on page 2. The r4 bug
// rebuilt the query with a second "?" and 404'd page 2 — duplicating every
// thread on exactly the 5+-round re-review lineages #429 targets.
func TestPostReviewDedupePaginatesPastHundred(t *testing.T) {
	var mu sync.Mutex
	pagesServed := map[int]bool{}
	others := []map[string]any{}
	for i := 0; i < 100; i++ {
		others = append(others, map[string]any{
			"id": 1000 + i, "commit_id": "aaaaaaaaaaaa", "path": "x.go",
			"line": i + 1, "body": "someone else's thread",
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		// Record the raw query: the r4 bug produced "...per_page=100?per_page=100&page=2".
		if strings.Contains(r.URL.RawQuery, "??") || strings.Count(r.URL.RawQuery, "per_page") > 1 {
			t.Errorf("malformed dedupe query: %s", r.URL.RawQuery)
		}
		if r.Method == http.MethodPost {
			mu.Lock()
			others = append(others, map[string]any{"posted": true})
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if page == "2" {
			_, _ = w.Write([]byte(`[` + strings.Join([]string{`{"id":9999,"commit_id":"deadbeef123","path":"a.go","line":7,"body":"_automated review of deadbeef_ finding one"}`}, ",") + `]`))
			pagesServed[2] = true
			return
		}
		pagesServed[1] = true
		out, _ := json.Marshal(others)
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	runPlugin(t, srv, false, baseReview([]any{map[string]any{"path": "a.go", "line": 7, "body": "finding one"}}))

	// Suppression: the page-1 threads are the only ones — no duplicate post.
	// Pagination proof: page 2 (where the marker lives) was actually served.
	mu.Lock()
	defer mu.Unlock()
	if !pagesServed[2] {
		t.Fatal("page 2 must be fetched — the guard stopped at page 1, so the marker was never seen")
	}
	if len(others) != 100 {
		t.Fatalf("the marker on page 2 must suppress the publish (page-1 threads only), got %d", len(others))
	}
}

// The [:20] cap is load-bearing (r6 P9: it silently downgrades findings —
// the merge currency keys on native threads). 21 findings → 20 threads +
// capped:1; deleting the cap turns this red.
func TestPostReviewCapTruncatesAtTwenty(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	cs := []any{}
	for i := 0; i < 21; i++ {
		cs = append(cs, map[string]any{"path": "f.go", "line": i + 1, "body": fmt.Sprintf("finding %d", i)})
	}
	out := runPlugin(t, srv, false, baseReview(cs))

	assertInlineThreads(t, out, 20, 0, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 20 {
		t.Fatalf("cap must publish exactly 20 threads, got %d", len(f.comments))
	}
	if !strings.Contains(out, "capped: 1 findings") {
		t.Errorf("the dropped findings must be named on the log, output:\n%s", out)
	}
	// side pass-through is asserted on the first posted thread (RIGHT default).
	if f.comments[0]["side"] != "RIGHT" {
		t.Errorf("github threads must carry side=RIGHT by default, got %v", f.comments[0]["side"])
	}
}

// r4 P5a/b: a dedupe SCAN failure must be visible in the artifact
// ("dedupe":"scan-failed") while remaining non-fatal — threads publish,
// the label is consumed.
func TestPostReviewDedupeScanFailureSpeaks(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	// Wrap: 500 the comments GET (the dedupe scan), leaving POSTs working.
	inner := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	})
	t.Cleanup(srv.Close)

	out := runPlugin(t, srv, false, baseReview([]any{map[string]any{"path": "a.go", "line": 7, "body": "finding one"}}))

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 {
		t.Fatalf("fail-open publish must still post, got %d", len(f.comments))
	}
	if !f.labelGone {
		t.Fatal("a scan failure must not skip the consume step")
	}
	// r5 P8 blocker: the flag must actually reach the artifact — the r4
	// implementation consumed it inside a branch, so the fail-open path
	// spoke to stderr only and this assertion stayed green across the defect.
	assertInlineThreads(t, out, 1, 0, 0)
	if !strings.Contains(out, `"dedupe":"scan-failed"`) {
		t.Errorf("the artifact must flag the failed dedupe scan, output:\n%s", out)
	}
}

// r4 P5d: a finding with a missing/garbage line is REJECTED (carried by the
// verdict body), never silently anchored at line 1.
func TestPostReviewLinelessFindingRejectedNotAnchored(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	runPlugin(t, srv, false, baseReview([]any{
		map[string]any{"path": "no-line.go", "body": "no line at all"},
		map[string]any{"path": "good.go", "line": 3, "body": "valid"},
	}))

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c["path"] == "no-line.go" {
			t.Fatalf("a lineless finding must not anchor: %v", c)
		}
	}
	if len(f.comments) != 1 || f.comments[0]["path"] != "good.go" {
		t.Fatalf("the valid sibling must post, got %v", f.comments)
	}
}

// A skill-following agent that self-posted a REVIEW at the SHA must also
// suppress the publish (r2: the guard covers both duplication paths).
func TestPostReviewSkipsWhenAgentSelfPosted(t *testing.T) {
	f := &fakeForge{}
	f.reviews = append(f.reviews, map[string]any{"id": 77, "commit_id": "deadbeef123", "body": "self-posted by the skill"})
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	runPlugin(t, srv, false, baseReview([]any{map[string]any{"path": "a.go", "line": 7, "body": "dup"}}))

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 {
		t.Fatalf("a self-posted round must not be duplicated (only the agent's own thread remains), got %d", len(f.comments))
	}
}

// One rejected finding skips itself, never the batch — and never the
// consume step (r1 P5 blocker: the batch crash skipped label removal and
// the artifact, producing a verdict-duplication loop).
func TestPostReviewOneRejectedLineDoesNotKillTheBatch(t *testing.T) {
	f := &fakeForge{}
	f.comment422 = map[string]bool{"not-in-diff.go": true}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	out := runPlugin(t, srv, false, baseReview([]any{
		map[string]any{"path": "not-in-diff.go", "line": 1, "body": "unanchorable"},
		map[string]any{"path": "good.go", "line": 3, "body": "valid"},
	}))

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 1 || f.comments[0]["path"] != "good.go" {
		t.Fatalf("the valid finding must still post when a sibling line is rejected, got %v", f.comments)
	}
	if !f.labelGone {
		t.Fatal("a thread rejection must not skip the label-removal consume step")
	}
	if !strings.Contains(out, "line not in diff") {
		t.Errorf("the rejection reason (from the response body) must reach the log, output:\n%s", out)
	}
	assertInlineThreads(t, out, 1, 1, 0)
}

// The Forgejo dialect fixture (r1 P9 blocker: the branch shipped untested
// while being the production suspect) — create-pull-review at the SHA with
// new_position anchoring, marker-keyed dedupe against the reviews listing.
func TestPostReviewForgejoDialect(t *testing.T) {
	var mu sync.Mutex
	reviewPosts := []map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			reviewPosts = append(reviewPosts, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5})
			return
		}
		mu.Lock()
		out, _ := json.Marshal([]any{})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	rc := map[string]any{
		"host": "git.rezus.cloud", "repo": "git.rezus.cloud/tibrez/rhesadox", "number": 99,
		"head": map[string]string{"sha": "deadbeef123"},
	}
	rj, _ := json.Marshal(baseReview([]any{map[string]any{"path": "k.go", "line": 4, "body": "forgejo finding"}}))
	cj, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HARMOSTES_WORKDIR="+dir,
		"HARMOSTES_FORGEJO_TOKEN=fake-token",
		"IS_FJ=true",
		"HARMOSTES_TEST_FORGEJO_API_BASE="+srv.URL,
	)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin run failed: %v\n%s", err, outBytes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reviewPosts) != 1 {
		t.Fatalf("expected 1 create-pull-review post, got %d", len(reviewPosts))
	}
	out := string(outBytes)
	p := reviewPosts[0]
	if p["event"] != "REQUEST_CHANGES" || p["commit_id"] != "deadbeef123" {
		// #470: the verdict's native state rides the review — Forgejo's
		// rejection event is REQUEST_CHANGES (REQUEST_REVIEW is not in the
		// CreatePullReview enum; it would degrade to a PENDING draft).
		t.Fatalf("forgejo REQUEST_CHANGES review must be REQUEST_CHANGES at the reviewed SHA, got %v", p)
	}
	cs, _ := p["comments"].([]any)
	if len(cs) != 1 {
		t.Fatalf("expected 1 anchored comment, got %v", p["comments"])
	}
	c, _ := cs[0].(map[string]any)
	if c["path"] != "k.go" || c["body"] != "forgejo finding" {
		t.Fatalf("forgejo comment misplaced: %v", c)
	}
	if _, hasLine := c["new_position"]; !hasLine {
		t.Fatalf("forgejo comments must anchor via new_position (new_line 500s), got %v", c)
	}
	assertInlineThreads(t, out, 1, 0, 0)
}

// #470: an APPROVE with zero findings on the Forgejo host submits the
// native APPROVED review — the whitelisted bot identity satisfying
// required_approvals=1 (rhesadox main), so the merge unblocks without a
// human re-click. The post is dedupe-guarded (one per SHA) and the
// verdict + label consume still ride alongside.
func TestPostReviewForgejoZeroFindingsPostsNativeApproval(t *testing.T) {
	var mu sync.Mutex
	reviewPosts := []map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			reviewPosts = append(reviewPosts, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 6})
			return
		}
		mu.Lock()
		out, _ := json.Marshal([]any{})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 3})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	review := baseReview(nil)
	review["decision"] = "APPROVE"
	review["body"] = "## Adversarial Review\n<!-- pr-review: APPROVE @ deadbeef123 -->"
	out := runForgejo(t, srv, review)

	mu.Lock()
	defer mu.Unlock()
	if len(reviewPosts) != 1 {
		t.Fatalf("expected the native APPROVED review post, got %d posts", len(reviewPosts))
	}
	p := reviewPosts[0]
	if p["event"] != "APPROVED" || p["commit_id"] != "deadbeef123" {
		t.Fatalf("forgejo zero-findings APPROVE must post event APPROVED at the reviewed SHA, got %v", p)
	}
	assertInlineThreads(t, out, 1, 0, 0)
	if !strings.Contains(out, "inline_threads") {
		t.Fatal("artifact must carry the unified inline_threads key set")
	}
}

// TestPostReviewForgejoTodosRideTheNativeApproval (r7-r10 TODO lane): an
// APPROVE with zero blocking findings but a todos[] list posts the native
// APPROVED review carrying the TODOs as anchored comments — the approval
// stands, the threads still owe the dev a follow-up, and the verdict line
// says so instead of hiding them.
// The thread-closure mandate flipped the TODO lane: a todo is a thread of
// the round and counts in the verdict immediately — an APPROVE over an
// open todo is downgraded, the todo publishes as a REQUEST_CHANGES-anchored
// thread with the fix-in-pr class prefix, and the verdict says so. (The old
// test pinned "todos ride the APPROVED review as non-blocking" — the
// live #2745 ending: APPROVE with two newborn TODOs, merged 60s later,
// follow-ups dead. Never again.)
func TestPostReviewTodosDowngradeTheApproval(t *testing.T) {
	var mu sync.Mutex
	var verdictBody string
	reviewPosts := []map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			reviewPosts = append(reviewPosts, body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 6})
			return
		}
		mu.Lock()
		out, _ := json.Marshal([]any{})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		verdictBody, _ = b["body"].(string)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 3})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
		"todos": []any{
			map[string]any{"path": "src/serve/web/assets/styles.css", "line": 12, "body": "dark-mode tokens are missing entirely"},
		},
	}
	_ = runForgejo(t, srv, review)

	mu.Lock()
	defer mu.Unlock()
	if len(reviewPosts) != 1 {
		t.Fatalf("expected one native review post, got %d", len(reviewPosts))
	}
	p := reviewPosts[0]
	if p["event"] != "REQUEST_CHANGES" {
		t.Fatalf("an APPROVE over an open todo must downgrade: the review event is %v, want REQUEST_CHANGES", p["event"])
	}
	cs, _ := p["comments"].([]any)
	if len(cs) != 1 {
		t.Fatalf("the downgraded review must anchor the TODO as a thread, got %v", p["comments"])
	}
	c, _ := cs[0].(map[string]any)
	cb, _ := c["body"].(string)
	if !strings.HasPrefix(cb, "**TODO**") || !strings.Contains(cb, "dark-mode tokens") {
		t.Errorf("TODO thread must carry the class prefix + body, got %q", cb)
	}
	if !strings.Contains(verdictBody, "downgraded: 1 open thread(s)") {
		t.Errorf("verdict must be self-describing about the open todo, got %q", verdictBody)
	}
	if !strings.Contains(verdictBody, "<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->") {
		t.Errorf("verdict must carry the downgraded trailer, got %q", verdictBody)
	}
}

// runForgejo runs the plugin the way the Forgejo dialect test does: the
// fixture's rc names the git.rezus.cloud paths the workspace.sh envelope
// produces, IS_FJ=true, and the Forgejo token env the publish path reads.
func runForgejo(t *testing.T, srv *httptest.Server, review map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	rc := map[string]any{
		"host": "git.rezus.cloud", "repo": "git.rezus.cloud/tibrez/rhesadox", "number": 99,
		"head": map[string]string{"sha": "deadbeef123"},
	}
	rj, _ := json.Marshal(review)
	cj, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(dir, "review.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), cj, 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "post-review", "post-review.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HARMOSTES_WORKDIR="+dir,
		"HARMOSTES_FORGEJO_TOKEN=fake-token",
		"IS_FJ=true",
		"HARMOSTES_TEST_FORGEJO_API_BASE="+srv.URL,
	)
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plugin run failed: %v\n%s", err, outBytes)
	}
	return string(outBytes)
}

// TestPostReviewDowngradeIsR7Conforming is the mutation probe the r7 review
// of 2e16f0c5 demanded: the APPROVE→REQUEST_CHANGES downgrade used to write
// `r["body"]` — a field a conforming review.json no longer has — which
// raised KeyError under `set -euo pipefail` and aborted the whole deploy
// (no verdict comment, no label DELETE: the round was lost). This test
// runs the SHIPPED plugin against an r7-shaped review (NO body) with one
// unresolved prior-round thread and asserts the full downgrade flow.
// Mutation probe: restore the `r["body"] = re.sub(...)` write — this test
// goes red (KeyError → nonzero exit).
func TestPostReviewDowngradeIsR7Conforming(t *testing.T) {
	f := &fakeForge{}
	mux := f.mux(t)
	// reviewThreads for the resolve-state map (C1): one UNresolved thread
	// whose only comment carries databaseId 77 — matching the prior-round
	// comment seeded below (unresolved ⇒ it can downgrade an APPROVE).
	f.graphqlBody = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":false,"comments":{"nodes":[{"databaseId":77}]}}]}}}}}`
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Seed the prior-round open thread the classifier downgrades on:
	// review comment id 77, anchored at a PRIOR commit (not the reviewed
	// SHA), unreplied, and graphqlResolved=false (unresolved).
	f.mu.Lock()
	f.comments = []map[string]any{
		{"id": 77, "path": "a.go", "line": 3, "commit_id": "old123", "in_reply_to": nil, "body": "prior-round finding"},
	}
	f.mu.Unlock()

	// r7 shape: decision APPROVE, NO body — the legacy fixtures carried a
	// body and thereby encoded the very shape that broke the writer.
	out := runPlugin(t, srv, false, map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.verdictPost {
		t.Fatal("the downgrade verdict comment must still post (the gate's consume signal)")
	}
	if !f.labelGone {
		t.Error("the downgrade must still consume the label")
	}
	vb := f.verdictBody
	if !strings.Contains(vb, "<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->") {
		t.Errorf("downgrade verdict must carry the REQUEST_CHANGES trailer (dw_wait_review's merge currency), got %q", vb)
	}
	if !strings.Contains(vb, "downgraded:") || !strings.Contains(vb, "1 open thread(s)") {
		t.Errorf("downgrade verdict must be self-describing (no false findings count), got %q", vb)
	}
	if strings.Contains(vb, "0 blocking findings") {
		t.Errorf("downgrade verdict must not inherit the findings count (nothing was posted this round), got %q", vb)
	}
	if strings.Count(vb, "\n") > 2 {
		t.Errorf("verdict must stay one line + trailer (r7 briefness), got %d newlines: %q", strings.Count(vb, "\n"), vb)
	}
	_ = out
}

// ── #480: native reviews post as the whitelisted bot identity ──────────────
// The worker's primary token belongs to the PR author; both Forgejo and
// GitHub 422 self-reviews ("approve/reject your own pull is not allowed").
// Observed live on rhesadox#2169: twice-APPROVED green, yet the approvals
// gate could never be satisfied and REQUEST_CHANGES threads died to prose.

// forgejoBotFixture builds the Forgejo-shaped mux the #480 tests share:
// it records every POST /reviews Authorization header, can force the
// self-review 422, and serves the minimal comment/label surface post-review
// needs (moved-head check, verdict comment, label removal).
type botFixture struct {
	mu          sync.Mutex
	authHeaders []string
	reviewPosts []map[string]any
}

func (b *botFixture) snapshot() ([]string, []map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.authHeaders...), append([]map[string]any{}, b.reviewPosts...)
}

func forgejoBotFixture(t *testing.T, forceSelfReview422 bool) (*httptest.Server, *botFixture) {
	t.Helper()
	b := &botFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			b.mu.Lock()
			b.authHeaders = append(b.authHeaders, r.Header.Get("Authorization"))
			if forceSelfReview422 {
				b.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"approve your own pull is not allowed"}`))
				return
			}
			b.reviewPosts = append(b.reviewPosts, body)
			b.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 6})
			return
		}
		b.mu.Lock()
		out, _ := json.Marshal([]any{})
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 3})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux), b
}

func TestPostReviewThreadsBatchUsesBotToken(t *testing.T) {
	srv, f := forgejoBotFixture(t, false)

	review := baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "finding one"},
	})
	review["decision"] = "REQUEST_CHANGES"
	out := runPluginEnv(t, srv, true, review, "HARMOSTES_FORGEJO_BOT_TOKEN=bot-token-xyz")

	headers, posts := f.snapshot()
	if len(posts) == 0 {
		t.Fatalf("expected the threads batch POST /reviews, got none (out: %s)", out)
	}
	for i, ah := range headers {
		if ah != "token bot-token-xyz" {
			t.Errorf("native review POST %d must carry the bot credential (#480), got %q", i, ah)
		}
	}
}

func TestPostReviewForgejoNativeApprovalUsesBotToken(t *testing.T) {
	srv, f := forgejoBotFixture(t, false)

	review := baseReview(nil)
	review["decision"] = "APPROVE"
	out := runPluginEnv(t, srv, true, review, "HARMOSTES_FORGEJO_BOT_TOKEN=bot-token-xyz")

	headers, posts := f.snapshot()
	if len(posts) != 1 {
		t.Fatalf("expected exactly the native APPROVED review post, got %d (out: %s)", len(posts), out)
	}
	if ev, _ := posts[0]["event"].(string); ev != "APPROVED" {
		t.Errorf("event must be APPROVED, got %q", ev)
	}
	for i, ah := range headers {
		if ah != "token bot-token-xyz" {
			t.Errorf("native approval POST %d must carry the bot credential (#480), got %q", i, ah)
		}
	}
}

func TestPostReviewForgejoApprovalSelfReviewForbiddenSpeaks(t *testing.T) {
	// No bot token provisioned → primary (author) token → the host 422s the
	// self-review. The artifact must SAY so (self-review-forbidden), not the
	// ambiguous approval-post-failed that hid the #480 root cause for hours.
	srv, _ := forgejoBotFixture(t, true)

	review := baseReview(nil)
	review["decision"] = "APPROVE"
	out := runPluginEnv(t, srv, true, review)

	if !strings.Contains(out, "self-review-forbidden") {
		t.Errorf("artifact must carry skipped:self-review-forbidden on a self-review 422 (#480), out:\n%s", out)
	}
	if strings.Contains(out, `"posted":1`) {
		t.Errorf("nothing may claim a posted approval when the host rejected it, out:\n%s", out)
	}
}

func TestPostReviewGitHubIgnoresForgejoBotToken(t *testing.T) {
	// r1 t1: the bot credential is a FORGEJO identity. It must never be
	// transmitted to GitHub/Codeberg — the host-gate is the fix, this test
	// is the pin (the review posts carry the primary token there).
	f := &fakeForge{}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	// #480 r3 t6: ambient decoys. Without hermeticEnv both outrank the
	// fixtures (host::token chains are first-non-empty-wins; the seam base
	// is a valid loopback URL that routes posts to a dead port) and the pin
	// below fails exactly the way the dogfood Job reproduced it.
	t.Setenv("HARMOSTES_GIT_TOKEN", "ambient-git-decoy")
	t.Setenv("HARMOSTES_TEST_GITHUB_API_BASE", "http://127.0.0.1:9")

	runPluginEnv(t, srv, false, baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "finding one"},
	}), "HARMOSTES_FORGEJO_BOT_TOKEN=bot-token-xyz")

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.authHeaders) == 0 {
		t.Fatal("expected native posts to record Authorization headers")
	}
	for i, ah := range f.authHeaders {
		if ah != "token fake-token" {
			t.Errorf("GitHub native post %d must carry the PRIMARY token, not the Forgejo bot credential, got %q", i, ah)
		}
	}
}

func TestPostReviewForgejoApprovalTransportFailureSurvives(t *testing.T) {
	// r1 t2: under set -euo pipefail the approval POST's curl assignment must
	// not abort the plugin on a TRANSPORT failure (timeout/DNS/reset → exit
	// non-zero even without -f) — label-consume and artifact must still run.
	// The mux serves every other endpoint; POST /reviews hijacks the
	// connection and drops it → curl exit 52 → APPROVAL_CODE=000.
	var mu sync.Mutex
	labelGone := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close() // drop the connection mid-request: a true transport failure
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 3})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		labelGone = true
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	review := baseReview(nil)
	review["decision"] = "APPROVE"
	out := runPluginEnv(t, srv, true, review)

	if !strings.Contains(out, "approval-transport-failed") {
		t.Errorf("a transport failure must speak as approval-transport-failed (r14 t22: distinct from a host-side refusal), out:\n%s", out)
	}
	mu.Lock()
	gone := labelGone
	mu.Unlock()
	if !gone {
		t.Error("the plugin must survive to consume the label after an approval transport failure (set -e would abort before it)")
	}
}

func TestPostReviewBotTokenExactHostGate(t *testing.T) {
	// r2 t3: IS_FJ is "not github.com" — true for codeberg.org and any
	// untrusted pr-context host. The bot credential goes ONLY to the forge
	// it was minted for (HARMOSTES_FORGEJO_BOT_HOST); everything else falls
	// back to the primary token.
	srv, f := forgejoBotFixture(t, false)

	review := baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "finding one"},
	})
	review["decision"] = "REQUEST_CHANGES"
	// host=codeberg.org (≠ HARMOSTES_FORGEJO_BOT_HOST default) + bot present:
	// the native posts must carry the PRIMARY token, never the bot secret.
	out := runPluginEnvCtx(t, srv, "codeberg.org", true, review, "HARMOSTES_FORGEJO_BOT_TOKEN=bot-token-xyz")

	headers, posts := f.snapshot()
	if len(posts) == 0 {
		t.Fatalf("expected the threads batch POST /reviews, got none (out: %s)", out)
	}
	for i, ah := range headers {
		if ah != "token fake-token" {
			t.Errorf("non-matching host must fall back to the primary token, POST %d got %q", i, ah)
		}
	}
}

func TestPostReviewThreadsPublisherSpeaksRejectionReason(t *testing.T) {
	// r5 t12: a bare rejected:N hid the #480 root cause for hours — the
	// artifact must carry the host's rejection reason (last_error), matching
	// the approval leg's speak contract.
	srv, _ := forgejoBotFixture(t, true) // every POST /reviews → 422 self-review shape

	review := baseReview([]any{
		map[string]any{"path": "a.go", "line": 7, "body": "finding one"},
	})
	review["decision"] = "REQUEST_CHANGES"
	out := runPluginEnv(t, srv, true, review)

	if !strings.Contains(out, "last_error") {
		t.Errorf("the threads summary must carry last_error when the host rejects the batch, out:\n%s", out)
	}
}

// ── fork-native resolve: the resolver object is authoritative ───────────────
// Observed live on rhesadox#2126 (2026-09-15): every round downgraded with
// "24 unresolved prior-round thread(s)" because the Forgejo fetch hard-set
// resolved=False ("closure is a closing reply") — but this fork's REST shape
// has NO in_reply_to (rezuscloud/forgejo fork gap), so a closing reply can
// never be expressed and prior threads were permanently open: any PR that
// ever received a finding could never APPROVE again. The fork serializes
// native resolve state as `resolver` (the resolve_doer object; absent/null
// = unresolved) — that is the only closable path and must be authoritative.
// Mutation probe: restore `c["resolved"]=False` — this test goes red (the
// resolved thread reopens and the verdict downgrades citing 2, not 1).
func TestPostReviewForgejoNativeResolverClosesPriorThreads(t *testing.T) {
	var mu sync.Mutex
	verdictBodies := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "deadbeef123"}})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 6})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out, _ := json.Marshal([]any{map[string]any{"id": 55, "user": map[string]any{"login": "harmostes-bot"}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	// Two prior-round threads on review 55: id 81 carries the fork's native
	// resolver object (resolve_doer present ⇒ resolved), id 82 does not.
	// Both anchor at a PRIOR commit so the reviewed-SHA criterion cannot
	// close them — only the resolver state can.
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/pulls/99/reviews/55/comments", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out, _ := json.Marshal([]any{
			map[string]any{"id": 81, "path": "a.go", "line": 1, "commit_id": "old123", "body": "resolved prior finding",
				"resolver": map[string]any{"id": 3, "login": "tibrez"}},
			map[string]any{"id": 82, "path": "b.go", "line": 2, "commit_id": "old123", "body": "open prior finding"},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		verdictBodies = append(verdictBodies, fmt.Sprint(body["body"]))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 3})
	})
	mux.HandleFunc("/repos/git.rezus.cloud/tibrez/rhesadox/issues/99/labels/needs-review", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	out := runForgejo(t, srv, map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(verdictBodies) == 0 {
		t.Fatal("the downgrade verdict comment must post")
	}
	vb := verdictBodies[len(verdictBodies)-1]
	if !strings.Contains(vb, "downgraded:") {
		t.Fatalf("the UNresolved thread must still downgrade the APPROVE, got %q", vb)
	}
	if !strings.Contains(vb, "1 open thread(s)") {
		t.Fatalf("exactly the resolver-less thread counts open (the resolver-carrying one is closed by native state); got %q", vb)
	}
	if strings.Contains(vb, "2 unresolved") {
		t.Fatalf("the resolver-carrying thread must be CLOSED (fork-native resolve is authoritative); got %q", vb)
	}
	_ = out
}

// The merge STATUS (pr-review/verdict): posted on every wired-dialect
// verdict round, head-bound, success ⇔ APPROVE (which now MEANS all
// threads closed or carried). REQUEST_CHANGES posts failure — a project
// adopting the context in branch protection gets a merge block that
// re-checks at merge time, not just at verdict time.
func TestPostReviewStatusPosted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		decision  string
		findings  []any
		wantState string
	}{
		{"clean APPROVE posts success", "APPROVE", []any{}, "success"},
		{"REQUEST_CHANGES posts failure", "REQUEST_CHANGES", []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: x"}}, "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
			srv := httptest.NewServer(f.mux(t))
			t.Cleanup(srv.Close)
			review := map[string]any{
				"decision":     tc.decision,
				"reviewed_sha": "deadbeef123",
				"comments":     tc.findings,
			}
			out := runPlugin(t, srv, false, review)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.statusBodies) != 1 {
				t.Fatalf("exactly one status post per verdict round, got %d (out: %s)", len(f.statusBodies), out)
			}
			body := f.statusBodies[0]
			if state, _ := body["state"].(string); state != tc.wantState {
				t.Errorf("status state = %q, want %q (body %v)", state, tc.wantState, body)
			}
			if ctx, _ := body["context"].(string); ctx != "pr-review/verdict" {
				t.Errorf("status context = %q, want pr-review/verdict", ctx)
			}
		})
	}
}

// TestPostReviewMixedLedgerPublishesOnlyUncarried pins the #685 r1
// index-space contract end-to-end through the SHIPPED plugin (the marker
// test proves the carry emits full indices; this proves the publisher
// consumes them): a ledger [fix-in-pr, architecture-carry] must publish
// ONLY the fix-in-pr todo as an open TODO thread — the carried one is
// terminated by the carry (issue minted, thread resolved via GraphQL).
// A filtered-space index inverts exactly this set (carried published,
// fix-in-pr dropped) — which is why the fixture is MIXED.
func TestPostReviewMixedLedgerPublishesOnlyUncarried(t *testing.T) {
	f := &fakeForge{
		comments: []map[string]any{},
		reviews:  []map[string]any{},
		// the carry's databaseId→threadId map: 902 resolvable via GraphQL
		graphqlBody: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"RT_902","isResolved":false,"comments":{"nodes":[{"databaseId":902}]}}]}}}}}`,
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
		"todos": []any{
			map[string]any{"path": "a.md", "line": 2, "body": "FIXINPR-todo must publish"},
			map[string]any{"path": "b.md", "line": 4, "body": "CARRY-todo must NOT publish",
				"disposition": "architecture-carry", "thread_ref": "902"},
		},
	}
	out := runPlugin(t, srv, false, review) // GitHub dialect

	f.mu.Lock()
	defer f.mu.Unlock()
	// the publish set: exactly one TODO thread — the fix-in-pr item.
	todoPosts := 0
	for _, c := range f.comments {
		body, _ := c["body"].(string)
		if strings.Contains(body, "CARRY-todo must NOT publish") {
			t.Errorf("the CARRIED todo was published as a thread (index-space inversion): %s", body)
		}
		if strings.Contains(body, "FIXINPR-todo must publish") {
			todoPosts++
		}
	}
	if todoPosts != 1 {
		t.Fatalf("exactly the fix-in-pr todo publishes (got %d posts; comments now %d)", todoPosts, len(f.comments))
	}
	// the carry: issue minted under the GitHub milestone NUMBER, thread
	// resolved via the GraphQL mutation (never the fork's /resolutions).
	if len(f.issues) != 1 {
		t.Fatalf("the carried item mints exactly one tracker issue, got %v", f.issues)
	}
	if ms, _ := f.issues[0]["milestone"].(float64); int(ms) != 2 {
		t.Errorf("GitHub mint milestone = %v, want the NUMBER 2 (not the id 7)", f.issues[0]["milestone"])
	}
	if len(f.resolvedGH) != 1 || len(f.resolutions) != 0 {
		t.Errorf("resolve via GraphQL mutation only (resolvedGH=%v resolutions=%v)", f.resolvedGH, f.resolutions)
	}
	// and the artifact line still parses (the whole point of the harness).
	assertInlineThreads(t, out, 1, 0, 0)
}

// TestPostReviewArtifactFinalLineParsesOnDegradedHost pins the #685 r1
// artifact-line contract: when the merge-status POST 422s, the host's
// quoted JSON error body must land SANITIZED in the note — the final
// line stays parseable or lastJSONLine zeroes the ENTIRE round artifact
// exactly when the operator most needs the note.
func TestPostReviewArtifactFinalLineParsesOnDegradedHost(t *testing.T) {
	f := &fakeForge{
		comments:       []map[string]any{},
		reviews:        []map[string]any{},
		forceStatus422: true,
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: degraded-host shape"}},
	}
	out := runPlugin(t, srv, false, review)

	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			if err := json.Unmarshal([]byte(line), &last); err != nil {
				t.Fatalf("artifact line unparsable on the degraded host (the lost-artifact bug): %v\n%s", err, line)
			}
		}
	}
	if last == nil {
		t.Fatalf("no artifact line in output:\n%s", out)
	}
	ev, _ := last["event"].(map[string]any)
	sp, _ := ev["status_post"].(map[string]any)
	if sp == nil {
		t.Fatalf("artifact carries no status_post: %v", ev)
	}
	note, _ := sp["note"].(string)
	if !strings.Contains(note, "status-post-failed-http-422") {
		t.Fatalf("the failure note must survive (spoke), got %q", note)
	}
	if strings.Contains(note, "\"") {
		t.Fatalf("raw quotes must be sanitized out of the note: %q", note)
	}
}

// TestPostReviewArtifactParsesOnGateSkip pins the #685 r2/r3 hardening:
// a listing failure interpolates the failure reason into the artifact's
// thread_gate field at three points — the sanitize keeps the final line
// parseable even when the reason carries quotes. The fixture HIJACKS the
// connection and answers with a quoted REASON PHRASE (str(HTTPError)
// carries it — a plain 500's reason has no quotes and can never bite;
// #685 r3 caught exactly that: the test could not fail).
func TestPostReviewArtifactParsesOnGateSkip(t *testing.T) {
	f := &fakeForge{
		comments:            []map[string]any{},
		reviews:             []map[string]any{},
		forceCommentsQuoted: true,
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
	}
	out := runPlugin(t, srv, false, review)

	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"thread_gate\"") {
			if err := json.Unmarshal([]byte(line), &last); err != nil {
				t.Fatalf("artifact line unparsable on the gate-skip path (raw gate status splice): %v\n%s", err, line)
			}
		}
	}
	if last == nil {
		t.Fatalf("no artifact line in output:\n%s", out)
	}
	ev, _ := last["event"].(map[string]any)
	tg, _ := ev["thread_gate"].(string)
	if !strings.HasPrefix(tg, "skipped:") {
		t.Fatalf("the gate skip must be spoken in thread_gate, got %q", tg)
	}
	if strings.Contains(tg, "\"") {
		t.Fatalf("raw quotes must be sanitized out of thread_gate: %q", tg)
	}
}

// TestPostReviewForkApproveCarriedTodosNotRepublished pins the #685 r5
// fifth-round blocker: the Forgejo zero-findings APPROVED path re-published
// architecture-carry todos as open roots — an APPROVE plus a success merge
// status posted over a newborn thread (the exact #2745 shape). With every
// todo carried (mint + resolve), the native APPROVED review must carry ZERO
// comments.
func TestPostReviewForkApproveCarriedTodosNotRepublished(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "APPROVE",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{},
		"todos": []any{
			map[string]any{"path": "b.md", "line": 4, "body": "carry me to an issue",
				"disposition": "architecture-carry", "thread_ref": "902"},
		},
	}
	out := runPluginEnv(t, srv, true, review) // fork context (host git.rezus.cloud → IS_FJ recomputed true)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resolutions) != 1 {
		t.Fatalf("the carry resolves the thread natively (resolutions=%v)", f.resolutions)
	}
	if len(f.reviews) == 0 {
		t.Fatal("the native APPROVED review is posted")
	}
	rv := f.reviews[len(f.reviews)-1]
	if ev, _ := rv["event"].(string); ev != "APPROVED" {
		t.Fatalf("native review event = %v, want APPROVED", rv["event"])
	}
	if cs, _ := rv["comments"].([]any); len(cs) != 0 {
		t.Fatalf("the CARRIED todo must not ride the APPROVED review as an open root (the #2745 shape), got %v", cs)
	}
	if !strings.Contains(f.verdictBody, "APPROVE at deadbeef123") {
		t.Fatalf("verdict stays APPROVE (nothing open), got %q", f.verdictBody)
	}
	_ = out
}

// TestPostReviewAnchorRejectionCarriedInVerdict pins the #685 r5 delivery
// hole: a blocking finding whose anchor the host 422s (line not in the
// diff) must not evaporate — the verdict posts AFTER the publisher and
// carries the unanchorable finding's text in its body with TRUE counts.
// This PR's own history is the existence proof: four rounds of "N blocking
// findings posted as review threads" while a rejected-anchor finding
// existed nowhere on the platform.
func TestPostReviewAnchorRejectionCarriedInVerdict(t *testing.T) {
	f := &fakeForge{
		comments:   []map[string]any{},
		reviews:    []map[string]any{},
		comment422: map[string]bool{"notin.diff": true},
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{map[string]any{"path": "notin.diff", "line": 99, "body": "Blocking: the off-diff anchor must not evaporate"}},
	}
	runPlugin(t, srv, false, review) // GitHub dialect — per-finding POST, the anchor 422s

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 0 {
		t.Fatalf("the anchor is rejected (nothing lands as a review comment), got %d", len(f.comments))
	}
	if !strings.Contains(f.verdictBody, "1 could not anchor") {
		t.Fatalf("the verdict must count the rejection honestly, got %q", f.verdictBody)
	}
	if !strings.Contains(f.verdictBody, "notin.diff:99") {
		t.Fatalf("the verdict body must carry the unanchorable finding's anchor, got %q", f.verdictBody)
	}
	if !strings.Contains(f.verdictBody, "the off-diff anchor must not evaporate") {
		t.Fatalf("the verdict body must carry the unanchorable finding's TEXT (the fallback landing), got %q", f.verdictBody)
	}
}

// TestPostReviewCapOverflowCarriesFindingsAndTodos pins the #685 r6 hole:
// the r5 carried-list covered FINDINGS only — a TODO dropped by the
// 20-thread cap evaporated exactly like an unanchored finding, while the
// comment claimed both classes were covered; the mixed 'posted' count
// also inflated the blocking number with TODO threads. 20 findings + 2
// TODOs: the findings fill the cap, both TODOs ride the carried list, and
// the verdict speaks per-class counts.
func TestPostReviewCapOverflowCarriesFindingsAndTodos(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	fs := []any{}
	for i := 0; i < 20; i++ {
		fs = append(fs, map[string]any{"path": fmt.Sprintf("f%02d.go", i), "line": 1, "body": fmt.Sprintf("Blocking: finding %d", i)})
	}
	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     fs,
		"todos": []any{
			map[string]any{"path": "t1.md", "line": 2, "body": "capped TODO one"},
			map[string]any{"path": "t2.md", "line": 3, "body": "capped TODO two"},
		},
	}
	out := runPlugin(t, srv, false, review)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.comments) != 20 {
		t.Fatalf("the cap admits exactly 20 threads (findings first), got %d", len(f.comments))
	}
	// both capped TODOs ride the verdict body — neither evaporates
	if !strings.Contains(f.verdictBody, "capped TODO one") || !strings.Contains(f.verdictBody, "capped TODO two") {
		t.Fatalf("capped TODOs must be carried in the verdict body, got %q", f.verdictBody)
	}
	if !strings.Contains(f.verdictBody, "2 TODO(s)") {
		t.Fatalf("the verdict must speak the TODO class separately, got %q", f.verdictBody)
	}
	if !strings.Contains(f.verdictBody, "(20 findings, 0 TODOs)") {
		t.Fatalf("posted counts must be per-class, got %q", f.verdictBody)
	}
	if !strings.Contains(f.verdictBody, "0 findings, 2 TODOs)") {
		t.Fatalf("carried counts must be per-class, got %q", f.verdictBody)
	}
	_ = out
}

// TestPostReviewLinelessTodoCarriesAsTodo pins the #685 r7 tally fix: a
// contract-legal lineless TODO (path+body, no line) that lands in the
// carried list is tallied as a TODO — the malformed/no-usable-line
// append sites defaulted to cls=finding, mis-counting a TODO as a
// blocking finding in the verdict's per-class breakdown.
func TestPostReviewLinelessTodoCarriesAsTodo(t *testing.T) {
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{map[string]any{"path": "a.go", "line": 1, "body": "Blocking: the in-diff finding"}},
		"todos":        []any{map[string]any{"path": "t.md", "body": "lineless TODO rides the carried list as a TODO"}},
	}
	out := runPlugin(t, srv, false, review)

	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.verdictBody, "0 findings, 1 TODOs)") {
		t.Fatalf("the lineless TODO must tally as a TODO in the carried breakdown, got %q", f.verdictBody)
	}
	// the artifact's carried entry carries cls=todo
	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			_ = json.Unmarshal([]byte(line), &last)
		}
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	cs, _ := it["carried"].([]any)
	if len(cs) != 1 {
		t.Fatalf("exactly one carried entry (the lineless TODO), got %v", cs)
	}
	if cls, _ := cs[0].(map[string]any)["cls"].(string); cls != "todo" {
		t.Fatalf("carried entry cls = %v, want todo", cs[0])
	}
}

// TestPostReviewCarriedBodyNotSilentlyTruncated pins the #685 r8 contract:
// a carried finding's text arrives in the verdict body complete or
// complete-with-an-EXPLICIT truncation marker — never a silent [:200] stub
// that drops the fix instruction while the verdict claims the finding
// "stands". Also pins the >10 case: every carried item stays addressable
// on the PR (no "N more in the run artifact" punt below the 30 cap).
func TestPostReviewCarriedBodyNotSilentlyTruncated(t *testing.T) {
	// item 0 SHORT (its FIX line fits the 1600 window → arrives complete);
	// items 1-11 long (FIX beyond the window → explicit truncation marker)
	fs := []any{}
	for i := 0; i < 12; i++ {
		var body string
		if i == 0 {
			body = "Blocking: " + strings.Repeat("short evidence; ", 20) + "FIX: apply fix number 0 exactly as written"
		} else {
			long := strings.Repeat(fmt.Sprintf("evidence %d: the publisher must not silently drop text; ", i), 30)
			body = "Blocking: " + long + fmt.Sprintf("FIX: apply fix number %d exactly as written", i)
		}
		fs = append(fs, map[string]any{"path": fmt.Sprintf("off%02d.diff", i), "line": 5, "body": body})
	}
	f := &fakeForge{
		comments: []map[string]any{},
		reviews:  []map[string]any{},
		comment422: func() map[string]bool {
			m := map[string]bool{}
			for i := 0; i < 12; i++ {
				m[fmt.Sprintf("off%02d.diff", i)] = true
			}
			return m
		}(),
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "deadbeef123", "comments": fs}
	runPlugin(t, srv, false, review)

	f.mu.Lock()
	defer f.mu.Unlock()
	vb := f.verdictBody
	// all 12 addressable on the PR
	for i := 0; i < 12; i++ {
		if !strings.Contains(vb, fmt.Sprintf("off%02d.diff:5", i)) {
			t.Fatalf("carried item %d must be addressable in the verdict body", i)
		}
	}
	if strings.Contains(vb, "more in the run artifact (ledger pathological") {
		t.Fatal("12 carried items is below the 30 cap — every one must be listed, no artifact punt")
	}
	// long bodies: complete or explicitly marked — count markers
	markers := strings.Count(vb, "…[truncated")
	if markers < 11 {
		t.Fatalf("every long carried body must carry an explicit truncation marker, got %d/11", markers)
	}
	// the SHORT body arrives complete, unmarked, FIX line intact
	if !strings.Contains(vb, "FIX: apply fix number 0 exactly as written") {
		t.Fatal("a body within the 1600-char window must arrive COMPLETE in the verdict body")
	}
}

// TestPostReviewCarriedArtifactComplete pins the #685 r9 fidelity: the
// artifact's carried entry keeps the FULL body (the FIX tail — what the
// dev needs — recoverable), while the verdict body truncates WITH a
// marker whose pointer ("full text in the run record (timeline, 7d)") is then TRUE.
func TestPostReviewCarriedArtifactComplete(t *testing.T) {
	long := "Blocking: " + strings.Repeat("evidence that must survive in the artifact; ", 45) +
		"FIX: replace the render-site cap and verify the artifact tail"
	f := &fakeForge{
		comments:   []map[string]any{},
		reviews:    []map[string]any{},
		comment422: map[string]bool{"off.diff": true},
	}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"comments":     []any{map[string]any{"path": "off.diff", "line": 7, "body": long}},
	}
	out := runPlugin(t, srv, false, review)

	// the artifact's carried entry: COMPLETE
	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			_ = json.Unmarshal([]byte(line), &last)
		}
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	cs, _ := it["carried"].([]any)
	if len(cs) != 1 {
		t.Fatalf("one carried entry, got %v", cs)
	}
	if b, _ := cs[0].(map[string]any)["body"].(string); !strings.Contains(b, "FIX: replace the render-site cap and verify the artifact tail") {
		t.Fatalf("the artifact carried entry must be COMPLETE (FIX tail present), got %d chars", len(b))
	}
	// the verdict body: truncated WITH the marker
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.verdictBody, "…[truncated") {
		t.Fatalf("the long body truncates in the verdict body with an explicit marker, got %d chars", len(f.verdictBody))
	}
	if strings.Contains(f.verdictBody, "FIX: replace the render-site cap") {
		t.Fatal("beyond the display cap the verdict shows the marker, not the tail (the artifact holds it)")
	}
}

// TestPostReviewLargeCarriedListSurvives pins the #685 r11 CRITICAL: an
// unbounded carried list rode `export THREADS` into execve and died at
// MAX_ARG_STRLEN (exit 126, no verdict, no artifact, label never
// consumed → re-arm crash loop; reproduced at n=30), and could breach
// lastJSONLine's 1MB scanner cap. The list now travels as a file path,
// the artifact line is python-built, and carried bodies are budgeted
// (64KB aggregate; 1600/window beyond). 40 off-diff findings × 4KB
// bodies: the round must survive with honest counts.
func TestPostReviewLargeCarriedListSurvives(t *testing.T) {
	m := map[string]bool{}
	fs := []any{}
	for i := 0; i < 40; i++ {
		p := fmt.Sprintf("big%02d.diff", i)
		m[p] = true
		fs = append(fs, map[string]any{"path": p, "line": 3,
			"body": "Blocking: " + strings.Repeat("four-kilobyte evidence body that must not kill execve. ", 68) + fmt.Sprintf("FIX #%d", i)})
	}
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}, comment422: m}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	review := map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "deadbeef123", "comments": fs}
	out := runPlugin(t, srv, false, review) // exit 0 — the exit-126 crash is the bug

	f.mu.Lock()
	labelGone, verdict := f.labelGone, f.verdictBody
	f.mu.Unlock()
	if !labelGone {
		t.Fatal("the label must be consumed (an unconsumed label re-arms into the same crash)")
	}
	if verdict == "" {
		t.Fatal("the verdict must post")
	}
	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			if err := json.Unmarshal([]byte(line), &last); err != nil {
				t.Fatalf("artifact line unparsable (the lost-artifact class): %v", err)
			}
		}
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	if bc, _ := it["budget_capped"].(float64); int(bc) == 0 {
		t.Fatalf("the 64KB budget must engage at 40×4KB bodies, got %v", it)
	}
	rej, _ := it["rejected"].(float64)
	capd, _ := it["capped"].(float64)
	if int(rej)+int(capd) != 40 {
		t.Fatalf("counts stay exact under the budget (rejected+capped=40), got rejected=%v capped=%v", rej, capd)
	}
	if len(out) > 300*1024 {
		t.Fatalf("the artifact line must stay far under the 1MB scanner cap, output %d bytes", len(out))
	}
}

// TestPostReviewBudgetInteraction pins the #685 r12 faces: over-budget ≠
// cut. (a) bodies UNDER the 1600 window with an over-budget aggregate
// ride WHOLE — no cut, budget_capped=0, the note must not claim cuts;
// (b) bodies cut by the budget end '[budget-capped]' and the verdict
// renders them FINAL — never re-capped into a false 'full text in the
// run record' pointer.
func TestPostReviewBudgetInteraction(t *testing.T) {
	m := map[string]bool{}
	fs := []any{}
	// 60 findings × ~1.2KB bodies (under the window) → aggregate >64KB
	for i := 0; i < 60; i++ {
		p := fmt.Sprintf("u%02d.diff", i)
		m[p] = true
		fs = append(fs, map[string]any{"path": p, "line": 2,
			"body": "Blocking: " + strings.Repeat("under-window body ", 60) + fmt.Sprintf("FIX %d", i)})
	}
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}, comment422: m}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	out := runPlugin(t, srv, false, map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "deadbeef123", "comments": fs})

	var last map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			_ = json.Unmarshal([]byte(line), &last)
		}
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	// Face B: under-window bodies are never CUT — the counter speaks cuts only
	if bc, _ := it["budget_capped"].(float64); int(bc) != 0 {
		t.Fatalf("under-window bodies must not be counted as cut (budget_capped=%v), got %v", bc, it)
	}
	if bu, _ := it["budget_uncut"].(float64); int(bu) == 0 {
		t.Fatalf("over-aggregate under-window entries are tagged budget-uncut (honest forensics), got %v", it)
	}
	f.mu.Lock()
	vb := f.verdictBody
	f.mu.Unlock()
	if strings.Contains(vb, "bodies CUT at 1600") {
		t.Fatalf("the verdict must not claim cuts that did not happen: %q", vb[:200])
	}

	// Face A: over-window bodies cut by the budget render FINAL — the
	// '[budget-capped]' marker survives, no run-record pointer appended.
	m2 := map[string]bool{"big.diff": true}
	f2 := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}, comment422: m2}
	srv2 := httptest.NewServer(f2.mux(t))
	t.Cleanup(srv2.Close)
	// one huge body + filler to push the aggregate over the budget first
	// 29 fillers × ~2.3KB push the aggregate past the 64KB budget; the
	// big body rides at carried index 29 — the LAST displayed slot — so it
	// is both CUT by the budget and RENDERED (the interaction under test).
	fs2 := []any{}
	for i := 0; i < 29; i++ {
		p := fmt.Sprintf("fill%02d.diff", i)
		m2[p] = true
		fs2 = append(fs2, map[string]any{"path": p, "line": 1, "body": strings.Repeat("filler evidence ", 150)})
	}
	fs2 = append(fs2, map[string]any{"path": "big.diff", "line": 9,
		"body": "Blocking: " + strings.Repeat("must end budget-capped ", 300) + "TAIL"})
	m2["big.diff"] = true
	runPlugin(t, srv2, false, map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "deadbeef123", "comments": fs2})
	f2.mu.Lock()
	vb2 := f2.verdictBody
	f2.mu.Unlock()
	if !strings.Contains(vb2, "[budget-capped]") {
		t.Fatalf("a budget-cut body must render with its final marker, got %q", vb2[len(vb2)-400:])
	}
	// the pointer is legal ONLY for whole-body truncation; over a
	// budget-cut body it lies (the record holds the same cut text)
	for _, ln := range strings.Split(vb2, "\n") {
		if strings.Contains(ln, "big.diff:9") {
			if strings.Contains(ln, "full text in the run record") {
				t.Fatalf("the budget-cut body's line must not carry the run-record pointer (it would lie): %q", ln[:160])
			}
			if !strings.Contains(ln, "[budget-capped]") {
				t.Fatalf("the budget-cut body's line must end with its final marker: %q", ln[:160])
			}
		}
	}
}

// TestPostReviewPathologicalCountBounded pins the #685 r12 TODO: the
// budget bounds per-entry SIZE, not COUNT — 700 under-window bodies
// breached the 1MB scanner (reproduced: 1.07MB line, zeroed parse). The
// artifact's carried list is now hard-bounded (96KB of entries, then
// count-only rows): the artifact line must stay parseable at any count.
func TestPostReviewPathologicalCountBounded(t *testing.T) {
	m := map[string]bool{}
	fs := []any{}
	for i := 0; i < 700; i++ {
		p := fmt.Sprintf("path%03d.diff", i)
		m[p] = true
		fs = append(fs, map[string]any{"path": p, "line": 2,
			"body": strings.Repeat("pathological evidence body ", 55)})
	}
	f := &fakeForge{comments: []map[string]any{}, reviews: []map[string]any{}, comment422: m}
	srv := httptest.NewServer(f.mux(t))
	t.Cleanup(srv.Close)

	out := runPlugin(t, srv, false, map[string]any{"decision": "REQUEST_CHANGES", "reviewed_sha": "deadbeef123", "comments": fs})

	var last map[string]any
	var maxLine int
	for _, line := range strings.Split(out, "\n") {
		if len(line) > maxLine {
			maxLine = len(line)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.Contains(line, "\"inline_threads\"") {
			if err := json.Unmarshal([]byte(line), &last); err != nil {
				t.Fatalf("artifact line unparsable at 700 findings (the lost-artifact class): %v", err)
			}
		}
	}
	if maxLine > 400*1024 {
		t.Fatalf("the artifact line must stay bounded (~96KB entries + framing), got %d bytes", maxLine)
	}
	ev, _ := last["event"].(map[string]any)
	it, _ := ev["inline_threads"].(map[string]any)
	if ab, _ := it["artifact_bound"].(float64); int(ab) == 0 {
		t.Fatalf("count-only rows must engage at 700×1.4KB, got %v", it)
	}
}
