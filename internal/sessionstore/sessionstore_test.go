package sessionstore

import (
	"fmt"
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

// ── #704: LinkSharedStore — the repository-shared sol-pi store. The
// lineage's sol-pi/ component becomes a symlink to the attachment mount,
// so the content-addressed observation objects land in one store per repo
// (shared across workflows, immune to lineage TTL). ──
func TestLinkSharedStore(t *testing.T) {
	logs := []string{}
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	mount := t.TempDir()
	lineage := t.TempDir()

	// Empty mount = undeclared: a strict no-op.
	LinkSharedStore(lineage, "", logf)
	if _, err := os.Lstat(filepath.Join(lineage, "sol-pi")); !os.IsNotExist(err) {
		t.Fatalf("undeclared must not create the link")
	}

	// Declared but the mount directory does not exist yet: the link is
	// created DANGLING (#704 review r2 — a real directory must never be
	// born in the gap; the next healthy arm re-points and recovers).
	LinkSharedStore(lineage, filepath.Join(mount, "missing"), logf)
	if target, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil || target != filepath.Join(mount, "missing") {
		t.Fatalf("declared-but-missing must still link: %q, %v", target, err)
	}
	if err := os.Remove(filepath.Join(lineage, "sol-pi")); err != nil {
		t.Fatal(err)
	}

	// The happy path: mount present → symlink created, pointing at it.
	if err := os.MkdirAll(filepath.Join(mount, "git.rezus.cloud-tibrez-rhesadox-d782cf64"), 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, mount, logf)
	target, err := os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != mount {
		t.Fatalf("link = %q, err = %v; want %q", target, err, mount)
	}

	// Idempotent: a second link to the same mount is a no-op.
	LinkSharedStore(lineage, mount, logf)
	target, err = os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != mount {
		t.Fatalf("relink changed the target: %q, %v", target, err)
	}

	// Re-pointing: a symlink at a stale mount is corrected.
	other := t.TempDir()
	LinkSharedStore(lineage, other, logf)
	target, err = os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != other {
		t.Fatalf("re-point = %q, %v; want %q", target, err, other)
	}

	// Legacy protection: a REAL directory is left alone — the PR keeps its
	// accumulated observations; sharing never orphans them.
	legacy := t.TempDir()
	legacyLink := filepath.Join(legacy, "sol-pi")
	if err := os.MkdirAll(legacyLink, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyLink, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs = nil
	LinkSharedStore(legacy, mount, logf)
	if st, err := os.Lstat(legacyLink); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy dir must stay a real directory: %v, %v", st, err)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "legacy") {
		t.Fatalf("legacy case must be observable: %v", logs)
	}
}

// ── #704 review r1+r2: link discipline across mount outages. r1: an
// UNDECLARED store must not leave a stale symlink behind (it would dangle
// forever — the env is gone, no later arm repairs it). r2: a DECLARED
// store must not have its link removed on a transient outage — the
// extension's `mkdir -p` would materialize a real sol-pi/ directory in
// the gap, and the legacy guard would honor that FOREVER (a transient
// failure permanently de-sharing the lineage). The discipline: declared =
// the link always exists (dangling while the mount is away — inert but
// recoverable); undeclared = our symlink is removed, real dirs kept. ──
func TestLinkSharedStoreLinkDiscipline(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	LinkSharedStore(lineage, mount, nil)
	link := filepath.Join(lineage, "sol-pi")
	if target, err := os.Readlink(link); err != nil || target != mount {
		t.Fatalf("setup: link = %q, %v", target, err)
	}

	// DECLARED but the mount transiently vanishes (CSI failover): the
	// link STAYS (dangling) — removing it would let the extension
	// materialize a real directory and lock the lineage out forever.
	if err := os.RemoveAll(mount); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, mount, nil)
	if target, err := os.Readlink(link); err != nil || target != mount {
		t.Fatalf("declared outage must keep the link (dangling is recoverable): %q, %v", target, err)
	}

	// The mount returns: the SAME link now resolves — recovery without
	// migration, no real directory was ever born.
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, mount, nil)
	if target, err := os.Readlink(link); err != nil || target != mount {
		t.Fatalf("recovery relink = %q, %v", target, err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the lineage must still be a symlink (never a manufactured real dir): %v %v", st, err)
	}

	// UNDECLARED after having been linked: the stale symlink is removed
	// (the env is gone — no later arm would ever repair it).
	LinkSharedStore(lineage, "", nil)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("undeclared must remove our stale symlink: %v", err)
	}

	// A REAL directory under the same undeclared condition is kept (it is
	// pre-#704 legacy data — nothing of ours to remove).
	real := t.TempDir()
	realLink := filepath.Join(real, "sol-pi")
	if err := os.MkdirAll(realLink, 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(real, "", nil)
	if st, err := os.Lstat(realLink); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy real dir must be untouched: %v %v", st, err)
	}

	// FRESH lineage + declared-but-broken mount: the link is created
	// DANGLING (os.Symlink succeeds against a missing target) — no real
	// directory is born in the gap, and the lineage recovers when the
	// store lands.
	fresh := t.TempDir()
	broken := filepath.Join(fresh, "not-mounted-yet")
	LinkSharedStore(fresh, broken, nil)
	if target, err := os.Readlink(filepath.Join(fresh, "sol-pi")); err != nil || target != broken {
		t.Fatalf("fresh lineage must get a (dangling) link: %q, %v", target, err)
	}
}

// The arm-time observability: a successful link logs the store's byte
// count, so the growth curve the shared store introduces is visible
// before exhaustion (retention decision: claim quota + this curve).
func TestLinkSharedStoreLogsStoreSize(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "git.rezus.cloud-tibrez-rhesadox-d782cf64", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "git.rezus.cloud-tibrez-rhesadox-d782cf64", "x", "obs_1.txt"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := []string{}
	LinkSharedStore(lineage, mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "10 bytes") {
		t.Fatalf("arm-time log must carry the store size, got: %v", logs)
	}
}
