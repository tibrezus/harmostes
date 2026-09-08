package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session lineages (ADR-0010): one pi session per PR, resumed — never
// rebuilt. The store is keyed by PR identity, so the association needs no
// separate database: the directory IS the lineage, and the stable session
// id makes pi reopen the conversation on every later spawn. Non-PR
// workflows (fork-maintenance, deterministic pipelines) keep the per-run
// session dirs (#243) — they have no conversation worth resuming.

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

// ErrNotAPR marks a pointer whose PR half is not a plain number — the
// run keeps per-run persistence (and no path derived from raw input).
var ErrNotAPR = fmt.Errorf("pr pointer is not numeric")

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

// ResolveSession returns the lineage dir, the PR's stable session id, and
// whether an existing session file will be RESUMED. The id is
// deterministic — pi creates the session on first use and reopens it on
// every later spawn, which is the entire mechanism: same dir + same id =
// same conversation (ADR-0010).
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
