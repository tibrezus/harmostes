package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

// The research journal (#494) is the cross-session sharing tier: append-only,
// capped on entries AND bytes (oldest dropped first — the story keeps its
// ending), degrading to a fresh journal on corruption. Injection renders
// newest-first and never exceeds the cap.
func TestAppendResearchJournal(t *testing.T) {
	e := func(at, status, model, note string) ResearchEntry {
		return ResearchEntry{At: at, Run: "run-" + at, Status: status, Model: model, Note: note}
	}

	// Append + newest-first rendering.
	j := AppendResearchJournal(nil, e("2026-09-14T10:00:00Z", "green", "m1", "all gates green"), 20, 4096)
	j = AppendResearchJournal(j, e("2026-09-14T11:00:00Z", "failed", "m1", "gate: lint red"), 20, 4096)
	rendered := RenderResearchJournal(j, 4096)
	if !strings.Contains(rendered, "gate: lint red") || !strings.Contains(rendered, "11:00") {
		t.Errorf("rendered journal missing the newest entry: %q", rendered)
	}
	if strings.Index(rendered, "11:00") > strings.Index(rendered, "10:00") {
		t.Errorf("render is not newest-first: %q", rendered)
	}

	// Entry cap: oldest dropped first.
	j = nil
	for i := 0; i < 25; i++ {
		j = AppendResearchJournal(j, e(string(rune('a'+i))+"-at", "green", "", ""), 20, 1<<20)
	}
	var entries []ResearchEntry
	if err := json.Unmarshal(j, &entries); err != nil || len(entries) != 20 {
		t.Fatalf("entries = %d (err %v), want 20", len(entries), err)
	}
	if entries[0].Run != "run-f-at" { // 'a'+21 = 'v' dropped... first survivor is the 6th (index 5 → 'f')
		t.Errorf("oldest not dropped: first survivor %q, want run-f-at", entries[0].Run)
	}

	// Byte cap: a long note cannot blow the budget.
	big := strings.Repeat("x", 5000)
	j2 := AppendResearchJournal(nil, e("2026-09-14T12:00:00Z", "green", "", big), 20, 4096)
	if len(j2) > 4096 {
		t.Errorf("journal = %d bytes, cap 4096", len(j2))
	}

	// Corrupt prev degrades to fresh.
	if got := AppendResearchJournal([]byte("{not json"), e("t", "green", "", ""), 20, 4096); !strings.Contains(string(got), `"status":"green"`) {
		t.Errorf("corrupt prev did not degrade to fresh: %q", got)
	}
}

func TestRenderResearchJournal_Caps(t *testing.T) {
	var j []byte
	for i := 0; i < 20; i++ {
		j = AppendResearchJournal(j, ResearchEntry{At: "t", Run: "r", Status: "green", Note: strings.Repeat("n", 200)}, 20, 1<<20)
	}
	r := RenderResearchJournal(j, 600)
	if r == "" || len(r) > 600 {
		t.Fatalf("rendered %d bytes, want ≤600 and non-empty", len(r))
	}
	if !strings.Contains(r, "- t green: n") {
		t.Errorf("rendered tail wrong: %q", r)
	}
	// Newest-first: the LAST entry's position must precede older ones.
	if strings.Index(r, "- t green") != 0 {
		t.Errorf("newest entry not first: %q", r[:80])
	}
}
