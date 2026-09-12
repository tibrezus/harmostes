package worker

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ToolUsage is the per-review rig-usage telemetry (#452 D3): the
// review-graph milestone's acceptance metric — orientation = 1 rig call,
// discovery (grep) ≤ 3 — must be measurable per attempt, not by a
// hand-built kubectl + JSONL archaeology pass (the #2085 audit was
// exactly that). Counted at SavePiSession from the SAME bytes that are
// uploaded, so the numbers describe what the reviewer actually did and
// travel beside the blob they describe.
type ToolUsage struct {
	ToolCalls int `json:"toolCalls,omitempty"` // every tool invocation
	RigCalls  int `json:"rigCalls,omitempty"`  // orientation (rig) calls
	GrepCalls int `json:"grepCalls,omitempty"` // discovery (grep/rg) calls
}

// countToolUsage scans a pi-session JSONL and counts tool invocations:
// total, rig (orientation), and grep-class (discovery — the `grep`/`rg`
// tool or a bash command invoking grep/rg). A grep inside a larger bash
// pipeline counts: its purpose is discovery regardless of what it is
// piped through. Handles both pi session dialects — tool_use/input (the
// provider wire shape) and toolCall/arguments (pi v3's recorded shape,
// what the harmostes UI serves). Malformed lines are skipped (a
// truncated tail line must not zero the counts); a line without any tool
// block contributes nothing.
func countToolUsage(raw []byte) ToolUsage {
	var usage ToolUsage
	for _, line := range bytes.Split(raw, []byte("\n")) {
		// cheap pre-filter: most lines carry no tool call
		if !bytes.Contains(line, []byte(`"tool_use"`)) &&
			!bytes.Contains(line, []byte(`"toolCall"`)) {
			continue
		}
		var msg struct {
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Command string `json:"command"`
					} `json:"input"`
					Arguments struct {
						Command string `json:"command"`
					} `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		for _, c := range msg.Message.Content {
			if c.Type != "tool_use" && c.Type != "toolCall" {
				continue
			}
			cmd := c.Input.Command
			if cmd == "" {
				cmd = c.Arguments.Command
			}
			usage.ToolCalls++
			switch {
			case c.Name == "rig":
				usage.RigCalls++
			case c.Name == "grep" || isGrepCommand(cmd):
				usage.GrepCalls++
			}
		}
	}
	return usage
}

// isGrepCommand reports whether a bash command line invokes grep/rg —
// word-boundary enough: `grep`, `rg`, `grep -rn`, `xargs grep` count;
// `documenting` does not.
func isGrepCommand(cmd string) bool {
	for _, field := range strings.Fields(cmd) {
		base := strings.Trim(field, "'\"();|&")
		if base == "grep" || base == "rg" || base == "/usr/bin/grep" || base == "/usr/bin/rg" {
			return true
		}
	}
	return false
}
