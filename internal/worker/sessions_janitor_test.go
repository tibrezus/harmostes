package worker

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The janitor prunes by IDLENESS: a lineage dir survives while its
// newest file is within TTL — pi rewrites the session file on every
// resumed turn, so active PRs are structurally never pruned; closed
// PRs' lineages expire. Empty dirs prune (zero mtime predates every
// cutoff); non-dir entries are never touched.
func TestJanitorSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	// idle lineage: newest file 30d old (beyond the 14d TTL) → pruned
	idle := filepath.Join(root, "repo-11111111~41")
	if err := os.MkdirAll(idle, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * 24 * time.Hour)
	if err := os.WriteFile(filepath.Join(idle, "x.jsonl"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(idle, "x.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	// active lineage: touched now → survives
	active := filepath.Join(root, "repo-22222222~42")
	if err := os.MkdirAll(filepath.Join(active, "sol-pi", "sess"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "sol-pi", "sess", "epr.json"), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}

	// empty dir: zero mtime → prunes first
	empty := filepath.Join(root, "repo-33333333~43")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	// stray FILE at root: never touched
	stray := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed := JanitorSessions(root, 14*24*time.Hour, now)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (idle + empty)", removed)
	}
	for _, gone := range []string{idle, empty} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s must be pruned", gone)
		}
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active lineage must survive: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("stray file must be untouched: %v", err)
	}

	// Non-positive TTL never prunes; missing root is a no-op.
	if removed := JanitorSessions(root, 0, now); removed != 0 {
		t.Fatalf("zero TTL must not prune, removed %d", removed)
	}
	if removed := JanitorSessions(filepath.Join(root, "absent"), time.Hour, now); removed != 0 {
		t.Fatalf("absent root must be a no-op, removed %d", removed)
	}
}

// newestFileMtime walks the whole subtree: the sol-pi dir UNDER a lineage
// can be newer than an old session file — the lineage survives on its
// newest ANYTHING.
func TestNewestFileMtimeSubtree(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sol-pi", "s")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "s.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "occ.json"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := newestFileMtime(dir); !got.After(now.Add(-time.Hour)) {
		t.Fatalf("subtree mtime must track the newest file anywhere, got %v", got)
	}
	if got := newestFileMtime(filepath.Join(dir, "absent")); !got.IsZero() {
		t.Fatalf("absent dir = zero time, got %v", got)
	}
}
