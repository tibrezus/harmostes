package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEvent(t *testing.T) {
	cases := []struct {
		line   string
		wantOK bool
		want   Event
	}{
		{`{"type":"agent_end"}`, true, Event{Type: "agent_end"}},
		{`{"type":"tool_execution_start","toolName":"bash","args":{"command":"ls"}}`, true, Event{Type: "tool_execution_start", ToolName: "bash"}},
		{`{"type":"response","success":false}`, true, Event{Type: "response"}},
		{`   `, false, Event{}},           // blank
		{`not json`, false, Event{}},      // unparseable
		{`{"foo":"bar"}`, false, Event{}}, // no type
	}
	for i, c := range cases {
		ev, err := parseEvent([]byte(c.line))
		if c.wantOK && err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
			continue
		}
		if !c.wantOK && err == nil {
			t.Errorf("case %d: expected error, got %+v", i, ev)
			continue
		}
		if c.wantOK && ev.Type != c.want.Type {
			t.Errorf("case %d: type = %q want %q", i, ev.Type, c.want.Type)
		}
	}
}

// writeFakePi writes a script that pretends to be pi --mode rpc: for each prompt
// line it reads on stdin, it emits `promptCount` tool_execution_start events then
// an agent_end. This lets us exercise the real subprocess wiring without a model.
func writeFakePi(t *testing.T, promptCount int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakepi")
	// The fake pi echoes one prompt's worth of events per stdin line it sees.
	script := strings.Join([]string{
		"#!/usr/bin/env bash",
		"# fake pi: for every line read on stdin, emit events then agent_end",
		"while IFS= read -r line; do",
		"  case \"$line\" in",
		"    *'\"type\":\"abort\"'*) exit 0 ;;",
		"  esac",
		"  for i in $(seq 1 " + itoa(promptCount) + "); do",
		"    echo '{\"type\":\"tool_execution_start\",\"toolName\":\"bash\",\"args\":{\"i\":'\"$i\"'}}'",
		"  done",
		"  echo '{\"type\":\"agent_end\"}'",
		"done",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%10]}, b...)
		n /= 10
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestRPCEndToEnd spins up the fake pi, runs two prompts on one RPC (warm
// session), and asserts each turn reports the scripted tool count and completes
// at agent_end.
func TestRPCEndToEnd(t *testing.T) {
	fakePi := writeFakePi(t, 3) // 3 tool calls per prompt
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	var seen []Event
	logger := func(ev Event) {
		seen = append(seen, ev)
		t.Logf("event: type=%q tool=%q msg=%q", ev.Type, ev.ToolName, truncate(ev.Message, 60))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rpc, err := NewRPC(ctx, RPCOptions{PiPath: fakePi, Workdir: ".", Log: logger})
	if err != nil {
		t.Fatalf("NewRPC: %v", err)
	}
	defer rpc.Abort(context.Background())

	// turn 1
	ev, tools, err := rpc.Prompt(ctx, "do the task", "initial task")
	if err != nil {
		t.Fatalf("prompt 1: %v", err)
	}
	if ev.Type != "agent_end" {
		t.Fatalf("prompt 1: expected agent_end, got %q", ev.Type)
	}
	if tools != 3 {
		t.Fatalf("prompt 1: expected 3 tool calls, got %d", tools)
	}
	// turn 2 — same process (warm session)
	ev, tools, err = rpc.Prompt(ctx, "now fix it", "feedback #1")
	if err != nil {
		t.Fatalf("prompt 2: %v", err)
	}
	if tools != 3 {
		t.Fatalf("prompt 2: expected 3 tool calls, got %d", tools)
	}
	// logger saw both turns' tool calls + both agent_ends
	toolStarts := 0
	agentEnds := 0
	for _, e := range seen {
		switch e.Type {
		case "tool_execution_start":
			toolStarts++
		case "agent_end":
			agentEnds++
		}
	}
	if toolStarts != 6 {
		t.Fatalf("expected 6 tool_execution_start events across 2 turns, got %d", toolStarts)
	}
	if agentEnds != 2 {
		t.Fatalf("expected 2 agent_end events, got %d", agentEnds)
	}
}
