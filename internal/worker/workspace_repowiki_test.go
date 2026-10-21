package worker

// #726: the repository wiki's clone is a golden block in workspace.sh
// (REPO-WIKI markers) — this runs the SHIPPED block verbatim with the
// SPEC envelope the worker wires, against a local bare repo standing
// in for the wiki, in both URL shapes the declarative field accepts.
// Review r1 hardened it: port-bearing URLs, credential placement
// (sentinel-populated env — a scrubbed table cannot see the one
// behavior the normalizer exists for), mis-declaration signal, and
// the unconditional clear of a previous round's docs.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func extractRepoWikiBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "workspace", "workspace.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "# REPO-WIKI-START")
	end := strings.Index(script, "# REPO-WIKI-END")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("REPO-WIKI markers not found in plugins/workspace/workspace.sh — keep the repo-wiki block between the markers")
	}
	return script[start:end]
}

// runRepoWikiBlock runs the shipped block with a minimal prelude mirroring
// workspace.sh's own bindings (SPEC → the workflow instance spec JSON).
// A pr-context.json fixture rides in the workdir so the fact stamp lands.
func runRepoWikiBlock(t *testing.T, workDir, specJSON string, extraEnv ...string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	gitHostAbs, err := filepath.Abs(filepath.Join("..", "..", "plugins", "lib", "git-host.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "pr-context.json"), []byte(`{"number":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	block := "set -euo pipefail\n" +
		"log() { echo \"[workspace] $*\"; }\n" +
		". " + gitHostAbs + "\n" +
		"WORKDIR=\"${HARMOSTES_WORKDIR:-/workspace}\"\n" +
		"SPEC=\"$HARMOSTES_SPEC\"\n" +
		extractRepoWikiBlock(t) + "\n"
	cmd := exec.Command(bash, "-c", block)
	cmd.Env = append(append(os.Environ(),
		"HARMOSTES_WORKDIR="+workDir,
		"HARMOSTES_SPEC="+specJSON,
		"PATH="+filepath.Dir(gitBin)+":"+os.Getenv("PATH"),
	), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("repo-wiki block failed: %v — %s", err, out)
	}
	return string(out)
}

func TestWorkspaceRepoWiki(t *testing.T) {
	// a local wiki: bare repo with the standard docs, clonable via its
	// file:// URL (the normalizer passes unknown schemes through).
	wiki := t.TempDir()
	gitEnv := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	mk := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	mk(wiki, "init", "-q", "-b", "main", "--bare")
	seed := t.TempDir()
	mk(seed, "init", "-q", "-b", "main")
	for _, doc := range []string{"DESIGN.md", "TOBE.md", "ARCHITECTURE.md"} {
		if err := os.WriteFile(filepath.Join(seed, doc), []byte("# "+doc+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(seed, "add", "-A")
	mk(seed, "commit", "-qm", "docs")
	mk(seed, "push", "-q", "file://"+wiki, "main")

	spec := func(url string) string {
		return `{"config":{"repoWiki":"` + url + `"}}`
	}
	readState := func(t *testing.T, work string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(work, "pr-context.json"))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, "repo_wiki_state")
		if i < 0 {
			return "(no stamp)"
		}
		end := i + 40
		if end > len(s) {
			end = len(s)
		}
		return s[i:end]
	}

	t.Run("declared: cloned at the promised path, logged, state stamped", func(t *testing.T) {
		work := t.TempDir()
		out := runRepoWikiBlock(t, work, spec("file://"+wiki))
		for _, doc := range []string{"DESIGN.md", "TOBE.md", "ARCHITECTURE.md"} {
			if _, err := os.Stat(filepath.Join(work, "repo-wiki", doc)); err != nil {
				t.Errorf("repo-wiki/%s missing: %v (out=%s)", doc, err, out)
			}
		}
		if !strings.Contains(out, "repo-wiki cloned") {
			t.Errorf("the clone must be logged (config facts never drop silently): %s", out)
		}
		if !strings.Contains(readState(t, work), "cloned") {
			t.Errorf("pr-context.json must carry repo_wiki_state=cloned, got %s", readState(t, work))
		}
	})

	t.Run("valid spec, field undeclared, STALE dir from a prior round: cleared, stated, no stale docs", func(t *testing.T) {
		work := t.TempDir()
		// a previous round's docs sit in the workdir; the field is GONE
		// from the spec — the reviewer must not read them as this
		// round's ground truth (review r1 finding: conditional clear).
		stale := filepath.Join(work, "repo-wiki", "TOBE.md")
		if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stale, []byte("# stale foreign docs\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := runRepoWikiBlock(t, work, `{"config":{"label":"needs-review"}}`)
		if _, err := os.Stat(filepath.Join(work, "repo-wiki")); !os.IsNotExist(err) {
			t.Fatal("undeclared repoWiki must CLEAR a stale repo-wiki dir — foreign docs would be read as ground truth")
		}
		if !strings.Contains(out, "not declared") {
			t.Errorf("undeclared must be stated in the log, got: %s", out)
		}
		if !strings.Contains(readState(t, work), "absent") {
			t.Errorf("pr-context.json must carry repo_wiki_state=absent, got %s", readState(t, work))
		}
	})

	t.Run("malformed SPEC config: distinct WARN, treated as undeclared", func(t *testing.T) {
		work := t.TempDir()
		out := runRepoWikiBlock(t, work, `{"config":{"`)
		if !strings.Contains(out, "WARN: repo-wiki: SPEC config is not valid JSON") {
			t.Fatalf("malformed config must WARN distinctly (silent absent hides a k8s-config typo), got: %s", out)
		}
		if _, err := os.Stat(filepath.Join(work, "repo-wiki")); !os.IsNotExist(err) {
			t.Fatal("malformed spec must not leave a dir")
		}
	})

	t.Run("unreachable wiki: WARN names the declared URL, state says clone-failed", func(t *testing.T) {
		work := t.TempDir()
		out := runRepoWikiBlock(t, work, spec("file:///nonexistent/wiki.git"))
		if !strings.Contains(out, "WARN") || !strings.Contains(out, "config.repoWiki") {
			t.Fatalf("a failed clone must WARN and name the config, got: %s", out)
		}
		if !strings.Contains(readState(t, work), "clone-failed") {
			t.Errorf("pr-context.json must carry repo_wiki_state=clone-failed, got %s", readState(t, work))
		}
	})
}

// The normalizer: declarative URLs arrive in operator shapes; the clone
// chain must reduce each to the authenticated https form — with the
// credential actually observed (sentinel env), ports stripped on
// scheme-ful shapes, and the scp colon-split applied ONLY where a colon
// is a separator (scheme-less shorthand).
func TestHostHttpsUrl(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	gitHost, err := filepath.Abs(filepath.Join("..", "..", "plugins", "lib", "git-host.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// sentinel chain: the credential table is exercised for real — a
	// scrubbed-env table can never see token placement (review r1:
	// deleting the token calls shipped green under it).
	run := func(url string, env ...string) string {
		cmd := exec.Command(bash, "-c", ". "+gitHost+"; host::https_url '"+url+"'")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir()}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("https_url %q: %v — %s", url, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// token-less shapes (scrubbed): structure + passthroughs
	for _, tc := range []struct{ in, want string }{
		{"git@gitlab.com:x/y.git", "https://gitlab.com/x/y.git"},
		{"weird", "weird"},
	} {
		if got := run(tc.in); got != tc.want {
			t.Errorf("https_url(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// sentinel shapes: the credential lands in the userinfo position,
	// per host, including port-bearing declarations (review r1: the
	// port was leaking into the path segment).
	for _, tc := range []struct {
		in, want string
		env      []string
	}{
		{"ssh://git@git.rezus.cloud/tibrez/rhesadox.wiki.git", "https://tibrez:FJSENT@git.rezus.cloud/tibrez/rhesadox.wiki.git", []string{"HARMOSTES_FORGEJO_TOKEN=FJSENT"}},
		{"ssh://git@git.rezus.cloud:2222/tibrez/rhesadox.wiki.git", "https://tibrez:FJSENT@git.rezus.cloud/tibrez/rhesadox.wiki.git", []string{"HARMOSTES_FORGEJO_TOKEN=FJSENT"}},
		{"https://git.rezus.cloud:8443/tibrez/rhesadox.wiki.git", "https://tibrez:FJSENT@git.rezus.cloud/tibrez/rhesadox.wiki.git", []string{"HARMOSTES_FORGEJO_TOKEN=FJSENT"}},
		{"https://github.com/foo/bar.wiki", "https://x-access-token:GHSENT@github.com/foo/bar.wiki.git", []string{"HARMOSTES_GITHUB_TOKEN=GHSENT"}},
		{"ssh://git@codeberg.org/a/b.git", "https://CBSENT@codeberg.org/a/b.git", []string{"HARMOSTES_CODEBERG_TOKEN=CBSENT"}},
		{"ssh://git@host.example.com:7999/o/r.wiki.git", "https://host.example.com/o/r.wiki.git", nil},
	} {
		if got := run(tc.in, tc.env...); got != tc.want {
			t.Errorf("https_url(%q, %v) = %q, want %q", tc.in, tc.env, got, tc.want)
		}
	}
	// delegation (review r1): the shape parser must agree byte-for-byte
	// with host::clone_url — the credential table lives there once.
	del := exec.Command(bash, "-c", ". "+gitHost+`; echo "$(host::https_url 'ssh://git@git.rezus.cloud:2222/t/rhesadox.wiki.git')|$(host::clone_url git.rezus.cloud t/rhesadox.wiki)"`)
	del.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "HARMOSTES_FORGEJO_TOKEN=FJSENT"}
	out, err := del.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) != 2 || parts[0] != parts[1] {
		t.Errorf("https_url must delegate to clone_url byte-identically: %q vs %q", parts[0], parts[1])
	}
}
