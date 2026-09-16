// Package agent — session lineage identity and dir mechanics.
//
// since #516 the implementation lives in internal/sessionstore (one deep
// module for the durable per-PR lineage — identity, scoping, fetch,
// publish, generation). The symbols here are thin re-exports for the
// in-package callers and tests; new code imports sessionstore directly.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/tibrezus/harmostes/internal/sessionstore"
)

// ErrNotAPR marks a pointer whose PR half is not a plain number.
//
// Deprecated: use sessionstore.ErrNotAPR.
var ErrNotAPR = sessionstore.ErrNotAPR

// SanitizeRepo maps a repo path to a filesystem-safe fragment.
//
// Deprecated: use sessionstore.SanitizeRepo.
func SanitizeRepo(repo string) string { return sessionstore.SanitizeRepo(repo) }

// SanitizePR accepts only digits: PR numbers are numeric everywhere we
// consume them.
//
// Deprecated: use sessionstore.SanitizePR.
func SanitizePR(pr string) bool { return sessionstore.SanitizePR(pr) }

// LineageDir is the PR's session directory under root: readable, and
// collision-proof across repos whose sanitized forms would coincide (the
// repo hash disambiguates "a_b/c" from "a/b-c").
//
// Deprecated: use sessionstore.LineageDir.
func LineageDir(root, repo, pr string) string { return sessionstore.LineageDir(root, repo, pr) }

// LineageSessionPath returns a pi-ADOPTABLE name for a fresh session file.
//
// Deprecated: use sessionstore.LineageSessionPath.
func LineageSessionPath(dir, id string) string { return sessionstore.LineageSessionPath(dir, id) }

// FindLineageSession locates the pi session file for id in dir.
//
// Deprecated: use sessionstore.FindLineageSession.
func FindLineageSession(dir, id string) (string, []byte, error) {
	return sessionstore.FindLineageSession(dir, id)
}

// ResolveSession returns the lineage dir, the PR's stable session id, and
// whether an existing session file will be RESUMED.
//
// Deprecated: use sessionstore.ResolveSession.
func ResolveSession(root, repo, pr string) (dir, id string, resume bool, err error) {
	return sessionstore.ResolveSession(root, repo, pr)
}

// ActorID builds the PRLineage entity id "<sanitized-repo>~<pr>".
//
// Deprecated: use sessionstore.ActorID.
func ActorID(repo, pr string) (string, error) {
	if !SanitizePR(pr) {
		return "", ErrNotAPR
	}
	sum := sha256.Sum256([]byte(repo))
	return fmt.Sprintf("%s-%s~%s", SanitizeRepo(repo), hex.EncodeToString(sum[:4]), pr), nil
}
