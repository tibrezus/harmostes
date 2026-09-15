package worker

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The sessions janitor (ADR-0010 follow-up): lineage dirs idle longer
// than the workflow's TTL are removed at attempt start. Idle means the
// NEWEST file in the dir predates the cutoff — a resumed conversation
// touches its session file (pi rewrites it after every turn), so active
// PRs are structurally never pruned; closed and merged PRs' lineages
// expire by age. The janitor never fails a run: pruning is best-effort,
// and a failed RemoveAll just leaves the dir for the next attempt.

// JanitorSessions removes lineage dirs under root whose newest file is
// older than ttl (relative to now) and returns how many were removed.
func JanitorSessions(root string, ttl time.Duration, now time.Time) int {
	if ttl <= 0 {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if newestFileMtime(dir).After(cutoff) {
			continue
		}
		if os.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed
}

// newestFileMtime walks dir (lineage dirs hold session jsonl files and
// the sol-pi/ subtree — dozens of small files at most) and returns the
// newest file mtime. An empty dir returns the zero time: an empty
// lineage dir is gone in every sense that matters, so it prunes first.
func newestFileMtime(dir string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return newest
}
