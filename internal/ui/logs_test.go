package ui

import (
	"strings"
	"testing"
)

// TestSplitLinesDesc pins the log drawer's ordering contract (#551): the
// drawer renders NEWEST FIRST — the last line of the raw pod log (the most
// recent) must render first, and new lines arrive at the top where the
// reader already is.
func TestSplitLinesDesc(t *testing.T) {
	got := splitLinesDesc("first\nsecond\nthird")
	want := []string{"third", "second", "first"}
	if len(got) != len(want) {
		t.Fatalf("splitLinesDesc = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitLinesDesc = %v, want %v", got, want)
		}
	}
	if got := splitLinesDesc(""); got != nil {
		t.Errorf("splitLinesDesc(\"\") = %v, want nil", got)
	}
	if got := splitLinesDesc("solo"); len(got) != 1 || got[0] != "solo" {
		t.Errorf("splitLinesDesc(solo) = %v, want [solo]", got)
	}
}

// TestRunLogLinesFragmentNewestFirst executes the drawer's line fragment
// through the REAL template set: the first rendered .log-line must be the
// newest source line. This is the ordering the operator reads — a silent
// flip back to oldest-first would bury the relevant output again.
func TestRunLogLinesFragmentNewestFirst(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	data := runLogsData{
		AttemptName: "att",
		RunName:     "run-1",
		RunPhase:    "succeeded",
		Logs:        "2026-09-20T07:00:01Z OLD line\n2026-09-20T07:00:02Z middle line\n2026-09-20T07:00:03Z NEWEST line",
	}
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "pages/frag_run_logs_lines.html", data); err != nil {
		t.Fatalf("execute lines fragment: %v", err)
	}
	out := b.String()
	first := strings.Index(out, `class="log-line`)
	if first < 0 {
		t.Fatalf("no log lines rendered:\n%s", out)
	}
	rest := out[first:]
	head := rest
	if len(head) > 200 {
		head = head[:200]
	}
	if !strings.Contains(head, "NEWEST line") {
		t.Errorf("first rendered line must be the NEWEST one (newest-first drawer), got:\n%s", head)
	}
	if strings.LastIndex(out, "OLD line") < strings.LastIndex(out, "NEWEST line") {
		t.Errorf("oldest line must render last, fragment:\n%s", out)
	}
	// Terminal run: the self-replacing poll must NOT be armed.
	if strings.Contains(out, "hx-trigger") {
		t.Errorf("terminal run must not arm the 3s poll, fragment:\n%s", out)
	}
	// Running + live pod: poll armed on the element itself (self-replacing).
	data.RunPhase = "running"
	data.PodName = "run-1"
	data.PodGone = false
	b.Reset()
	if err := tmpl.ExecuteTemplate(&b, "pages/frag_run_logs_lines.html", data); err != nil {
		t.Fatalf("execute lines fragment (running): %v", err)
	}
	out = b.String()
	if !strings.Contains(out, `hx-trigger="every 3s"`) || !strings.Contains(out, `part=lines`) {
		t.Errorf("running run must arm the every-3s self-poll (?part=lines), fragment:\n%s", out)
	}
	if !strings.Contains(out, `hx-swap="outerHTML"`) {
		t.Errorf("poll must replace the element itself (outerHTML) so a dead pod stops the churn, fragment:\n%s", out)
	}
	// Pod gone: poll disarms even though the run is still "running".
	data.PodName = ""
	b.Reset()
	if err := tmpl.ExecuteTemplate(&b, "pages/frag_run_logs_lines.html", data); err != nil {
		t.Fatalf("execute lines fragment (pod gone): %v", err)
	}
	if strings.Contains(b.String(), "hx-trigger") {
		t.Errorf("pod-gone poll response must not re-arm the 3s churn:\n%s", b.String())
	}
}
