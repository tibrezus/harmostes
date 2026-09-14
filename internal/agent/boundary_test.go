package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBotTokenBoundaryClosure pins the credential boundary mechanically
// (#480 r9 t16/t18, r10 t20): two scans over the repo source.
//
//  1. Sprawl: the raw env-key literal may appear ONLY in this package (the
//     constants), the plugin's bash (cannot import Go), the chart rendering,
//     and tests. Any other occurrence fails CI — new sites reference the
//     constants via ChildEnv/botTokenEnvForNode.
//  2. Exec-site discipline: every non-test Go file under cmd/+internal/ that
//     spawns a child (exec.Command/exec.CommandContext) must route that child
//     through ChildEnv or enumerate cmd.Env per spawn. This is the honest
//     form of the closure — a scrub that misses a leaf contains no literal,
//     so only counting spawns against env-assignments can see it. The scan is
//     file-granular and states its limits: behavioural per-leaf tests
//     (TestCmdGateScrubBotToken et al.) carry the semantics.
func TestBotTokenBoundaryClosure(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allowedPrefixes := []string{
		filepath.Join(root, "internal", "agent"),
		filepath.Join(root, "plugins", "post-review"),
		filepath.Join(root, "chart"),
	}
	skipDirs := map[string]bool{
		".git": true, "vendor": true, "node_modules": true,
		".llm-wiki": true, "ui-dist": true,
		".agents": true, "submodules": true, // submodule working trees — other repos
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		isShOrYaml := (strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".yaml")) &&
			!strings.HasSuffix(name, "_test.sh")
		if !isGo && !isShOrYaml {
			return nil
		}
		allowed := false
		for _, p := range allowedPrefixes {
			if strings.HasPrefix(path, p+string(filepath.Separator)) {
				allowed = true
				break
			}
		}
		if allowed {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(data)
		if strings.Contains(src, "HARMOSTES_FORGEJO_BOT_TOKEN") ||
			strings.Contains(src, "HARMOSTES_FORGEJO_BOT_HOST") {
			t.Errorf("%s: credential-boundary literal outside the sanctioned set — reference agent.BotTokenEnvKey/agent.BotHostEnvKey (or ChildEnv) instead; a drift between sites is fail-open (r9 t16)", path)
			return nil
		}
		// exec-site discipline (scan 2) — Go sources under cmd/+internal only.
		// Strict spawn:assignment counting — NO ChildEnv escape hatch: a file
		// that mentions the helper elsewhere must still enumerate env for
		// EVERY spawn (that leniency is exactly what would have hidden the
		// workspace git legs, r10 t20).
		if isGo && (strings.HasPrefix(path, filepath.Join(root, "cmd")+string(filepath.Separator)) ||
			strings.HasPrefix(path, filepath.Join(root, "internal")+string(filepath.Separator))) {
			spawns := strings.Count(src, "exec.Command(") + strings.Count(src, "exec.CommandContext(")
			envSets := strings.Count(src, ".Env =")
			if spawns > 0 && envSets < spawns {
				t.Errorf("%s: %d exec spawn(s) but only %d child-env assignments — a spawn inherits the process env verbatim and the bot credential rides it (r10 t20)", path, spawns, envSets)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
