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
	"testing"
)

// The #428 regression suite: the reviewer's context (pr-context.json +
// pr-diff.patch) must be TRUE — derivable, attributable, complete. Each
// test maps to one finding of the filed report:
//
//	finding 1  merge state stated as API facts, not narrated guesses
//	finding 2  files_changed and diff_stats from the SAME tree (no 100-cap)
//	finding 3  pr-diff.patch in FULL — a vendored-first diff cannot lose
//	           the code delta to a head-first byte cap
//	finding 4  the gate's verified contexts exposed as facts (narratives
//	           can only name gates that exist)
//
// The real plugins/workspace/pr_context.py runs against a fixture git repo
// (the git tier) and a fake forge (the network tier), env-scrubbed through
// the same hermeticEnv the post-review plugin tests use. The script files
// are read first so `go test` cache-tracks them (r4 P9: a cached pass hid
// a real mutation).

const vendoredFileCount = 150 // >100 so the OLD files?limit=100 path fails T2

// fixtureRepo builds the reviewed PR shape in git: base branch with a
// README, then a feature branch whose diff is VENDORED-FIRST (150 files)
// with the Go delta LAST — the exact ordering under which the old
// diff[:100000] cap amputated the code changes (#428 finding 3).
func fixtureRepo(t *testing.T) (dir, baseSha, headSha string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "base")
	baseSha = strings.TrimSpace(run("rev-parse", "HEAD"))
	run("checkout", "-qb", "feat")
	// vendored-first bulk
	for i := 0; i < vendoredFileCount; i++ {
		vdir := filepath.Join(dir, "vendor", "pkg", fmt.Sprintf("p%03d", i%7))
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, fmt.Sprintf("f%03d.js", i)),
			[]byte(strings.Repeat(fmt.Sprintf("// vendored line %d padding padding padding\n", i), 16)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "vendor sync")
	// the code delta LAST — first to be amputated by a head-first cap
	if err := os.MkdirAll(filepath.Join(dir, "internal", "piargs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "piargs", "piargs.go"),
		[]byte("package piargs\n\n// the real review surface\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "code delta")
	headSha = strings.TrimSpace(run("rev-parse", "HEAD"))
	return dir, baseSha, headSha
}

// fakeForge serves the network tier: the PR object (merge state is per
// test), commit statuses, and — for the API-fallback test — the files
// list (paginated) and .diff surface.
func fakePRForge(t *testing.T, merged bool, apiDiff string, filePages ...[]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/9.diff", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, apiDiff) // test ResponseWriter write
	})
	mux.HandleFunc("/repos/o/r/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("accept"), "diff") {
			_, _ = fmt.Fprint(w, apiDiff) // test ResponseWriter write
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title": "T", "body": "Refs #5", "html_url": "u",
			"state":     map[bool]string{true: "closed", false: "open"}[merged],
			"merged":    merged,
			"merged_at": map[bool]any{true: "2026-09-16T12:00:00Z", false: nil}[merged],
			"user":      map[string]any{"login": "dev"},
			"head":      map[string]any{"ref": "feat", "sha": "headsha"},
			"base":      map[string]any{"ref": "main", "sha": "basesha"},
		})
	})
	mux.HandleFunc("/repos/o/r/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		_, _ = fmt.Sscanf( // missing page param → 1
			r.URL.Query().Get("page"), "%d", &page)
		if page >= 1 && page <= len(filePages) {
			_ = json.NewEncoder(w).Encode(filePages[page-1])
			return
		}
		_ = json.NewEncoder(w).Encode([]any{})
	})
	mux.HandleFunc("/repos/o/r/commits/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"context": "ci/test", "status": "success"},
		})
	})
	mux.HandleFunc("/repos/o/r/issues/5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title": "linked issue", "body": "b",
			"labels": []map[string]any{{"name": "bug"}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runContextPhase executes the real pr_context.py with the fixture env.
func runContextPhase(t *testing.T, srvURL, workdir, headSha string, extra ...string) (map[string]any, string) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "workspace", "pr_context.py"))
	if err != nil {
		t.Fatal(err)
	}
	// cache-track the plugin + its caller (r4 P9)
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join("..", "..", "plugins", "workspace", "workspace.sh")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", script, "meta")
	cmd.Dir = workdir
	cmd.Env = hermeticEnv(append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HARMOSTES_WORKDIR=" + workdir, "WORKDIR=" + workdir,
		"HOST=git.rezus.cloud", "REPO=o/r", "PR_NUM=9", "HEAD_SHA=" + headSha,
		"API_BASE=" + srvURL,
		"TRIG_BASE=main", "GIT_HOST_TOKEN=fake",
		"HARMOSTES_TEST_FORGEJO_API_BASE=" + srvURL,
		"TRIG_CONTEXTS=" + `{"requiredContexts":["ci/test","ci/lint"],"greenContexts":["ci/test","ci/lint"]}`,
	}, extra...))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("meta phase: %v\n%s", err, out)
	}
	cmd = exec.Command("python3", script, "context")
	cmd.Dir = workdir
	cmd.Env = hermeticEnv(append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HARMOSTES_WORKDIR=" + workdir, "WORKDIR=" + workdir,
		"HOST=git.rezus.cloud", "REPO=o/r", "PR_NUM=9", "HEAD_SHA=" + headSha,
		"API_BASE=" + srvURL,
		"TRIG_BASE=main", "GIT_HOST_TOKEN=fake", "IS_FJ=true",
		"HARMOSTES_TEST_FORGEJO_API_BASE=" + srvURL,
		"TRIG_CONTEXTS=" + `{"requiredContexts":["ci/test","ci/lint"],"greenContexts":["ci/test","ci/lint"]}`,
	}, extra...))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("context phase: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "pr-context.json"))
	if err != nil {
		t.Fatalf("no pr-context.json: %v", err)
	}
	var ctx map[string]any
	if err := json.Unmarshal(raw, &ctx); err != nil {
		t.Fatalf("pr-context.json not JSON: %v", err)
	}
	return ctx, string(out)
}

// gitTierEnv is the api()-base redirect the script needs when the network
// tier must still be reachable (CI/issue/statuses) but the diff comes from
// git. Both phases read API_BASE from the exported HOST mapping, so the
// fake forge serves everything; the DIFF still must come from git.
func cloneFixture(t *testing.T, src, headSha string) string {
	t.Helper()
	workdir := t.TempDir()
	dst := filepath.Join(workdir, "repo")
	// Production shape: file:// transport (a --no-hardlinks path clone copies
	// loose objects file-by-file and proved flaky under the test runner's
	// tempdir churn — ENOENT mid-copy ~50%), single-branch shallow at the
	// feature ref — so origin/<base> does NOT exist and the merge base must
	// come through the base fetch's FETCH_HEAD, exactly like workspace.sh.
	cmd := exec.Command("git", "clone", "-q", "--depth", "50", "--branch", "feat", "file://"+src, dst)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone fixture: %v\n%s", err, out)
	}
	for _, f := range [][]string{
		{"fetch", "--quiet", "--depth", "50", "origin", headSha},
		{"fetch", "--quiet", "--depth", "50", "origin", "main"},
	} {
		g := exec.Command("git", append([]string{"-C", dst}, f...)...)
		g.Env = cmd.Env
		if out, err := g.CombinedOutput(); err != nil {
			t.Fatalf("fixture fetch %v: %v\n%s", f, err, out)
		}
	}
	return workdir
}

// Test428FullDiffNotTruncated — finding 3: the patch carries the ENTIRE
// diff of the dispatched SHA, vendored bulk AND the trailing Go delta; the
// old diff[:100000] cap amputated exactly that tail.
func Test428FullDiffNotTruncated(t *testing.T) {
	src, _, headSha := fixtureRepo(t)
	want := execGit(t, src, "diff", "main..feat")
	if len(want) <= 100000 {
		t.Fatalf("fixture diff must exceed the old 100000 cap, got %d", len(want))
	}
	if !strings.Contains(want, "piargs.go") || !strings.Contains(want, "vendor/pkg") {
		t.Fatal("fixture must be vendored-first with the code delta last")
	}
	srv := fakePRForge(t, false, "STALE-API-DIFF")
	workdir := cloneFixture(t, src, headSha)
	ctx, _ := runContextPhase(t, srv.URL, workdir, headSha)
	raw, _ := os.ReadFile(filepath.Join(workdir, "pr-diff.patch"))
	got := string(raw)
	if got != want {
		t.Fatalf("pr-diff.patch != full git diff (got %d bytes, want %d) — truncated or foreign",
			len(got), len(want))
	}
	if strings.Contains(got, "STALE-API-DIFF") {
		t.Fatal("git tier available but the API diff won — attribution lost (#428 finding 2)")
	}
	if ctx["diff_source"] != "git" {
		t.Fatalf("diff_source = %v, want git", ctx["diff_source"])
	}
}

// Test428FilesAndStatsAgree — finding 2: files_changed and diff_stats
// describe the same tree; the old ?limit=100 files call disagreed with the
// raw-diff stats by whole orders of magnitude.
func Test428FilesAndStatsAgree(t *testing.T) {
	src, _, headSha := fixtureRepo(t)
	srv := fakePRForge(t, false, "")
	workdir := cloneFixture(t, src, headSha)
	ctx, _ := runContextPhase(t, srv.URL, workdir, headSha)
	files := ctx["files_changed"].([]any)
	if len(files) != vendoredFileCount+1 {
		t.Fatalf("files_changed = %d entries, want %d (the 100-cap shape — finding 2)",
			len(files), vendoredFileCount+1)
	}
	stats := ctx["diff_stats"].(map[string]any)
	if int(stats["files"].(float64)) != vendoredFileCount+1 {
		t.Fatalf("diff_stats.files = %v, want %d — stats and files disagree",
			stats["files"], vendoredFileCount+1)
	}
	// the amputation victim must be present as a file entry
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.(map[string]any)["filename"].(string), "piargs.go") {
			found = true
		}
	}
	if !found {
		t.Fatal("files_changed omits the code delta (piargs.go)")
	}
}

// Test428MergeStateStatedAsFact — finding 1: the context carries the API's
// merge state; an open PR can no longer be narrated as merged.
func Test428MergeStateStatedAsFact(t *testing.T) {
	src, _, headSha := fixtureRepo(t)
	for _, merged := range []bool{false, true} {
		srv := fakePRForge(t, merged, "")
		workdir := cloneFixture(t, src, headSha)
		ctx, _ := runContextPhase(t, srv.URL, workdir, headSha)
		if ctx["merged"] != merged {
			t.Fatalf("merged = %v, want %v", ctx["merged"], merged)
		}
		if merged {
			if ctx["pr_state"] != "closed" || ctx["merged_at"] == nil {
				t.Fatalf("merged PR must state closed + merged_at, got %v/%v", ctx["pr_state"], ctx["merged_at"])
			}
			if !strings.Contains(ctx["merge_note"].(string), "MERGED") {
				t.Fatalf("merge_note for a merged PR must say MERGED, got %q", ctx["merge_note"])
			}
		} else {
			if ctx["pr_state"] != "open" {
				t.Fatalf("open PR must state open, got %v", ctx["pr_state"])
			}
			if !strings.Contains(ctx["merge_note"].(string), "open") {
				t.Fatalf("merge_note for an open PR must say open, got %q", ctx["merge_note"])
			}
		}
	}
}

// Test428GateContextsExposed — finding 4: the envelope's required/green
// contexts land in the context as facts, so review narratives can only
// name gates that exist.
func Test428GateContextsExposed(t *testing.T) {
	src, _, headSha := fixtureRepo(t)
	srv := fakePRForge(t, false, "")
	workdir := cloneFixture(t, src, headSha)
	ctx, _ := runContextPhase(t, srv.URL, workdir, headSha)
	gate, ok := ctx["gate"].(map[string]any)
	if !ok {
		t.Fatalf("ctx.gate missing: %v", ctx["gate"])
	}
	if fmt.Sprint(gate["required_contexts"]) != "[ci/test ci/lint]" {
		t.Fatalf("required_contexts = %v", gate["required_contexts"])
	}
	if fmt.Sprint(gate["green_contexts"]) != "[ci/test ci/lint]" {
		t.Fatalf("green_contexts = %v", gate["green_contexts"])
	}
}

// Test428APIFallbackPaginatedAndFull — the fallback path when git
// derivation is impossible: files are paginated to completeness and the
// diff is taken in full (no cap), stamped diff_source=api.
func Test428APIFallbackPaginatedAndFull(t *testing.T) {
	bigPage := make([]map[string]any, 300)
	for i := range bigPage {
		bigPage[i] = map[string]any{"status": "modified",
			"filename": fmt.Sprintf("vendor/p%d.js", i), "additions": 2, "deletions": 1}
	}
	srv := fakePRForge(t, false, strings.Repeat("x\n", 120000/2), // >100000 bytes
		bigPage,
		[]map[string]any{{"status": "added", "filename": "a.go", "additions": 1, "deletions": 0}},
	)
	workdir := t.TempDir()
	ctx, _ := runContextPhase(t, srv.URL, workdir, "somehead")
	if ctx["diff_source"] != "api" {
		t.Fatalf("diff_source = %v, want api (no repo → fallback)", ctx["diff_source"])
	}
	if n := len(ctx["files_changed"].([]any)); n != 301 {
		t.Fatalf("files_changed = %d, want 301 (both pages — the old single-call truncation)", n)
	}
	raw, _ := os.ReadFile(filepath.Join(workdir, "pr-diff.patch"))
	if len(raw) <= 100000 {
		t.Fatalf("api-fallback diff still capped: %d bytes", len(raw))
	}
}

// Test428HeadShaAttribution — the context is pinned to the DISPATCHED sha,
// not the forge's current head (the clone is checked out there first).
func Test428HeadShaAttribution(t *testing.T) {
	src, _, headSha := fixtureRepo(t)
	srv := fakePRForge(t, false, "")
	workdir := cloneFixture(t, src, headSha)
	ctx, _ := runContextPhase(t, srv.URL, workdir, headSha)
	if ctx["head_sha"] != headSha {
		t.Fatalf("head_sha = %v, want the dispatched %s", ctx["head_sha"], headSha)
	}
	if mb, ok := ctx["merge_base"].(string); !ok || len(mb) != 40 {
		t.Fatalf("merge_base missing/malformed: %v", ctx["merge_base"])
	}
}

func execGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// #704 coverage carry: the workspace feeds the per-PR ledger from the
// shared store into the round — coverage-ledger.json (with touched flags
// computed against the review delta), review-delta.patch (the fix diff:
// last-reviewed-sha..head), and a `coverage` fact block in
// pr-context.json. The tests run the shipped coverage_carry.py against a
// real git fixture, plus a wiring pin on workspace.sh (the #706 r12
// lesson: a test that assembles its own wiring proves only what it
// injected — the INVOCATION in the shipped script is pinned verbatim).
func TestWorkspaceCoverageCarry(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 missing in CI — the coverage carry is the ledger's reader and must be tested")
		}
		t.Skip("python3 not available locally")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}

	// fixture repo: LAST commits a.go; HEAD modifies a.go and adds c.go —
	// b.go (the frontier) is untouched by the delta.
	last := func(t *testing.T, dir string) (lastSha, headSha string) {
		t.Helper()
		run := func(args ...string) string {
			cmd := exec.Command(gitBin, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v — %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		run("init", "-q", "-b", "main")
		run("config", "user.email", "t@t")
		run("config", "user.name", "t")
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte("frontier\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "-A")
		run("commit", "-qm", "last")
		lastSha = run("rev-parse", "HEAD") // -q commit prints nothing; the sha comes from rev-parse
		if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("two\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "c.go"), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "-A")
		run("commit", "-qm", "head")
		headSha = run("rev-parse", "HEAD")
		return lastSha, headSha
	}

	runCarry := func(t *testing.T, workdir, store, prNum, headSha string) {
		t.Helper()
		script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "workspace", "coverage_carry.py"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, script)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(),
			"HARMOSTES_WORKDIR="+workdir,
			"HARMOSTES_SOL_PI_STORE="+store,
			"HARMOSTES_TRIGGER_PR="+prNum,
			"HARMOSTES_TRIGGER_SHA="+headSha,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("coverage_carry crashed: %v — %s", err, out)
		}
	}

	t.Run("ledger carried: touched flags, delta patch, context stamp", func(t *testing.T) {
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		lastSha, headSha := last(t, repo)
		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(map[string]any{
			"version": 1, "pr": 1, "rounds": 1, "last_reviewed_sha": lastSha,
			"covered":       []any{map[string]any{"path": "a.go", "sha": lastSha, "round": 1}},
			"frontier":      []any{map[string]any{"path": "b.go", "reason": "never reached"}},
			"findings_open": 1,
		})
		if err := os.WriteFile(filepath.Join(store, "coverage", "1.json"), lb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		runCarry(t, dir, store, "1", headSha)

		// the agent-readable ledger: touched computed against the delta
		b, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
		if err != nil {
			t.Fatalf("coverage-ledger.json not carried: %v", err)
		}
		var led map[string]any
		if err := json.Unmarshal(b, &led); err != nil {
			t.Fatal(err)
		}
		covered, _ := led["covered"].([]any)
		if len(covered) != 1 {
			t.Fatalf("one covered entry expected, got %v", covered)
		}
		entry := covered[0].(map[string]any)
		if entry["touched"] != true {
			t.Errorf("a.go changed since last review — touched must be true, got %v", entry)
		}
		if led["delta_emitted"] != true {
			t.Errorf("delta must be emitted, got %v", led["delta_emitted"])
		}

		// the delta patch: the fix diff LAST..HEAD
		delta, err := os.ReadFile(filepath.Join(dir, "review-delta.patch"))
		if err != nil {
			t.Fatalf("review-delta.patch not emitted: %v", err)
		}
		if !strings.Contains(string(delta), "diff --git a/a.go") || !strings.Contains(string(delta), "b/c.go") {
			t.Errorf("delta must be last..head (a.go modified, c.go added), got:\n%s", delta[:min(len(delta), 300)])
		}

		// the context fact block
		cb, err := os.ReadFile(filepath.Join(dir, "pr-context.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ctx map[string]any
		if err := json.Unmarshal(cb, &ctx); err != nil {
			t.Fatal(err)
		}
		cov, ok := ctx["coverage"].(map[string]any)
		if !ok {
			t.Fatalf("pr-context.json must carry the coverage fact block, got %v", ctx)
		}
		if cov["delta_patch"] != "review-delta.patch" || cov["covered_total"] != float64(1) ||
			cov["frontier_count"] != float64(1) || cov["rounds"] != float64(1) || cov["same_head"] != false {
			t.Errorf("coverage fact block wrong: %v", cov)
		}
	})

	t.Run("no store: clean no-op (full-surface round)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		runCarry(t, dir, "", "1", "deadbeef")
		if _, err := os.Stat(filepath.Join(dir, "coverage-ledger.json")); !os.IsNotExist(err) {
			t.Error("no ledger must be written without a store")
		}
		if _, err := os.Stat(filepath.Join(dir, "review-delta.patch")); !os.IsNotExist(err) {
			t.Error("no delta must be emitted without a store")
		}
	})

	t.Run("same head: no delta, same_head stamped", func(t *testing.T) {
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		lastSha, headSha := last(t, repo)
		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(map[string]any{
			"version": 1, "pr": 2, "rounds": 1, "last_reviewed_sha": headSha,
			"covered":  []any{map[string]any{"path": "a.go", "sha": headSha, "round": 1}},
			"frontier": []any{map[string]any{"path": "b.go"}},
		})
		if err := os.WriteFile(filepath.Join(store, "coverage", "2.json"), lb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = lastSha
		runCarry(t, dir, store, "2", headSha)
		b, err := os.ReadFile(filepath.Join(dir, "pr-context.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ctx map[string]any
		_ = json.Unmarshal(b, &ctx)
		cov := ctx["coverage"].(map[string]any)
		if cov["same_head"] != true || cov["delta_patch"] != nil {
			t.Errorf("same-head round: no delta, same_head true — got %v", cov)
		}
		if _, err := os.Stat(filepath.Join(dir, "review-delta.patch")); !os.IsNotExist(err) {
			t.Error("a same-head round must not emit a delta patch")
		}
	})

	t.Run("last-reviewed sha unreachable: no delta, ledger still carried", func(t *testing.T) {
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		_, headSha := last(t, repo)
		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(map[string]any{
			"version": 1, "pr": 3, "rounds": 1, "last_reviewed_sha": "0000000000000000000000000000000000000000",
			"covered":  []any{map[string]any{"path": "a.go", "sha": "0000000000000000000000000000000000000000", "round": 1}},
			"frontier": []any{},
		})
		if err := os.WriteFile(filepath.Join(store, "coverage", "3.json"), lb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		runCarry(t, dir, store, "3", headSha) // must exit 0 despite the unreachable sha
		if _, err := os.Stat(filepath.Join(dir, "review-delta.patch")); !os.IsNotExist(err) {
			t.Error("an unreachable last-reviewed sha must not fabricate a delta")
		}
		b, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
		if err != nil {
			t.Fatalf("the ledger must still carry (the frontier guides the round): %v", err)
		}
		if !strings.Contains(string(b), "delta_emitted\": false") && !strings.Contains(string(b), "\"delta_emitted\":false") {
			t.Errorf("delta_emitted must be false, got %s", b)
		}
		// #704 r1: the entry's own sha is ALSO unreachable — touched must
		// degrade to TRUE (cannot prove unchanged → re-verify), never the
		// all-false trust default.
		var led struct {
			Covered []struct {
				Path           string `json:"path"`
				Touched        bool   `json:"touched"`
				TouchedUnknown bool   `json:"touched_unknown"`
			} `json:"covered"`
		}
		if err := json.Unmarshal(b, &led); err != nil {
			t.Fatal(err)
		}
		if len(led.Covered) != 1 || !led.Covered[0].Touched || !led.Covered[0].TouchedUnknown {
			t.Errorf("an unknowable coverage sha must pin touched=true (conservative), got %+v", led.Covered)
		}
	})

	t.Run("global sha unreachable but the entry's own sha reachable: per-sha verdict is real", func(t *testing.T) {
		// rebase/force-push killed last_reviewed_sha, but the entry's
		// coverage sha survives: the per-sha computation still answers
		// (changed → true, unknown flag ABSENT), while a global-only
		// computation would degrade every entry to conservative-true.
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) string {
			cmd := exec.Command(gitBin, args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v — %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		run("init", "-q", "-b", "main")
		run("config", "user.email", "t@t")
		run("config", "user.name", "t")
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("a.go", "one\n")
		write("b.go", "one\n")
		run("add", "-A")
		run("commit", "-qm", "covered-at")
		coveredAt := run("rev-parse", "HEAD")
		// a real (reachable) commit changes a.go…
		write("a.go", "two\n")
		run("add", "-A")
		run("commit", "-qm", "real-change")
		// …and an orphaned one would have changed b.go — reset away, so
		// last_reviewed_sha (taken here, then faked unreachable in the
		// ledger) has no reachable counterpart.
		write("b.go", "two\n")
		run("add", "-A")
		run("commit", "-qm", "forced-away")
		run("reset", "--hard", "HEAD~1")
		head := run("rev-parse", "HEAD")

		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(map[string]any{
			"version": 1, "pr": 5, "rounds": 1, "last_reviewed_sha": "0000000000000000000000000000000000000000",
			"covered": []any{
				map[string]any{"path": "a.go", "sha": coveredAt, "round": 1},
				map[string]any{"path": "b.go", "sha": coveredAt, "round": 1},
			},
			"frontier": []any{},
		})
		if err := os.WriteFile(filepath.Join(store, "coverage", "5.json"), lb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		runCarry(t, dir, store, "5", head)
		b, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
		if err != nil {
			t.Fatal(err)
		}
		var led struct {
			Covered []struct {
				Path           string `json:"path"`
				Touched        bool   `json:"touched"`
				TouchedUnknown bool   `json:"touched_unknown"`
			} `json:"covered"`
		}
		if err := json.Unmarshal(b, &led); err != nil {
			t.Fatal(err)
		}
		byPath := map[string]struct {
			touched, unknown bool
		}{}
		for _, e := range led.Covered {
			byPath[e.Path] = struct {
				touched, unknown bool
			}{e.Touched, e.TouchedUnknown}
		}
		if got := byPath["a.go"]; !got.touched || got.unknown {
			t.Errorf("a.go changed since its coverage sha — real per-sha verdict required (touched=true, not the conservative unknown), got %+v", got)
		}
		if got := byPath["b.go"]; got.touched || got.unknown {
			t.Errorf("b.go unchanged since its coverage sha — the per-sha answer must be false (global-only would say conservative-true), got %+v", got)
		}
	})

	t.Run("per-entry touched: an entry at an OLDER sha is judged against its own sha", func(t *testing.T) {
		// two rounds ago S1 covered a.go; last round S2 covered b.go; head
		// S3 changed b.go only. Global delta S2..S3 misses a.go is fine
		// (untouched — its own sha agrees); b.go must read touched=true
		// against S2 even though a last_reviewed_sha-only computation at an
		// unreachable S2 would have said false.
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) string {
			cmd := exec.Command(gitBin, args...)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v — %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		run("init", "-q", "-b", "main")
		run("config", "user.email", "t@t")
		run("config", "user.name", "t")
		write := func(name, content string) {
			if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("a.go", "one\n")
		write("b.go", "one\n")
		run("add", "-A")
		run("commit", "-qm", "s1")
		s1 := run("rev-parse", "HEAD")
		write("b.go", "two\n")
		run("add", "-A")
		run("commit", "-qm", "s2")
		s2 := run("rev-parse", "HEAD")
		write("b.go", "three\n")
		run("add", "-A")
		run("commit", "-qm", "s3")
		s3 := run("rev-parse", "HEAD")

		store := t.TempDir()
		if err := os.MkdirAll(filepath.Join(store, "coverage"), 0o755); err != nil {
			t.Fatal(err)
		}
		lb, _ := json.Marshal(map[string]any{
			"version": 1, "pr": 4, "rounds": 2, "last_reviewed_sha": s2,
			"covered": []any{
				map[string]any{"path": "a.go", "sha": s1, "round": 1},
				map[string]any{"path": "b.go", "sha": s2, "round": 2},
			},
			"frontier": []any{},
		})
		if err := os.WriteFile(filepath.Join(store, "coverage", "4.json"), lb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		runCarry(t, dir, store, "4", s3)
		b, err := os.ReadFile(filepath.Join(dir, "coverage-ledger.json"))
		if err != nil {
			t.Fatal(err)
		}
		var led struct {
			Covered []struct {
				Path    string `json:"path"`
				Touched bool   `json:"touched"`
			} `json:"covered"`
		}
		if err := json.Unmarshal(b, &led); err != nil {
			t.Fatal(err)
		}
		byPath := map[string]bool{}
		for _, e := range led.Covered {
			byPath[e.Path] = e.Touched
		}
		if byPath["a.go"] {
			t.Error("a.go unchanged since its OWN coverage sha (s1) — touched must be false")
		}
		if !byPath["b.go"] {
			t.Error("b.go changed since its coverage sha (s2) — touched must be true (a last_reviewed_sha-only computation would miss it when s2 is unreachable)")
		}
	})

	t.Run("wiring pin: workspace.sh invokes the carry and clears stale artifacts", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "workspace", "workspace.sh"))
		if err != nil {
			t.Fatal(err)
		}
		script := string(raw)
		inv := `python3 "$(dirname "$0")/coverage_carry.py"`
		if !strings.Contains(script, inv) {
			t.Fatalf("workspace.sh lost the coverage-carry invocation (%q) — the ledger never reaches the round", inv)
		}
		for _, stale := range []string{"review-delta.patch", "coverage-ledger.json"} {
			if !strings.Contains(script, `"$WORKDIR/`+stale+`"`) {
				t.Fatalf("workspace.sh must clear stale %s in the provisioning rm (a foreign round's carry would mislead the agent)", stale)
			}
		}
	})
}

// pluginEnvScrub + hermeticEnv: restored from the post-review plugin tests
// (deleted with the script, #749 Phase 3) — workspace.sh's test runs must
// stay hermetic against ambient credential env.
var pluginEnvScrub = []string{
	"HARMOSTES_REVIEW_POST_TOKEN", "HARMOSTES_REVIEW_POST_TOKEN_FJ",
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

// TestWorkspaceResearchDigest — the compound is SERVED, not searched for
// (#2845 audit: five rounds published four research notes and read the
// shared store zero times; every fix-push generation re-derived its
// orientation from scratch). research_digest.py folds THIS PR's
// prior-round notes into research-prior.md (newest first) and stamps the
// fact into pr-context.json. Another PR's notes and prefix-less
// repo-wide notes stay out. Cold rounds (no store, no notes) stay
// exactly as before: no file, no failure.
func TestWorkspaceResearchDigest(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("python3 missing in CI — the research digest is the compound's reader and must be tested")
		}
		t.Skip("python3 not available locally")
	}

	runDigest := func(t *testing.T, workdir, store, pr string) {
		t.Helper()
		script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "workspace", "research_digest.py"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, script)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(),
			"HARMOSTES_WORKDIR="+workdir,
			"HARMOSTES_SOL_PI_STORE="+store,
			"HARMOSTES_TRIGGER_PR="+pr,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("research_digest crashed: %v — %s", err, out)
		}
	}

	t.Run("this PR's notes served newest-first; other PRs and repo-wide notes excluded", func(t *testing.T) {
		dir := t.TempDir()
		store := t.TempDir()
		src := filepath.Join(store, "_shared", "research")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		older := strings.Join([]string{
			"---",
			"repo: tibrez/rhesadox",
			"workflow: pr-review",
			"pr: \"2845\"",
			"head_sha: dbebaba",
			"written_at: 2026-10-24T17:50:00Z",
			"---",
			"",
			"# Round 1 orientation",
			"",
			"The CUDA decode path lives in src/backend/decode_cuda.zig.",
			"",
		}, "\n")
		newer := strings.Join([]string{
			"---",
			"repo: tibrez/rhesadox",
			"workflow: pr-review",
			"pr: \"2845\"",
			"head_sha: 6dfb0f2",
			"written_at: 2026-10-24T22:30:00Z",
			"---",
			"",
			"# Round 2 orientation",
			"",
			"Builds on round 1: the gate asserts the resolved-selection record.",
			"",
		}, "\n")
		if err := os.WriteFile(filepath.Join(src, "2845-dbebaba.md"), []byte(older), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "2845-6dfb0f2.md"), []byte(newer), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "999-other-pr.md"), []byte("# another PR's context\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "repo-wide.md"), []byte("# repo-wide note, no PR prefix\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pr-context.json"), []byte(`{"number": 2845}`), 0o644); err != nil {
			t.Fatal(err)
		}
		runDigest(t, dir, store, "2845")

		b, err := os.ReadFile(filepath.Join(dir, "research-prior.md"))
		if err != nil {
			t.Fatalf("research-prior.md not served: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, "2845-6dfb0f2.md") || !strings.Contains(s, "2845-dbebaba.md") {
			t.Fatalf("both PR notes must be served, got:\n%s", s)
		}
		if strings.Contains(s, "999-other-pr.md") || strings.Contains(s, "repo-wide.md") {
			t.Fatalf("other PRs' and prefix-less notes must stay out, got:\n%s", s)
		}
		// newest first: round 2's header before round 1's
		if strings.Index(s, "2845-6dfb0f2.md") > strings.Index(s, "2845-dbebaba.md") {
			t.Fatalf("notes must be newest-first, got:\n%s", s)
		}
		// the round learns about the compound from its ONE environment file
		cb, err := os.ReadFile(filepath.Join(dir, "pr-context.json"))
		if err != nil {
			t.Fatal(err)
		}
		var ctx map[string]any
		if err := json.Unmarshal(cb, &ctx); err != nil {
			t.Fatal(err)
		}
		rp, _ := ctx["research_prior"].(map[string]any)
		if rp == nil || rp["notes"] != float64(2) {
			t.Fatalf("pr-context research_prior fact block missing/incorrect: %v", ctx["research_prior"])
		}
	})

	t.Run("cold round: no notes → no file, no failure", func(t *testing.T) {
		dir := t.TempDir()
		store := t.TempDir()
		runDigest(t, dir, store, "2845")
		if _, err := os.Stat(filepath.Join(dir, "research-prior.md")); !os.IsNotExist(err) {
			t.Fatalf("a cold round must not fabricate a digest")
		}
	})

	t.Run("no store configured → silent no-op", func(t *testing.T) {
		dir := t.TempDir()
		script, err := filepath.Abs(filepath.Join("..", "..", "plugins", "workspace", "research_digest.py"))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(py, script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HARMOSTES_WORKDIR="+dir) // no store, no PR
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("digest must be a silent no-op without the store: %v — %s", err, out)
		}
	})
}
