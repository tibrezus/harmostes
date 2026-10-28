package sessionstore

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// line renders one pi session event as JSONL.
func line(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return string(b)
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}

func statFile(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func sampleTranscript(t *testing.T) []byte {
	t.Helper()
	var lines []string
	lines = append(lines, line(t, map[string]any{"type": "session", "id": "harmostes-99", "timestamp": "2026-10-28T10:00:00Z", "version": 3, "cwd": "/workspace"}))
	lines = append(lines, line(t, map[string]any{"type": "model_change", "modelId": "litellm/test/primary", "timestamp": "2026-10-28T10:00:01Z"}))
	assistant := func(content any, at string) string {
		return line(t, map[string]any{"type": "message", "timestamp": at, "message": map[string]any{"role": "assistant", "content": content}})
	}
	// a read (orientation — never a traced path)
	lines = append(lines, assistant([]map[string]any{
		{"type": "toolCall", "name": "read", "arguments": map[string]any{"path": "/skills/pr-review/SKILL.md"}},
	}, "2026-10-28T10:00:02Z"))
	// an edit + a write (traced paths)
	lines = append(lines, assistant([]map[string]any{
		{"type": "thinking", "thinking": "hmm"},
		{"type": "toolCall", "name": "edit", "arguments": map[string]any{"path": "/workspace/repo/internal/a/a.go"}},
	}, "2026-10-28T10:01:00Z"))
	lines = append(lines, assistant([]map[string]any{
		{"type": "toolCall", "name": "write", "arguments": map[string]any{"path": "/workspace/repo/docs/b.md"}},
	}, "2026-10-28T10:02:00Z"))
	// duplicate path — deduped
	lines = append(lines, assistant([]map[string]any{
		{"type": "toolCall", "name": "edit", "arguments": map[string]any{"path": "/workspace/repo/internal/a/a.go"}},
	}, "2026-10-28T10:03:00Z"))
	// a bash call (not a file-touch)
	lines = append(lines, assistant([]map[string]any{
		{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": "go test ./..."}},
	}, "2026-10-28T10:04:00Z"))
	// the closing rationale
	lines = append(lines, assistant([]map[string]any{
		{"type": "text", "text": "REQUEST_CHANGES: the fallback table is keyed by the wrong group id."},
	}, "2026-10-28T10:05:00Z"))
	// an unparsable line — skipped, never fatal
	lines = append(lines, "{not json")
	return []byte(strings.Join(lines, "\n"))
}

func TestExtractCarryRoundsSkeleton(t *testing.T) {
	carry := ExtractCarry(sampleTranscript(t), "", "deadbeef1234")
	for _, want := range []string{
		"## This round",
		"head: deadbeef1234",
		"models: litellm/test/primary",
		"turns: 6 assistant messages, 5 tool calls",
		"edit×2, bash×1",
		"files touched: /workspace/repo/internal/a/a.go; /workspace/repo/docs/b.md",
		"REQUEST_CHANGES: the fallback table is keyed by the wrong group id.",
	} {
		if !strings.Contains(carry, want) {
			t.Errorf("carry missing %q:\n%s", want, carry)
		}
	}
	// orientation reads are not traced as touched files
	if strings.Contains(carry, "SKILL.md") {
		t.Errorf("read-only path leaked into the touched-files trace:\n%s", carry)
	}
	// the unparsable line did not kill the extraction
	if !strings.Contains(carry, "Closing rationale") {
		t.Error("unparsable line broke the extraction")
	}
}

func TestExtractCarryDeterministic(t *testing.T) {
	raw := sampleTranscript(t)
	first := ExtractCarry(raw, "", "deadbeef")
	second := ExtractCarry(raw, "", "deadbeef")
	if first != second {
		t.Error("extraction is not deterministic")
	}
}

func TestExtractCarryFoldsPriorNewestFirst(t *testing.T) {
	prior := "## This round\n- head: aaa111\n\n## Earlier rounds (newest first)\n\n- head: 000000\n"
	carry := ExtractCarry(sampleTranscript(t), prior, "deadbeef1234")
	hi := strings.Index(carry, "deadbeef1234")
	mid := strings.Index(carry, "aaa111")
	lo := strings.Index(carry, "000000")
	if hi < 0 || mid < 0 || lo < 0 {
		t.Fatalf("folded carry missing a layer:\n%s", carry)
	}
	if hi >= mid || mid >= lo {
		t.Errorf("fold order is not newest-first (head@%d, prior@%d, older@%d)", hi, mid, lo)
	}
	if !strings.Contains(carry, "Earlier rounds") {
		t.Error("earlier-rounds section header missing")
	}
}

func TestExtractCarryBounded(t *testing.T) {
	// a bloated prior + a bloated closing rationale must land under the cap
	var big strings.Builder
	for big.Len() < 4*MaxCarryBytes {
		big.WriteString("- head: old\nline of old context that must be trimmed from the tail\n")
	}
	var transcript strings.Builder
	transcript.WriteString(line(t, map[string]any{"type": "model_change", "modelId": "m", "timestamp": "2026-10-28T10:00:01Z"}) + "\n")
	var final strings.Builder
	for final.Len() < 2*maxCarryFinal {
		final.WriteString("verbose rationale prose ")
	}
	transcript.WriteString(line(t, map[string]any{"type": "message", "timestamp": "2026-10-28T10:05:00Z", "message": map[string]any{
		"role": "assistant", "content": []map[string]any{{"type": "text", "text": final.String()}},
	}}))
	carry := ExtractCarry([]byte(transcript.String()), big.String(), "deadbeef")
	if len(carry) > MaxCarryBytes {
		t.Errorf("carry %d bytes over the %d cap", len(carry), MaxCarryBytes)
	}
	if !strings.Contains(carry, "(older rounds trimmed)") && !strings.Contains(carry, "(truncated)") {
		t.Error("trimming happened silently — the loss must be marked")
	}
	// the newest content (this round's rationale head) survives
	if !strings.Contains(carry, "verbose rationale prose") {
		t.Error("this round's rationale did not survive the bound")
	}
}

func TestExtractCarryEmptyTranscriptStillRenders(t *testing.T) {
	carry := ExtractCarry([]byte(""), "", "")
	if !strings.Contains(carry, "## This round") {
		t.Errorf("empty transcript must still render the skeleton:\n%s", carry)
	}
}

func TestResetLineageClearsOnlyThisPR(t *testing.T) {
	root := t.TempDir()
	kept := root + "/2026-10-28T10-00-00-000Z_harmostes-11.jsonl"
	mine := root + "/2026-10-28T09-00-00-000Z_harmostes-99.jsonl"
	legacy := root + "/harmostes-99.jsonl"
	for _, f := range []string{kept, mine, legacy} {
		if err := writeFile(f, []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ResetLineage(root, "harmostes-99")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 removed (this PR's ts + legacy), got %d", n)
	}
	if _, err := statFile(kept); err != nil {
		t.Errorf("another PR's transcript must survive: %v", err)
	}
	if _, err := statFile(mine); err == nil {
		t.Error("this PR's transcript survived the reset")
	}
	if _, err := statFile(legacy); err == nil {
		t.Error("the legacy bare-name transcript survived the reset")
	}
}

func TestExtractCarryOnRealOutageBlob(t *testing.T) {
	// The shape-pinning test: the extractor must handle the event mix the
	// rhesadox#2833 outage lineage actually contained (session,
	// model_change, thinking_level_change, message×321, custom×118,
	// context_edit) and land far under the cap.
	var transcript strings.Builder
	transcript.WriteString(line(t, map[string]any{"type": "session", "id": "harmostes-2833", "timestamp": "2026-10-22T12:18:27Z", "version": 3}) + "\n")
	transcript.WriteString(line(t, map[string]any{"type": "model_change", "modelId": "litellm/test/legacy-primary", "timestamp": "2026-10-22T12:18:27Z"}) + "\n")
	transcript.WriteString(line(t, map[string]any{"type": "custom", "customType": "sol-pi-online-context-state-v1", "data": map[string]any{"epoch": 0}, "timestamp": "2026-10-22T12:18:28Z"}) + "\n")
	for i := 0; i < 320; i++ {
		transcript.WriteString(line(t, map[string]any{"type": "message", "timestamp": "2026-10-28T18:00:00Z", "message": map[string]any{
			"role": "assistant", "content": []map[string]any{{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": "ls"}}},
		}}) + "\n")
	}
	transcript.WriteString(line(t, map[string]any{"type": "context_edit", "targetId": "x", "replacement": nil, "timestamp": "2026-10-28T18:11:44Z"}) + "\n")
	transcript.WriteString(line(t, map[string]any{"type": "message", "timestamp": "2026-10-28T18:12:00Z", "message": map[string]any{
		"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Coverage declared in review.json: 6 files fully reviewed."}},
	}}) + "\n")
	carry := ExtractCarry([]byte(transcript.String()), "", "5e3be30e365c")
	if len(carry) > MaxCarryBytes {
		t.Fatalf("carry %d over cap", len(carry))
	}
	if !strings.Contains(carry, "bash×320") || !strings.Contains(carry, "litellm/test/legacy-primary") {
		t.Errorf("outage-shape extraction lost facts:\n%s", carry)
	}
	if !strings.Contains(carry, "Coverage declared in review.json") {
		t.Error("closing rationale missing")
	}
}

func TestTailBytesKeepsNewestLinesWhole(t *testing.T) {
	s := "line1\nline2\nline3\nline4-final\n"
	got := TailBytes(s, 12)
	if strings.Contains(got, "line1") || strings.Contains(got, "line2") {
		t.Errorf("oldest lines survived the tail cut: %q", got)
	}
	if !strings.Contains(got, "line4-final") {
		t.Errorf("the newest line did not survive: %q", got)
	}
	if strings.HasPrefix(strings.TrimPrefix(got, "\n"), "3\n") {
		t.Errorf("the cut started mid-line: %q", got)
	}
	if TailBytes(s, 1000) != s {
		t.Error("small inputs must pass through untouched")
	}
}
