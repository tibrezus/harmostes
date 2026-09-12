package worker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A minimal pi-session line with one tool call, in the shape the audit of
// the #2085 review session established (assistant message, content array,
// tool_use blocks with name + input.command).
func usageLine(name, command string) string {
	msg := map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "thinking"},
				map[string]any{"type": "tool_use", "name": name, "input": map[string]any{"command": command}},
			},
		},
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

func TestCountToolUsageCategorizes(t *testing.T) {
	lines := []string{
		usageLine("rig", `{"command":"brief"}`),               // orientation
		usageLine("rig", "overview"),                          // orientation again
		usageLine("bash", "grep -rn pick_victim cuda/"),       // discovery
		usageLine("grep", "pick_victim cuda/"),                // discovery (native tool)
		usageLine("bash", "sed -n '727,830p' cuda/expert.cu"), // reading
		usageLine("bash", "cat /workspace/pr-context.json"),   // fixed
		"not json at all {",                                   // malformed — skipped
		`{"message":{"content":"plain string"}}`,              // no tool_use — nothing
	}
	usage := countToolUsage([]byte(strings.Join(lines, "\n")))
	if usage.ToolCalls != 6 {
		t.Errorf("ToolCalls = %d, want 6", usage.ToolCalls)
	}
	if usage.RigCalls != 2 {
		t.Errorf("RigCalls = %d, want 2", usage.RigCalls)
	}
	if usage.GrepCalls != 2 {
		t.Errorf("GrepCalls = %d, want 2", usage.GrepCalls)
	}
}

func TestCountToolUsageEmptyAndGarbage(t *testing.T) {
	if u := countToolUsage(nil); u != (ToolUsage{}) {
		t.Errorf("nil input = %+v, want zero", u)
	}
	if u := countToolUsage([]byte("\n\n")); u != (ToolUsage{}) {
		t.Errorf("blank lines = %+v, want zero", u)
	}
}

func TestIsGrepCommandWordBoundary(t *testing.T) {
	yes := []string{"grep -rn foo", "rg pick_victim cuda/", "cat x | grep y",
		`git grep -n "todo"`, "/usr/bin/grep -c x"}
	no := []string{"sed -n '1,2p' documenting.md", "cat README", "make test"}
	for _, c := range yes {
		if !isGrepCommand(c) {
			t.Errorf("isGrepCommand(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if isGrepCommand(c) {
			t.Errorf("isGrepCommand(%q) = true, want false", c)
		}
	}
}

// The counter must describe the REAL artifact: run it over the archived
// #2085 audit session when present (dev machines keep it; CI skips).
func TestCountToolUsageAgainstArchived2085Session(t *testing.T) {
	raw, err := os.ReadFile("/tmp/session-3ac2b3d.jsonl")
	if err != nil {
		t.Skip("archived #2085 session not present on this machine")
	}
	usage := countToolUsage(raw)
	if usage.ToolCalls != 37 {
		t.Errorf("ToolCalls = %d, want 37 (the #2085 audit total)", usage.ToolCalls)
	}
	if usage.RigCalls != 1 {
		t.Errorf("RigCalls = %d, want 1 (orientation = 1 was the finding)", usage.RigCalls)
	}
	if usage.GrepCalls < 10 {
		t.Errorf("GrepCalls = %d, want ≥ 10 (the discovery glut was the finding)", usage.GrepCalls)
	}
}
