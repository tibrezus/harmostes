package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineageDirDeterministicAndIsolated(t *testing.T) {
	a1 := LineageDir("/s", "git.rezus.cloud/tibrez/rhesadox", "99")
	a2 := LineageDir("/s", "git.rezus.cloud/tibrez/rhesadox", "99")
	if a1 != a2 {
		t.Fatalf("lineage must be deterministic: %q vs %q", a1, a2)
	}
	if b := LineageDir("/s", "git.rezus.cloud/tibrez/rhesadox", "100"); b == a1 {
		t.Fatalf("different PRs must not share a lineage: %q", b)
	}
	// Sanitizer colliders must stay distinct: "a_b/c" and "a/b-c" both
	// sanitize to "a-b-c" — the repo hash separates them.
	x := LineageDir("/s", "a_b/c", "1")
	y := LineageDir("/s", "a/b-c", "1")
	if x == y {
		t.Fatalf("sanitizer colliders must not collide: %q", x)
	}
	if strings.Contains(a1, "/") && !strings.HasPrefix(a1, "/s/") {
		t.Fatalf("lineage must live under the root: %q", a1)
	}
}

func TestResolveSessionFreshThenResume(t *testing.T) {
	root := t.TempDir()
	dir, id, resume, err := ResolveSession(root, "git.rezus.cloud/tibrez/rhesadox", "99")
	if err != nil || dir == "" || resume {
		t.Fatalf("fresh lineage: dir=%q resume=%v err=%v", dir, resume, err)
	}
	if id != "harmostes-99" {
		t.Fatalf("stable id expected, got %q", id)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("lineage dir must be created eagerly: %v", err)
	}
	// A session file appears (pi wrote one in a prior run) → the NEXT
	// spawn must see resume=true, same dir, same id.
	if err := os.WriteFile(filepath.Join(dir, "harmostes-99.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir2, id2, resume2, err := ResolveSession(root, "git.rezus.cloud/tibrez/rhesadox", "99")
	if err != nil || !resume2 || dir2 != dir || id2 != id {
		t.Fatalf("existing session must RESUME in place: dir=%q id=%q resume=%v err=%v", dir2, id2, resume2, err)
	}
}

// r20 P4 blocker: the PR half must never carry path syntax — traversal
// probe ("../../outside") must be refused, not joined.
func TestSanitizePRTraversalRefused(t *testing.T) {
	for _, evil := range []string{"../evil", "../../outside", "", "99/../x", "../../../etc"} {
		if SanitizePR(evil) {
			t.Fatalf("SanitizePR must refuse %q", evil)
		}
		if _, _, _, err := ResolveSession(t.TempDir(), "host/o/r", evil); err != ErrNotAPR {
			t.Fatalf("ResolveSession must refuse %q with ErrNotAPR, got %v", evil, err)
		}
	}
	if !SanitizePR("99") || !SanitizePR("12345678") {
		t.Fatal("numeric PRs must pass")
	}
	if SanitizePR("123456789") {
		t.Fatal("absurdly long pr refused")
	}
}
