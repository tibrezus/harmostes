package agent

import (
	"context"
	"testing"
)

// discardCloser stands in for the subprocess stdin pipe on a hand-built RPC.
type discardCloser struct{}

func (discardCloser) Write(p []byte) (int, error) { return len(p), nil }
func (discardCloser) Close() error                { return nil }

// The live incident (#239): in the spawn shape harmostes drives, pi's RPC
// stream omits message boundary events — agent_end.messages is the only
// carrier of usage and final text. These tests pin both protocol shapes and
// the warm-session delta across prompts on one RPC.

func feedRPC(t *testing.T, lines ...string) *RPC {
	t.Helper()
	r := &RPC{stdin: discardCloser{}, events: make(chan Event, len(lines)+2), done: make(chan struct{})}
	for _, l := range lines {
		ev, err := parseEvent([]byte(l))
		if err != nil {
			t.Fatalf("parse %s: %v", l, err)
		}
		r.events <- ev
	}
	return r
}

const agentEndTurn1 = `{"type":"agent_end","willRetry":false,"messages":[
 {"role":"user","content":[{"type":"text","text":"do the thing"}]},
 {"role":"assistant","usage":{"input":455,"output":42,"cost":{"total":0.01}},"content":[{"type":"toolCall","name":"bash"}]},
 {"role":"toolResult","content":[{"type":"text","text":"hello\n"}]},
 {"role":"assistant","usage":{"input":491,"output":1,"cost":{"total":0.003}},"content":[{"type":"text","text":"done"}]}
]}`

const toolStart = `{"type":"tool_execution_start","toolName":"bash","args":{"command":"echo hello"}}`
const toolEnd = `{"type":"tool_execution_end","toolName":"bash","success":true}`
const update = `{"type":"message_update","usage":{"input":0,"output":0},"assistantMessageEvent":{"type":"text_delta","delta":"d"}}`

// Exec-driven shape: no message_end anywhere; everything must come from the
// agent_end snapshot.
func TestPromptUsageFromAgentEndSnapshot(t *testing.T) {
	r := feedRPC(t, update, toolStart, toolEnd, update, agentEndTurn1)
	ev, tools, usage, capture, err := r.Prompt(context.Background(), "do the thing", "initial task")
	if err != nil || ev.Type != "agent_end" {
		t.Fatalf("prompt: err=%v ev=%s", err, ev.Type)
	}
	if tools != 1 {
		t.Errorf("tools = %d, want 1 (tool events still captured)", tools)
	}
	if usage.Input != 946 || usage.Output != 43 {
		t.Errorf("usage = %d in / %d out, want 946/43 (sum of both assistant snapshots)", usage.Input, usage.Output)
	}
	if usage.Cost < 0.0129 || usage.Cost > 0.0131 {
		t.Errorf("cost = %v, want ~0.013", usage.Cost)
	}
	if capture.Response != "done" {
		t.Errorf("response = %q, want %q (last assistant text)", capture.Response, "done")
	}
}

// Shell-driven shape: message_end already supplied usage + response; the
// snapshot must not double-count.
func TestPromptNoDoubleCountWhenMessageEndPresent(t *testing.T) {
	me := `{"type":"message_end","message":{"role":"assistant","usage":{"input":100,"output":5,"cost":{"total":0.002}},"content":[{"type":"text","text":"hi"}]}}`
	r := feedRPC(t, me, agentEndTurn1)
	_, _, usage, capture, err := r.Prompt(context.Background(), "m", "l")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Input != 100 || usage.Output != 5 {
		t.Errorf("usage = %d/%d, want 100/5 (message_end wins, no double count)", usage.Input, usage.Output)
	}
	if capture.Response != "hi" {
		t.Errorf("response = %q, want hi", capture.Response)
	}
}

// Warm session: the second prompt's agent_end re-sends the whole
// conversation; only the delta belongs to turn 2.
func TestWarmSessionUsageDelta(t *testing.T) {
	r := feedRPC(t, agentEndTurn1)
	if _, _, usage, _, err := r.Prompt(context.Background(), "do the thing", "initial task"); err != nil {
		t.Fatal(err)
	} else if usage.Input != 946 {
		t.Fatalf("turn1 usage = %d, want 946", usage.Input)
	}
	agentEndTurn2 := `{"type":"agent_end","willRetry":false,"messages":[
 {"role":"user","content":[{"type":"text","text":"do the thing"}]},
 {"role":"assistant","usage":{"input":455,"output":42,"cost":{"total":0.01}},"content":[]},
 {"role":"toolResult","content":[]},
 {"role":"assistant","usage":{"input":491,"output":1,"cost":{"total":0.003}},"content":[]},
 {"role":"user","content":[{"type":"text","text":"again"}]},
 {"role":"assistant","usage":{"input":600,"output":2,"cost":{"total":0.004}},"content":[{"type":"text","text":"ok2"}]}
]}`
	r.events <- mustParse(t, agentEndTurn2)
	_, _, usage, capture, err := r.Prompt(context.Background(), "again", "feedback #1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Input != 600 || usage.Output != 2 {
		t.Errorf("turn2 usage = %d/%d, want 600/2 (delta only)", usage.Input, usage.Output)
	}
	if capture.Response != "ok2" {
		t.Errorf("turn2 response = %q, want ok2", capture.Response)
	}
}

func mustParse(t *testing.T, line string) Event {
	t.Helper()
	ev, err := parseEvent([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// Shrinking snapshot (context compaction): the watermark resets so the
// post-compaction messages are all fresh accounting.
func TestWarmSessionCompactionResetsWatermark(t *testing.T) {
	r := feedRPC(t, agentEndTurn1) // 4 messages
	if _, _, usage, _, err := r.Prompt(context.Background(), "do the thing", "initial task"); err != nil {
		t.Fatal(err)
	} else if usage.Input != 946 {
		t.Fatalf("turn1 usage = %d, want 946", usage.Input)
	}
	// Compacted: conversation replaced by a 1-message summary + this turn's
	// exchange — shorter than the old watermark.
	agentEndCompacted := `{"type":"agent_end","willRetry":false,"messages":[
 {"role":"user","content":[{"type":"text","text":"(compacted) again"}]},
 {"role":"assistant","usage":{"input":50,"output":3,"cost":{"total":0.001}},"content":[{"type":"text","text":"ok3"}]}
]}`
	r.events <- mustParse(t, agentEndCompacted)
	_, _, usage, capture, err := r.Prompt(context.Background(), "again", "feedback #1")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Input != 50 || usage.Output != 3 {
		t.Errorf("post-compaction usage = %d/%d, want 50/3 (watermark reset)", usage.Input, usage.Output)
	}
	if capture.Response != "ok3" {
		t.Errorf("post-compaction response = %q, want ok3", capture.Response)
	}
}
