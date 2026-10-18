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

// LinkSharedStore points the lineage dir's sol-pi runtime at the
// repository-scoped attachment mount (#704). The vendored SoL-Pi extension
// derives its runtime root as <lineage>/sol-pi/<session-id>; making the
// "sol-pi" component a symlink to the shared store lands its
// content-addressed observation and reducer objects in one store per
// REPO — shared by every workflow targeting that repo, and immune to the
// lineage TTL that garbage-collects a quiet PR's research after 14 days.
// The extension stays byte-pristine (UPSTREAM.md r5): the redirect is
// filesystem-level.
//
// RETENTION OWNER (#704 review r1, explicit decision): the store is
// deliberately NOT garbage-collected by this repo — its durability is the
// feature, and lineage-TTL pruning (the accidental GC) cannot reach
// through the symlink. The bound is the claim's quota (sessions = 10Gi
// by default); the fleet accepts growth until it alarms. The link log
// below emits the store's byte size whenever the mount resolves, so the
// growth curve is visible BEFORE exhaustion, not as an inert extension
// mid-run. A real retention owner (TTL sweep, byte-budget LRU, or a
// dedicated claim) is tracked as the #704 follow-up and must land before
// the store is enabled fleet-wide beyond the two pr-review workflows.
// The retention owner must also own POISON RECOVERY (r2, r4): the
// extension writes objects with flag wx but NOT atomically — an attempt
// killed mid-write (the #695 wall terminates attempts) can leave a
// truncated object at a content-addressed path, and every later dedup
// of that payload throws instead of repairing. Pre-#704 the lineage TTL
// gave that poison a half-life; the shared store keeps it forever. The
// repair unit is the SINGLE OBJECT — unlink the one mismatched
// obs_<id>.txt / objects/<hash> entry. It is NOT the per-session
// subtree: <store>/harmostes-<pr> is keyed by session id only and is
// shared by every workflow that reviews the PR (that is this feature
// working as designed), so deleting the subtree would heal one
// workflow's poison by destroying another's healthy, deduped objects —
// whose receipts in a durable session would then quote archives that
// no longer exist. Per-object is safe precisely because the payload is
// in hand and content-addressed: the rewrite is idempotent. The
// upstream fix (atomic write or repair-on-mismatch) makes the window
// redundant and is tracked with the follow-up; the fleet accepts it
// until then.
//
// Link discipline (#704 review r2): while the store is DECLARED the
// symlink is never removed and never left un-created — `os.Symlink`
// succeeds against a not-yet-present target, so a transient mount
// failure (CSI failover, SubPath re-create, Flux churn on the
// declaration) leaves a DANGLING link, which keeps the lineage a
// shared-store lineage: SoL-Pi's mkdir through the dead component fails
// (mechanisms inert for that attempt — observable as extension_error),
// no real directory is born in the gap, and the next healthy arm
// re-points and recovers automatically. Removing the link instead would
// let the extension's `mkdir -p` materialize a real sol-pi/ directory,
// which the legacy guard below would then honor FOREVER — a transient
// failure permanently de-sharing the lineage. Removal happens only when
// the store is UNDECLARED: a symlink is the only thing this function
// ever created at that path, so removing it restores the stock
// per-lineage store; a REAL directory is pre-#704 legacy data and is
// kept as-is (the log says exactly that).
func LinkSharedStore(lineageDir, mount string, logf func(string, ...any)) {
	const linkName = "sol-pi"
	link := filepath.Join(lineageDir, linkName)
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
	// Declared. Existing symlink: re-point only when the target differs.
	if existing, err := os.Readlink(link); err == nil {
		if existing == mount {
			// Idempotent arm — the STEADY state for every re-review (the
			// lineage persists across Jobs). Liveness before measurement:
			// during an outage the walk's first entry is the missing root,
			// and measuring would log a line asserting the opposite of the
			// state (#704 review r3: "0 bytes … the real total is LARGER"
			// for an ABSENT store). Same wording as the create path, so one
			// grep finds the outage on every arm shape.
			if info, err := os.Stat(mount); err != nil || !info.IsDir() {
				if logf != nil {
					logf("sol-pi DEGRADED: store %s not mounted — observation-pack will fail open (context stays unpaged) and the reducer will error, until the mount returns", mount)
				}
				return
			}
			logStoreSize(mount, logf)
			return
		}
		if err := os.Remove(link); err != nil {
			if logf != nil {
				logf("sol-pi store re-point failed: %v", err)
			}
			return
		}
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
	// Create (or re-create) the link. os.Symlink succeeds against a
	// missing target: during a mount outage the link dangles, no
	// per-lineage directory is manufactured, and SoL-Pi DEGRADES — with
	// per-mechanism asymmetry that matters for observability (r2):
	// evidence-preserving-reducer's archive mkdir escapes its handler and
	// surfaces as extension_error (counted), but observation-pack FAILS
	// OPEN per message (console.error only) — the agent's context simply
	// stops being paged, which the wall budget is most sensitive to. That
	// is why the arm-time log below names the outage explicitly: it is
	// the only signal the primary mechanism will ever raise.
	if err := os.Symlink(mount, link); err != nil {
		if logf != nil {
			logf("sol-pi store link failed: %v", err)
		}
		return
	}
	if info, err := os.Stat(mount); err != nil || !info.IsDir() {
		if logf != nil {
			logf("sol-pi DEGRADED: store %s not mounted — observation-pack will fail open (context stays unpaged) and the reducer will error, until the mount returns", mount)
		}
		return
	}
	logStoreSize(mount, logf)
}

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

func measureStore(mount string) (total int64, files int, truncated bool, scanErr error) {
	const maxFiles = 20000
	err := walkStore(mount, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// The root itself is unreadable: nothing to measure.
			if path == mount {
				return ErrStoreRootMissing
			}
			scanErr = fmt.Errorf("%s: %w", path, err)
			return nil // keep counting the rest
		}
		if files >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
			files++
		}
		return nil
	})
	if err != nil && scanErr == nil {
		scanErr = err
	}
	return total, files, truncated, scanErr
}

func logStoreSize(mount string, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	total, files, truncated, scanErr := measureStore(mount)
	marker := ""
	if truncated {
		marker = " (truncated at 20000 files)"
	}
	if scanErr != nil {
		marker += fmt.Sprintf(" (scan INCOMPLETE: %v — the real total is LARGER)", scanErr)
	}
	logf("sol-pi shared store: %d bytes across %d files%s", total, files, marker)
}
