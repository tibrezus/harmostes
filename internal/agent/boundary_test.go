package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBotTokenBoundaryClosure pins the credential boundary mechanically
// (#480 r9 t16/t18): the raw env-key literal may appear ONLY in this
// package (the constants), the plugin's bash (cannot import Go), the chart
// rendering, and tests. Any other occurrence — a new exec site spelling the
// name itself, a scrub that missed a leaf — fails CI. Exec sites obtain the
// credential boundary via ChildEnv / botTokenEnvForNode, which reference the
// constants here.
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
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// vendor, .git, node_modules are not source
			switch info.Name() {
			case ".git", "vendor", "node_modules", ".llm-wiki", "ui-dist":
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sh") && !strings.HasSuffix(name, ".yaml") {
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil // tests may spell the literal in fixtures
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
		if strings.Contains(string(data), "HARMOSTES_FORGEJO_BOT_TOKEN") ||
			strings.Contains(string(data), "HARMOSTES_FORGEJO_BOT_HOST") {
			t.Errorf("%s: credential-boundary literal outside the sanctioned set — reference agent.BotTokenEnvKey/agent.BotHostEnvKey (or ChildEnv) instead; a drift between sites is fail-open (r9 t16)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
