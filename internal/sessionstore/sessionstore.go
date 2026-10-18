// Package sessionstore owns the durable per-PR agent lineage (ADR-0010):
// identity, scoping, fetch, publish, the monotonic generation counter, and
// the pi session-file mechanics.
//
// One concept, one module. The Dapr PRLineage actor and the RWX lineage
// claim are ADAPTERS at this seam — before #516 the publish contract lived
// in four packages at once (identity in agent, state in agentlineage,
// materialization in the worker, mount scheme in k8s), and a single
// contract bug (#497's inert publish) required debugging all of them.
package sessionstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotAPR marks a pointer whose PR half is not a plain number — the run
// keeps per-run persistence (and no path derived from raw input).
var ErrNotAPR = fmt.Errorf("pr pointer is not numeric")

// SanitizeRepo maps a repo path to a filesystem-safe fragment.
func SanitizeRepo(repo string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		}
		return '-'
	}, repo)
}

// SanitizePR accepts only digits: PR numbers are numeric everywhere we
// consume them, so anything else ("../evil", empty, junk) structurally
// cannot become a path segment (r20 P4 traversal blocker).
func SanitizePR(pr string) bool {
	if pr == "" || len(pr) > 8 {
		return false
	}
	for _, r := range pr {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// LineageDir is the PR's session directory under root: readable, and
// collision-proof across repos whose sanitized forms would coincide (the
// repo hash disambiguates "a_b/c" from "a/b-c").
func LineageDir(root, repo, pr string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(root, fmt.Sprintf("%s-%s~%s", SanitizeRepo(repo), hex.EncodeToString(sum[:4]), pr))
}

// LineageSessionPath returns a pi-ADOPTABLE name for a fresh session
// file. pi stores sessions as "<ISO-ms-timestamp>_<id>.jsonl" and resolves
// --session-id by decoding the filename prefix — a bare "<id>.jsonl" is
// invisible to it (r21 P4.1, verified against the pinned CLI).
func LineageSessionPath(dir, id string) string {
	name := time.Now().UTC().Format("2006-01-02T15-04-05-000Z") + "_" + id + ".jsonl"
	return filepath.Join(dir, name)
}

// FindLineageSession locates the pi session file for id in dir: pi renames
// an adopted/created file to its timestamped form after the first turn, so
// the newest "<ts>_<id>.jsonl" is the live conversation.
func FindLineageSession(dir, id string) (string, []byte, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*_"+id+".jsonl"))
	if err != nil {
		return "", nil, err
	}
	if len(matches) == 0 {
		return "", nil, os.ErrNotExist
	}
	sort.Strings(matches) // timestamp prefix sorts lexicographically
	file := matches[len(matches)-1]
	b, err := os.ReadFile(file)
	return file, b, err
}

// ResolveSession returns the lineage dir, the PR's stable session id, and
// whether an existing session file will be RESUMED. The id is
// deterministic — pi creates the session on first use and reopens it on
// every later spawn, which is the entire mechanism: same dir + same id =
// same conversation (ADR-0010).
func ResolveSession(root, repo, pr string) (dir, id string, resume bool, err error) {
	if !SanitizePR(pr) {
		return "", "", false, ErrNotAPR
	}
	dir = LineageDir(root, repo, pr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return "", "", false, err
	}
	return dir, "harmostes-" + pr, len(matches) > 0, nil
}

// ActorID builds the PRLineage entity id "<sanitized-repo>~<pr>" (the
// repo hash mirrors LineageDir: sanitizer colliders ("a_b/c" vs "a/b-c")
// must not share one durable entity — r24 P4.2).
func ActorID(repo, pr string) (string, error) {
	if !SanitizePR(pr) {
		return "", ErrNotAPR
	}
	sum := sha256.Sum256([]byte(repo))
	return fmt.Sprintf("%s-%s~%s", SanitizeRepo(repo), hex.EncodeToString(sum[:4]), pr), nil
}

// ValidID mirrors ActorID for untrusted ids arriving off the wire.
func ValidID(id string) bool {
	i := strings.LastIndex(id, "~")
	return i > 0 && SanitizePR(id[i+1:])
}

// Lineage is the actor's persisted state: the pi session JSONL (the whole
// conversation, resumable), the head it was last published against, and a
// monotonic publish counter (the ordered-growth signal — it must only ever
// increase; a regression means state was lost or two writers raced).
type Lineage struct {
	Session    string `json:"session"`
	LastHead   string `json:"lastHead"`
	Generation int    `json:"generation"`
	// File is the pi-side FILENAME of the live conversation (pi renames
	// sessions to "<ts>_<id>.jsonl" after the first turn); fetch must
	// materialize under the SAME name or pi cannot adopt it (r21 P4.1).
	File string `json:"file,omitempty"`
}

// Advance is the server-side publish rule: the generation is
// authoritative from the STORED state (read-modify-write inside the
// actor's turn — the sidecar serializes calls per id, so this is
// race-free by construction). A corrupt stored blob refuses rather than
// silently resetting the counter (the caller maps the refusal to a
// fail-closed publish).
func Advance(stored []byte, ok bool, in Lineage) (Lineage, error) {
	var cur Lineage
	if ok && len(stored) > 0 {
		if err := json.Unmarshal(stored, &cur); err != nil {
			return Lineage{}, fmt.Errorf("corrupt session state: %w", err)
		}
	}
	in.Generation = cur.Generation + 1
	return in, nil
}

// ActorInvoker is the Dapr capability the store needs (satisfied by
// internal/dapr's client; narrowed here so tests can fake it).
type ActorInvoker interface {
	InvokeActor(ctx context.Context, actorType, actorID, method string, payload []byte) ([]byte, error)
}

// ActorType is the registered Dapr entity name.
const ActorType = "PRLineage"

// Store is the durable lineage persistence over the PRLineage actor:
// turn-based (the sidecar serializes per id), durable (state store), and
// isolated (no cross-PR reads are expressible).
type Store struct {
	Actors ActorInvoker
}

// Fetch reads the stored lineage; an absent or unreadable blob is the
// ZERO lineage (fetch is a view — refusing to render stale bytes beats
// failing the read entirely).
func (s Store) Fetch(ctx context.Context, repo, pr string) (Lineage, error) {
	id, err := ActorID(repo, pr)
	if err != nil {
		return Lineage{}, err
	}
	raw, err := s.Actors.InvokeActor(ctx, ActorType, id, "fetch", nil)
	if err != nil {
		return Lineage{}, err
	}
	var l Lineage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &l) // lenient: corrupt serves as empty
	}
	return l, nil
}

// Publish persists in with a server-authoritative generation bump. The
// stored blob's JSON is carried verbatim as the state value (the actor
// handler re-validates monotonicity inside its turn — defense in depth).
func (s Store) Publish(ctx context.Context, repo, pr string, in Lineage) error {
	id, err := ActorID(repo, pr)
	if err != nil {
		return err
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = s.Actors.InvokeActor(ctx, ActorType, id, "publish", b)
	return err
}

// Materialize writes the stored lineage's session under a pi-ADOPTABLE
// name in dir and reports whether the run resumes. Only a basename ending
// in "_"+id+".jsonl" is honored (client-settable File must never become a
// traversal write path — r24 P4.1).
func Materialize(dir, id string, l Lineage) (name string, ok bool, err error) {
	name = filepath.Base(l.File)
	if !strings.HasSuffix(name, "_"+id+".jsonl") {
		name = filepath.Base(LineageSessionPath(dir, id))
	}
	if err = os.WriteFile(filepath.Join(dir, name), []byte(l.Session), 0o600); err != nil {
		return "", false, err
	}
	return name, l.Session != "", nil
}

// LinkComponent is the lineage-dir component the vendored SoL-Pi
// extension joins into its runtime root
// (extensions/sol-pi/src/sol-pi/runtime-paths.ts: join(sessionDir,
// "sol-pi", sessionId)). The linker redirects exactly this component;
// TestSolPiRuntimeComponentSingleSource pins the vendored literal so an
// upstream bump that renames or re-nests it cannot silently de-share
// every lineage while the arm-time log still reports a healthy store
// (#704 review r5 — the same wiring claim as piargs.RigGraphPath).
const LinkComponent = "sol-pi"

// LinkSharedStore points the lineage dir's sol-pi runtime at the shared
// attachment store (#704). The vendored SoL-Pi extension derives its
// runtime root as <lineage>/<LinkComponent>/<session-id>; making the
// component a symlink lands its content-addressed observation and
// reducer objects OUTSIDE the lineage subtree — immune to the TTL that
// garbage-collects a quiet PR's research after 14 days, and deduped by
// content address across the lineage's rounds.
//
// READ SCOPE (r5, deliberate): the store leaf is
// <mount>/<workflow> — raw tool output is NOT redacted on this path
// (the durable-session publisher redacts #115-style before writing; the
// byte-pristine extension cannot be made to), so cross-workflow READS
// stay disabled until an upstream redaction hook lands. Per-workflow
// dedup and TTL immunity are live now; flipping the leaf off is the
// owner's call after the upstream fix.
//
// RETENTION OWNER (r1, explicit decision): the store is deliberately
// NOT garbage-collected by this repo — its durability is the feature,
// and lineage-TTL pruning cannot reach through the symlink. The bound
// is the claim's quota; the fleet accepts growth until it alarms. The
// arm-time byte count is a MONOTONE LOWER BOUND (the walk caps on
// budget, never freezes at a constant), so the growth curve moves until
// exhaustion. A real retention owner (TTL sweep, byte-budget LRU, or a
// dedicated claim) is tracked as the #704 follow-up.
//
// POISON RECOVERY (r2, r4): the extension writes objects with flag wx
// but NOT atomically — a wall-killed attempt can leave a truncated
// object at a content-addressed path, and later dedups throw instead of
// repairing; the lineage TTL used to give that poison a half-life. The
// repair unit is the SINGLE OBJECT (unlink the one mismatched
// obs_<id>.txt / objects/<hash> entry — safe because the payload is in
// hand and content-addressed). It is NOT the session subtree: the
// subtree is shared by every review of the PR (dedup working as
// designed) and per-object is what the upstream atomic-write fix makes
// redundant. Tracked with the follow-up; the fleet accepts the window.
//
// LINK DISCIPLINE (r2, r5): while the store is declared the component
// is never left absent and never replaced by a real directory —
// creation is atomic (symlink to a temp name, rename over the link:
// rename replaces an existing symlink in one step, so a re-point
// destroys nothing and a failed create leaves the old link untouched).
// An outage leaves a DANGLING link: SoL-Pi runs degraded (reducer
// errors surface as extension_error; observation-pack fails open
// silently — the arm-time log names it), no real directory is ever
// born in the gap, and the next healthy arm recovers without
// migration. Removal happens only when the store is UNDECLARED: the
// symlink is our artifact and its removal restores the stock
// per-lineage store; a real directory is pre-#704 legacy data and is
// kept as-is.
func LinkSharedStore(lineageDir, workflow, mount string, logf func(string, ...any)) {
	link := filepath.Join(lineageDir, LinkComponent)
	if mount == "" {
		// Undeclared: undo our own artifact if present. A symlink is the
		// only thing this function ever created at that path — removing
		// it lets the stock per-lineage store apply. A real directory is
		// pre-#704 legacy data: kept, untouched.
		if existing, err := os.Readlink(link); err == nil && existing != "" {
			if err := os.Remove(link); err != nil {
				if logf != nil {
					logf("sol-pi stale link removal failed: %v", err)
				}
				return
			}
			if logf != nil {
				logf("sol-pi: store undeclared — removed the stale link; stock per-lineage runtime applies")
			}
		}
		return
	}
	// The workflow leaf: read scope is per-workflow by default (r5).
	target := filepath.Join(mount, workflow)
	// Existing symlink: idempotent when it already points at the leaf.
	if existing, err := os.Readlink(link); err == nil {
		if existing == target {
			// Liveness before measurement (r4): an outage must name
			// itself with the same wording on every arm shape, not
			// render an ABSENT store as an unmeasurably large one.
			if info, err := os.Stat(target); err != nil || !info.IsDir() {
				if logf != nil {
					logf("sol-pi DEGRADED: store %s not mounted — observation-pack will fail open (context stays unpaged) and the reducer will error, until the mount returns", target)
				}
				return
			}
			logStoreSize(target, logf)
			return
		}
		// Re-point: the atomic rename below replaces the link without a
		// destructive pre-remove.
	} else if st, err := os.Lstat(link); err == nil && st.IsDir() {
		// Legacy real directory (pre-#704 lineage): the PR's accumulated
		// observations stay local; replacing them with a symlink would
		// orphan them. Distinct log wording — this is deliberate
		// protection of old data, not an outage artifact.
		if logf != nil {
			logf("sol-pi: legacy per-lineage store present — not sharing this PR's runtime")
		}
		return
	}
	// Atomic create/re-point: symlink to a temp name, rename over the
	// link. A failed create leaves the old link (or absence) exactly as
	// it was — the extension can never win a gap (r5).
	tmp := link + ".new"
	if err := symlinkStore(target, tmp); err != nil {
		if logf != nil {
			logf("sol-pi store link failed: %v", err)
		}
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		if logf != nil {
			logf("sol-pi store link rename failed: %v", err)
		}
		return
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		if logf != nil {
			logf("sol-pi DEGRADED: store %s not mounted — observation-pack will fail open (context stays unpaged) and the reducer will error, until the mount returns", target)
		}
		return
	}
	logStoreSize(target, logf)
}

// symlinkStore is the creation seam (probe injection, the walkStore
// pattern): os.Symlink is NOT atomic-with-recovery — a failure leaves
// the path absent and the byte-pristine extension would materialize a
// real directory in the gap, so the linker creates through a temp name
// and renames.
var symlinkStore = os.Symlink

// measureStore sizes the shared store with a bounded walk. Per-entry
// errors (an entry removed mid-walk by a concurrent attempt, a CSI blip)
// SKIP that entry — they never abort the scan and never mask the rest —
// but they are carried out as scanErr, because an under-report that
// looks like a small store is exactly how the quota fills unseen
// (#704 review r2). The injectable walker exists for the probe. A
// root-level error is reported as ErrStoreRootMissing — an ABSENT store
// is a different state from a partially-scanned one, and the caller's
// wording must not invert it (r3).
var walkStore = filepath.WalkDir

var ErrStoreRootMissing = errors.New("sol-pi store root missing (not mounted)")

// measureStore sizes the shared store with a budgeted walk. The FILE
// cap stops per-entry Info() (a real lstat each on the RWX claim) but
// the walk CONTINUES summing sizes past it — a monotone lower bound
// ("≥ N bytes") keeps the growth curve moving on a huge store, where a
// frozen constant would read as a store that stopped growing while the
// claim fills (#704 review r5). Per-entry errors skip that entry and
// carry scanErr (an under-report must not read as a small store); a
// root-level error is ErrStoreRootMissing — an ABSENT store is a
// different state from a partially-scanned one. The injectable walker
// exists for the probe.
const (
	measureMaxFiles     = 20000
	measureMaxFileBytes = 4 << 30 // stop summing past 4GiB counted — the quota is 10Gi
)

func measureStore(mount string) (total int64, files int, capped bool, scanErr error) {
	err := walkStore(mount, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// The root itself is unreadable: nothing to measure.
			if path == mount {
				return ErrStoreRootMissing
			}
			scanErr = fmt.Errorf("%s: %w", path, err)
			return nil // keep counting the rest
		}
		if capped {
			// Past the file cap the walk KEEPS SUMMING sizes (monotone
			// lower bound — the curve must move) and stops only at the
			// byte budget.
			if total >= measureMaxFileBytes {
				return fs.SkipAll
			}
			if !d.IsDir() {
				files++
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
			files++
			if files >= measureMaxFiles {
				capped = true
			}
		}
		return nil
	})
	if err != nil && scanErr == nil {
		scanErr = err
	}
	return total, files, capped, scanErr
}

func logStoreSize(mount string, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	total, files, capped, scanErr := measureStore(mount)
	if errors.Is(scanErr, ErrStoreRootMissing) {
		// Mounted-but-unlistable (claim uid/fsGroup mismatch, root_squash):
		// the store EXISTS — "not mounted" would send the operator to
		// remount a healthy PVC, and "the real total is LARGER" inverts the
		// state. Name the actual failure (r4: the sentinel is the contract,
		// not dead code).
		logf("sol-pi shared store UNREADABLE at %s (mounted but not listable — check claim uid/fsGroup)", mount)
		return
	}
	if capped {
		// Monotone lower bound: the curve must keep moving past the scan
		// budget, never freeze at a constant that reads as "stopped
		// growing" (#704 review r5).
		logf("sol-pi shared store: >= %d bytes (capped scan: %d files counted)", total, files)
		return
	}
	marker := ""
	if scanErr != nil {
		marker = fmt.Sprintf(" (scan INCOMPLETE: %v — the real total is LARGER)", scanErr)
	}
	logf("sol-pi shared store: %d bytes across %d files%s", total, files, marker)
}
