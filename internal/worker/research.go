package worker

import (
	"encoding/json"
	"time"
)

// The research journal (#494, SoL-Pi principles C3/C4/C19): a per-workflow,
// append-only record of run outcomes that future sessions of the SAME
// workflow read at start — known outcomes, failed approaches, the model
// that ran. Deterministic by construction (it is the run's own record, not
// an LLM summary), capped on both entries and bytes so injection can never
// overfill context. Deeper LLM-distilled findings are a later tier; this
// tier costs nothing and cannot hallucinate.

const (
	// ResearchJournalEntries caps the journal's tail.
	ResearchJournalEntries = 20
	// ResearchJournalMaxBytes caps the stored journal.
	ResearchJournalMaxBytes = 4096
	// ResearchInjectMaxBytes caps what a new session receives at start —
	// the tail, never the archive (handles over payloads).
	ResearchInjectMaxBytes = 1536
	// researchNoteCap bounds one entry's note.
	researchNoteCap = 160
)

// ResearchEntry is one run's outcome line in the journal.
type ResearchEntry struct {
	At     string `json:"at"`              // RFC3339 UTC
	Run    string `json:"run"`             // run name (attempt-scoped)
	Status string `json:"status"`          // graph terminal status
	Model  string `json:"model,omitempty"` // the model this run pinned
	Note   string `json:"note,omitempty"`  // short terminal message
}

// AppendResearchJournal appends e to prev, trims to the newest maxEntries
// and maxBytes (dropping OLDEST first — the story keeps its ending), and
// returns the stored form. Unparseable prev degrades to a fresh journal
// (never an error path for the run).
func AppendResearchJournal(prev []byte, e ResearchEntry, maxEntries, maxBytes int) []byte {
	if e.Note != "" && len(e.Note) > researchNoteCap {
		e.Note = e.Note[:researchNoteCap]
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	var entries []ResearchEntry
	if len(prev) > 0 {
		_ = json.Unmarshal(prev, &entries) // corrupt journal = fresh start
	}
	entries = append(entries, e)
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return prev
	}
	for len(out) > maxBytes && len(entries) > 1 {
		entries = entries[len(entries)-1:] // drop oldest until it fits
		if out, err = json.Marshal(entries); err != nil {
			return prev
		}
	}
	if len(out) > maxBytes {
		return prev // a single oversized entry cannot evict itself
	}
	return out
}

// RenderResearchJournal renders the journal for injection into a new
// session: newest-first one-liners, capped at maxBytes. Empty when there
// is nothing to say.
func RenderResearchJournal(prev []byte, maxBytes int) string {
	var entries []ResearchEntry
	if len(prev) == 0 || json.Unmarshal(prev, &entries) != nil {
		return ""
	}
	if len(entries) == 0 {
		return ""
	}
	out := ""
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		line := "- " + e.At + " " + e.Status
		if e.Model != "" {
			line += " [" + e.Model + "]"
		}
		if e.Note != "" {
			line += ": " + e.Note
		}
		line += "\n"
		if len(out)+len(line) > maxBytes {
			break
		}
		out += line
	}
	return out
}
