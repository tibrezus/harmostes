package worker

// #726: the repository wiki's clone is a golden block in workspace.sh
// (REPO-WIKI markers) — this runs the SHIPPED block verbatim with the
// SPEC envelope the worker wires, against a local bare repo standing
// in for the wiki, in both URL shapes the declarative field accepts.

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
func runRepoWikiBlock(t *testing.T, workDir, specJSON string) string {
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
	block := "set -euo pipefail\n" +
		"log() { echo \"[workspace] $*\"; }\n" +
		// workspace.sh sources the lib at its top; the block calls into it
		". \"$(dirname \"$0\")/../lib/git-host.sh\" 2>/dev/null || . " + gitHostAbs + "\n" +
		"WORKDIR=\"${HARMOSTES_WORKDIR:-/workspace}\"\n" +
		"SPEC=\"$HARMOSTES_SPEC\"\n" +
		extractRepoWikiBlock(t) + "\n"
	cmd := exec.Command(bash, "-c", block)
	cmd.Env = append(os.Environ(),
		"HARMOSTES_WORKDIR="+workDir,
		"HARMOSTES_SPEC="+specJSON,
		"PATH="+filepath.Dir(gitBin)+":"+os.Getenv("PATH"),
	)
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
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wiki
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", "--bare")
	seed := t.TempDir()
	run2 := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = seed
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	run2("init", "-q", "-b", "main")
	for _, doc := range []string{"DESIGN.md", "TOBE.md", "ARCHITECTURE.md"} {
		if err := os.WriteFile(filepath.Join(seed, doc), []byte("# "+doc+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run2("add", "-A")
	run2("commit", "-qm", "docs")
	run2("push", "-q", "file://"+wiki, "main")

	spec := func(url string) string {
		return `{"config":{"repoWiki":"` + url + `"}}`
	}

	t.Run("declared: cloned at the promised path, logged", func(t *testing.T) {
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
	})

	t.Run("absent field: no clone, no dir, no crash", func(t *testing.T) {
		work := t.TempDir()
		runRepoWikiBlock(t, work, `{"config":{"}}`)
		if _, err := os.Stat(filepath.Join(work, "repo-wiki")); !os.IsNotExist(err) {
			t.Fatal("undeclared repoWiki must not create the dir")
		}
	})

	t.Run("unreachable wiki: WARN, round proceeds", func(t *testing.T) {
		work := t.TempDir()
		out := runRepoWikiBlock(t, work, spec("file:///nonexistent/wiki.git"))
		if !strings.Contains(out, "WARN") {
			t.Fatalf("a failed clone must WARN (non-fatal), got: %s", out)
		}
	})
}

// The normalizer: declarative URLs arrive in operator shapes; the clone
// chain must reduce each to the authenticated https form.
func TestHostHttpsUrl(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// host::token with no chain resolves empty in the test env — the
	// token env is scrubbed so the outputs are EXACT (a prefix/suffix
	// pair let a garbage middle through in probe; exact strings pin the
	// whole normalized shape).
	run := func(url string) string {
		cmd := exec.Command(bash, "-c", ". "+filepath.Join("..", "..", "plugins", "lib", "git-host.sh")+"; host::https_url '"+url+"'")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir()}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("https_url %q: %v — %s", url, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, tc := range []struct{ in, want string }{
		{"ssh://git@git.rezus.cloud/tibrez/rhesadox.wiki.git", "https://tibrez:@git.rezus.cloud/tibrez/rhesadox.wiki.git"},
		{"https://github.com/foo/bar.wiki", "https://x-access-token:@github.com/foo/bar.wiki.git"},
		{"git@gitlab.com:x/y.git", "https://gitlab.com/x/y.git"},
		{"ssh://git@codeberg.org/a/b.git", "https://@codeberg.org/a/b.git"},
		{"weird", "weird"},
	} {
		if got := run(tc.in); got != tc.want {
			t.Errorf("https_url(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
