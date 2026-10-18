package sessionstore

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
	LinkSharedStore(lineage, "pr-review-rhesadox", "", logf)
	if _, err := os.Lstat(filepath.Join(lineage, "sol-pi")); !os.IsNotExist(err) {
		t.Fatalf("undeclared must not create the link")
	}

	// Declared but the mount directory does not exist yet: the link is
	// created DANGLING (#704 review r2 — a real directory must never be
	// born in the gap; the next healthy arm re-points and recovers).
	LinkSharedStore(lineage, "pr-review-rhesadox", filepath.Join(mount, "missing"), logf)
	if target, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil || target != filepath.Join(mount, "missing", "pr-review-rhesadox") {
		t.Fatalf("declared-but-missing must still link (at the workflow leaf): %q, %v", target, err)
	}
	if err := os.Remove(filepath.Join(lineage, "sol-pi")); err != nil {
		t.Fatal(err)
	}

	// The happy path: mount present → symlink created, pointing at it.
	if err := os.MkdirAll(filepath.Join(mount, "git.rezus.cloud-tibrez-rhesadox-d782cf64"), 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, logf)
	target, err := os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != filepath.Join(mount, "pr-review-rhesadox") {
		t.Fatalf("link = %q, err = %v; want the workflow leaf", target, err)
	}

	// Idempotent: a second link to the same mount is a no-op.
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, logf)
	target, err = os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != filepath.Join(mount, "pr-review-rhesadox") {
		t.Fatalf("relink changed the target: %q, %v", target, err)
	}

	// Re-pointing: a symlink at a stale mount is corrected.
	other := t.TempDir()
	LinkSharedStore(lineage, "pr-review-rhesadox", other, logf)
	target, err = os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != filepath.Join(other, "pr-review-rhesadox") {
		t.Fatalf("re-point = %q, %v; want %q", target, err, filepath.Join(other, "pr-review-rhesadox"))
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
	LinkSharedStore(legacy, "pr-review-rhesadox", mount, logf)
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
	leaf := filepath.Join(mount, "pr-review-rhesadox")
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	link := filepath.Join(lineage, "sol-pi")
	if target, err := os.Readlink(link); err != nil || target != leaf {
		t.Fatalf("setup: link = %q, %v", target, err)
	}

	// DECLARED but the mount transiently vanishes (CSI failover): the
	// link STAYS (dangling) — removing it would let the extension
	// materialize a real directory and lock the lineage out forever.
	if err := os.RemoveAll(mount); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if target, err := os.Readlink(link); err != nil || target != leaf {
		t.Fatalf("declared outage must keep the link (dangling is recoverable): %q, %v", target, err)
	}

	// The mount returns: the SAME link now resolves — recovery without
	// migration, no real directory was ever born.
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if target, err := os.Readlink(link); err != nil || target != leaf {
		t.Fatalf("recovery relink = %q, %v", target, err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the lineage must still be a symlink (never a manufactured real dir): %v %v", st, err)
	}

	// UNDECLARED after having been linked: the stale symlink is removed
	// (the env is gone — no later arm would ever repair it).
	LinkSharedStore(lineage, "pr-review-rhesadox", "", nil)
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
	LinkSharedStore(real, "pr-review-rhesadox", "", nil)
	if st, err := os.Lstat(realLink); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy real dir must be untouched: %v %v", st, err)
	}

	// FRESH lineage + declared-but-broken mount: the link is created
	// DANGLING (os.Symlink succeeds against a missing target) — no real
	// directory is born in the gap, and the lineage recovers when the
	// store lands.
	fresh := t.TempDir()
	broken := filepath.Join(fresh, "not-mounted-yet")
	LinkSharedStore(fresh, "pr-review-rhesadox", broken, nil)
	if target, err := os.Readlink(filepath.Join(fresh, "sol-pi")); err != nil || target != filepath.Join(broken, "pr-review-rhesadox") {
		t.Fatalf("fresh lineage must get a (dangling) link at the workflow leaf: %q, %v", target, err)
	}
}

// The arm-time observability: a successful link logs the store's byte
// count, so the growth curve the shared store introduces is visible
// before exhaustion (retention decision: claim quota + this curve).
func TestLinkSharedStoreLogsStoreSize(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mount, "pr-review-rhesadox", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "pr-review-rhesadox", "x", "obs_1.txt"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := []string{}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "10 bytes") {
		t.Fatalf("arm-time log must carry the store size, got: %v", logs)
	}
}

// ── #704 review r2: the growth-curve signal must not under-report
// silently. A walk error (an entry removed mid-walk on the shared claim,
// a CSI blip) skips that entry and CARRIES the error — the log marks the
// scan INCOMPLETE instead of letting a partial total read as a small
// store. Probed red on the abort-on-first-error version. ──
func TestMeasureStoreScanErrorObservable(t *testing.T) {
	mount := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(mount, name), bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	healthy, files, capped, scanErr := measureStore(mount)
	if healthy != 300 || files != 3 || capped || scanErr != nil {
		t.Fatalf("healthy store: %d bytes/%d files capped=%v err=%v", healthy, files, capped, scanErr)
	}

	original := walkStore
	defer func() { walkStore = original }()
	walkStore = func(root string, fn fs.WalkDirFunc) error {
		count := 0
		return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			count++
			// Simulate a mid-walk removal the way the real walker
			// delivers it: a per-entry error handed TO the callback —
			// the walk continues iff the callback returns nil.
			if count == 2 {
				return fn(path, d, fmt.Errorf("removal race: ENOENT"))
			}
			return fn(path, d, err)
		})
	}
	total, files, capped, scanErr := measureStore(mount)
	if total == 300 {
		t.Fatalf("a scan that lost entries must not report the healthy total")
	}
	if scanErr == nil {
		t.Fatal("the scan error must be carried, not swallowed")
	}
	_, _, _, _ = capped, files, total, scanErr
}

// ── #704 review r3/r4: the idempotent arm is the COMMON arm (every
// re-review), so the outage signal must fire there too — with the same
// wording as the create path, not a "0 bytes … real total is LARGER"
// line that asserts the opposite of the state (an ABSENT store is not an
// unmeasurably large one). ──
func TestLinkSharedStoreIdempotentArmNamesOutage(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	original2 := symlinkStore
	defer func() { symlinkStore = original2 }()
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	// The steady-state arm measures the LEAF — create it (the extension
	// would have, in production).
	if err := os.MkdirAll(filepath.Join(mount, "pr-review-rhesadox"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Steady state, store present: no DEGRADED line.
	logs := []string{}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	for _, l := range logs {
		if strings.Contains(l, "DEGRADED") {
			t.Fatalf("healthy steady-state arm must not log DEGRADED: %v", logs)
		}
	}

	// Outage on the idempotent arm: the mount path is genuinely
	// uncreatable (its parent is a file — ENOTDIR, not the r6
	// MkdirAll-recreates-the-mount-point case), so the arm must name the
	// outage with the shared DEGRADED wording.
	blockedParent := filepath.Join(mount, "..", "blocker")
	_ = blockedParent
	blockedBase := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedBase, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(blockedBase, "file", "mnt") // ENOTDIR all the way
	// Re-point the existing link lineage at the blocked path.
	symlinkOriginal := symlinkStore
	symlinkStore = func(oldname, newname string) error { return fmt.Errorf("ENOTDIR") }
	_ = symlinkOriginal
	logs = nil
	LinkSharedStore(lineage, "pr-review-rhesadox", blocked, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	symlinkStore = original2
	if len(logs) == 0 || !strings.Contains(logs[0], "DEGRADED") || !strings.Contains(logs[0], "not mounted") {
		t.Fatalf("idempotent arm must name the outage with the shared wording, got: %v", logs)
	}
}

// measureStore distinguishes an ABSENT root from a partially-scanned
// store — the caller must not render the former as "the real total is
// LARGER".
func TestMeasureStoreRootMissingIsItsOwnState(t *testing.T) {
	_, _, _, scanErr := measureStore(filepath.Join(t.TempDir(), "never-created"))
	if scanErr == nil || !errors.Is(scanErr, ErrStoreRootMissing) {
		t.Fatalf("root-missing must surface as ErrStoreRootMissing, got: %v", scanErr)
	}
}

// ── #704 review r4/r5: the sentinel is a CONTRACT on the rendered line.
// A mount that stats fine but is not listable (claim uid mismatch,
// root_squash) makes the walk fail at the ROOT: the arm-time line must
// say UNREADABLE (check uid/fsGroup) — never "not mounted", never
// "the real total is LARGER". Probes the caller, not just the producer. ──
func TestLogStoreSizeRootMissingRendersUnreadable(t *testing.T) {
	statable := t.TempDir() // exists, IsDir — passes every Stat guard
	lineage := t.TempDir()
	LinkSharedStore(lineage, "pr-review-rhesadox", statable, nil)

	original := walkStore
	defer func() { walkStore = original }()
	walkStore = func(root string, fn fs.WalkDirFunc) error {
		return fn(root, nil, ErrStoreRootMissing) // the walk's first error IS the root
	}

	logs := []string{}
	logStoreSize(statable, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if len(logs) != 1 {
		t.Fatalf("expected exactly one line, got %v", logs)
	}
	line := logs[0]
	if !strings.Contains(line, "UNREADABLE") || !strings.Contains(line, "uid/fsGroup") {
		t.Fatalf("root-missing must render as UNREADABLE with the uid hint: %q", line)
	}
	if strings.Contains(line, "LARGER") || strings.Contains(line, "not mounted") {
		t.Fatalf("root-missing must not render as partial-scan or not-mounted: %q", line)
	}
}

// ── #704 review r5: creation is atomic. A failing os.Symlink used to
// leave the component ABSENT — the byte-pristine extension's mkdir -p
// then materialized a real directory in the gap, the legacy guard
// honored it forever, and the lineage was permanently de-shared with a
// log line that read as deliberate. The linker now creates through a
// temp name and renames: a failed create leaves the old state untouched,
// and no gap exists. Probed red via the symlinkStore seam. ──
func TestLinkSharedStoreAtomicCreate(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()

	original := symlinkStore
	defer func() { symlinkStore = original }()

	// The re-point case is the sharpest: a WORKING link exists, and the
	// create to the new store fails (EDQUOT/EROFS/EACCES). Atomicity means
	// the old link still resolves — the non-atomic version removed it
	// first, leaving the component absent for the extension's mkdir -p to
	// fill with a real directory (the manufactured-legacy gap).
	if err := os.MkdirAll(filepath.Join(mount, "pr-review-rhesadox"), 0o700); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if _, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil {
		t.Fatalf("setup: %v", err)
	}
	other := t.TempDir()
	symlinkStore = func(oldname, newname string) error { return fmt.Errorf("EDQUOT: claim full") }
	logs := []string{}
	LinkSharedStore(lineage, "pr-review-rhesadox", other, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if target, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil || target != filepath.Join(mount, "pr-review-rhesadox") {
		t.Fatalf("a failed re-point must leave the OLD link resolving (atomic): %q, %v", target, err)
	}

	// The fresh-lineage case (r7 contract): a failed create leaves a
	// STAMPED real directory — never absent (the extension's mkdir -p
	// would fill an absent component with an unmarked real dir), never
	// unmarked (the legacy guard would honor it forever).
	fresh := t.TempDir()
	symlinkStore = func(oldname, newname string) error { return fmt.Errorf("EROFS: read-only") }
	LinkSharedStore(fresh, "pr-review-rhesadox", filepath.Join(fresh, "store"), func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	freshLink := filepath.Join(fresh, "sol-pi")
	if st, err := os.Lstat(freshLink); err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		t.Fatalf("a failed fresh create must leave a stamped real directory: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(freshLink, manufacturedMarker)); err != nil {
		t.Fatalf("the fallback must carry the manufactured marker: %v", err)
	}

	// The extension materializes a real directory in the gap ONLY when the
	// component is absent and we returned early — with the atomic rename
	// there is no gap, so simulate the pre-fix failure mode's aftermath
	// and prove the healthy arm still recovers a lineage that has NO real
	// dir. (A lineage where a real dir DID get born is pre-#704/legacy —
	// covered by the legacy tests.)
	symlinkStore = original
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	target, err := os.Readlink(filepath.Join(lineage, "sol-pi"))
	if err != nil || target != filepath.Join(mount, "pr-review-rhesadox") {
		t.Fatalf("the next healthy arm must share: %q, %v", target, err)
	}
}

// ── #704 review r5: the growth curve must move past the scan budget —
// a frozen constant reads as a store that stopped growing while the
// claim fills. Past the file cap the walk keeps summing and the log
// reports a monotone LOWER BOUND. Probed red on the frozen version. ──
func TestMeasureStoreCappedKeepsGrowing(t *testing.T) {
	mount := t.TempDir()
	// maxFiles tiny entries…
	for i := 0; i < measureMaxFiles; i++ {
		dir := filepath.Join(mount, fmt.Sprintf("d%d", i/500))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t1, f1, capped1, _ := measureStore(mount)
	if !capped1 {
		t.Fatalf("fixture must trip the cap (files=%d)", f1)
	}
	// …then one oversized object lands.
	big := filepath.Join(mount, "zz_big", "obs_big.txt")
	if err := os.MkdirAll(filepath.Dir(big), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, bytes.Repeat([]byte("y"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	t2, f2, capped2, _ := measureStore(mount)
	if !capped2 {
		t.Fatal("still capped")
	}
	// The r7 contract: past the size budget the COUNT keeps moving (the
	// curve never freezes) while the byte figure is an honest measured
	// lower bound — no Info() lstat storm before pi spawns.
	if f2 <= measureMaxFiles {
		t.Fatalf("the count must keep moving past the size budget: %d", f2)
	}
	if t2 < t1 {
		t.Fatalf("the measured bytes must never go down: %d -> %d", t1, t2)
	}
}

// ── #704 review r5: the "sol-pi" component is a cross-boundary contract
// with the vendored extension (runtime-paths.ts joins it into the
// runtime root). Nothing else pins it; an upstream rename would de-share
// every lineage while the arm-time log still reports a healthy store.
// Same wiring-claim pattern as TestRigGraphPathSingleSource. ──
func TestSolPiRuntimeComponentSingleSource(t *testing.T) {
	src, err := os.ReadFile("../../extensions/sol-pi/src/sol-pi/runtime-paths.ts")
	if err != nil {
		t.Skipf("vendored extension not present: %v", err)
	}
	want := `join(sessionDir, "` + LinkComponent + `", sessionId)`
	if !strings.Contains(string(src), want) {
		t.Errorf("runtime-paths.ts no longer joins %q — LinkSharedStore's symlink and the extension's runtime root have drifted apart", LinkComponent)
	}
}

// ── #704 review r6: the leaf cannot self-heal — the only writer is the
// byte-pristine extension, writing THROUGH the link (mkdir → ENOTDIR on
// a dangling component). The linker creates the workflow leaf at arm
// time; a mount without the leaf must end the arm with the leaf existing
// and the link resolving into it. ──
func TestLinkSharedStoreCreatesMissingLeaf(t *testing.T) {
	mount := t.TempDir() // NO leaf — the shape the fleet actually runs
	lineage := t.TempDir()

	LinkSharedStore(lineage, "pr-review-other", mount, nil)

	leaf := filepath.Join(mount, "pr-review-other")
	if st, err := os.Stat(leaf); err != nil || !st.IsDir() {
		t.Fatalf("the linker must create the workflow leaf (the extension cannot): %v", err)
	}
	if target, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil || target != leaf {
		t.Fatalf("link = %q, %v; want %q", target, err, leaf)
	}
}

// The growth signal measures the MOUNT — the whole repo tree shares the
// claim's quota, so data written by OTHER workflows' leaves must be
// visible to this workflow's arm-time curve (r6: the leaf-only scope
// under-reports by a factor of N workflows).
func TestLinkSharedStoreGrowthSignalSeesWholeMount(t *testing.T) {
	mount := t.TempDir()
	// Another workflow's leaf holds 5 bytes of objects.
	otherData := filepath.Join(mount, "pr-review-someoneelse", "obs_x.txt")
	if err := os.MkdirAll(filepath.Dir(otherData), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherData, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}

	lineage := t.TempDir()
	logs := []string{}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "5 bytes") {
		t.Fatalf("the growth signal must see the whole repo mount (other workflows' data), got: %v", joined)
	}
}

// ── #704 review r7: the degraded fallback is OWNED, not orphaned. A
// fresh lineage during a creation failure gets a REAL sol-pi directory
// stamped with the manufactured marker (never absent, never unmarked);
// the next healthy arm migrates its contents into the shared store and
// replaces it with the link. ──
func TestLinkSharedStoreManufacturedFallbackMigrates(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()

	// Arm under a failing symlink on a FRESH lineage: the component must
	// become a real directory stamped manufactured (never absent).
	original := symlinkStore
	defer func() { symlinkStore = original }()
	symlinkStore = func(oldname, newname string) error { return fmt.Errorf("EROFS: read-only") }
	LinkSharedStore(lineage, "pr-review-rhesadox", filepath.Join(mount, "gone"), nil)
	fallback := filepath.Join(lineage, "sol-pi")
	if st, err := os.Lstat(fallback); err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		t.Fatalf("a failed fresh create must leave a stamped real directory (never absent): %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(fallback, manufacturedMarker)); err != nil {
		t.Fatalf("the fallback must carry the manufactured marker: %v", err)
	}

	// The extension writes an observation into the fallback during the
	// degraded window.
	obs := filepath.Join(fallback, "harmostes-706", "observation-pack", "objects", "obs_1.txt")
	if err := os.MkdirAll(filepath.Dir(obs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(obs, []byte("observed"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The mount returns: the healthy arm migrates the observation into
	// the shared store and replaces the fallback with the link.
	symlinkStore = original
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if st, err := os.Lstat(fallback); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the healthy arm must replace the fallback with the link: %v %v", st, err)
	}
	migrated, err := os.ReadFile(filepath.Join(mount, "pr-review-rhesadox", "harmostes-706", "observation-pack", "objects", "obs_1.txt"))
	if err != nil || string(migrated) != "observed" {
		t.Fatalf("the degraded-window observation must migrate into the store: %v %q", err, migrated)
	}
	if _, err := os.Stat(fallback + ".new"); !os.IsNotExist(err) {
		t.Fatalf("no stale temp may survive: %v", err)
	}
}

// A real directory WITHOUT the marker is pre-#704 legacy data — kept,
// never migrated (the marker is what makes ownership honest, r7).
func TestLinkSharedStoreLegacyUntouchedByMigration(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	legacy := filepath.Join(lineage, "sol-pi")
	if err := os.MkdirAll(filepath.Join(legacy, "harmostes-99"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "harmostes-99", "obs_old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if st, err := os.Lstat(legacy); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("unmarked legacy data must stay local: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(mount, "pr-review-rhesadox", "harmostes-99")); !os.IsNotExist(err) {
		t.Fatal("legacy data must not be migrated")
	}
}

// The stale-temp poisoning: a leftover <link>.new from an arm killed
// between Symlink and Rename must not make every later arm fail EEXIST.
func TestLinkSharedStoreClearedStaleTemp(t *testing.T) {
	mount := t.TempDir()
	lineage := t.TempDir()
	if err := os.WriteFile(filepath.Join(lineage, "sol-pi.new"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, nil)
	if target, err := os.Readlink(filepath.Join(lineage, "sol-pi")); err != nil || target != filepath.Join(mount, "pr-review-rhesadox") {
		t.Fatalf("a stale temp must not block linking: %q, %v", target, err)
	}
}

// The three failure states are named separately (r7): uncreatable leaf
// on a PRESENT mount says UNWRITABLE, never "not mounted" — and does not
// contradict itself with a healthy byte count in the same arm.
func TestLinkSharedStoreUnwritableLeafSaysUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits — the unwritable state is not simulatable")
	}
	mount := t.TempDir()
	if err := os.Chmod(mount, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(mount, 0o700) })
	lineage := t.TempDir()
	logs := []string{}
	LinkSharedStore(lineage, "pr-review-rhesadox", mount, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	if len(logs) == 0 || !strings.Contains(logs[0], "uncreatable") {
		t.Fatalf("an unwritable claim must say UNWRITABLE, got: %v", logs)
	}
	for _, l := range logs {
		if strings.Contains(l, "not mounted") {
			t.Fatalf("a present-but-unwritable mount must not say 'not mounted': %v", logs)
		}
		if strings.Contains(l, "bytes across") {
			t.Fatalf("a failed leaf creation must not be followed by a healthy byte count: %v", logs)
		}
	}
}
