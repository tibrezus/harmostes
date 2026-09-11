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
	mu          sync.Mutex
	prState     string // "open" | "closed"
	reviews     []map[string]any
	comments    []map[string]any
	verdictPost bool
	verdictBody string
	labelGone   bool
	comment422  map[string]bool // path → force 422
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
		out, _ := json.Marshal(f.reviews)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/repos/tibrezus/harmostes/pulls/99/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			out, _ := json.Marshal(f.comments)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		path, _ := body["path"].(string)
		f.mu.Lock()
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
	cmd.Env = append(os.Environ(),
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

func baseReview(comments []any) map[string]any {
	return map[string]any{
		"decision":     "REQUEST_CHANGES",
		"reviewed_sha": "deadbeef123",
		"body":         "## Adversarial Review\n<!-- pr-review: REQUEST_CHANGES @ deadbeef123 -->",
		"comments":     comments,
	}
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
	// The APPROVE gate maps resolved threads via GraphQL (reviewThreads).
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`))
	})
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

	assertInlineThreads(t, out, 0, 0, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
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
	if p["event"] != "COMMENT" || p["commit_id"] != "deadbeef123" {
		t.Fatalf("forgejo review must be COMMENT at the reviewed SHA, got %v", p)
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
