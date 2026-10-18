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

// ── #704: the shared sol-pi store must be IMMUNE to the lineage TTL.
// With the repository-scoped store, <lineage>/sol-pi is a SYMLINK into a
// store shared by every workflow on the repo. The invariant that makes
// the feature safe: pruning a lineage unlinks the symlink WITHOUT
// traversing it — one idle PR's TTL must never delete a repository's
// shared research. A janitor refactor that follows links (WalkDir with
// FollowSegment, a resolve-then-remove "cleanup") would do exactly that,
// failing no existing test — this test is the tripwire. Companion
// assertion: store writes do not count as lineage activity
// (newestFileMtime ignores the linked subtree), so a quiet PR's lineage
// still prunes even though the shared store was touched recently. ──
func TestJanitorSessionsSparesSharedSolPiStore(t *testing.T) {
	root := t.TempDir()
	store := t.TempDir()
	sentinel := filepath.Join(store, "shared", "obs_1.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("research"), 0o600); err != nil {
		t.Fatal(err)
	}

	lineage := filepath.Join(root, "git.rezus.cloud-tibrez-rhesadox-d782cf64~99")
	if err := os.MkdirAll(lineage, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-400 * time.Hour)
	if err := os.Chtimes(lineage, old, old); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(lineage, "2026-01-01T00-00-00-000Z_harmostes-99.jsonl")
	if err := os.WriteFile(sessionFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sessionFile, old, old); err != nil {
		t.Fatal(err)
	}
	// The #704 shape: sol-pi is a SYMLINK into the shared store.
	if err := os.Symlink(store, filepath.Join(lineage, "sol-pi")); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	removed := JanitorSessions(root, 336*time.Hour, now)
	if removed != 1 {
		t.Fatalf("an idle lineage must still prune (removed=%d)", removed)
	}
	if _, err := os.Stat(lineage); !os.IsNotExist(err) {
		t.Fatalf("lineage must be gone: %v", err)
	}
	// THE invariant: the shared store survives the prune, byte for byte.
	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "research" {
		t.Fatalf("the shared store must survive a lineage prune: %v, %q", err, got)
	}

	// The other half: the linked subtree does not count as lineage
	// activity — a recent store write must not resurrect an idle lineage.
	if err := os.WriteFile(sentinel, []byte("touched-now"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(lineage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, filepath.Join(lineage, "sol-pi")); err != nil {
		t.Fatal(err)
	}
	if m := newestFileMtime(lineage); m.After(old.Add(time.Hour)) {
		t.Fatalf("newestFileMtime must not follow the shared-store link (got %v)", m)
	}
}
