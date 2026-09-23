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
	"fmt"
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
