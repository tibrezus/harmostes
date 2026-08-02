package agent

import (
	"encoding/json"
	"testing"
)

func TestMessageEndUsage(t *testing.T) {
	raw := json.RawMessage(`{"type":"message_end","message":{"role":"assistant","usage":{"input":1234,"output":567,"cacheRead":100,"cacheWrite":50,"cost":{"total":0.002}}}}`)
	u, ok := messageEndUsage(raw)
	if !ok {
		t.Fatal("expected ok=true for assistant message with usage")
	}
	if u.Input != 1234 {
		t.Errorf("input = %d, want 1234", u.Input)
	}
	if u.Output != 567 {
		t.Errorf("output = %d, want 567", u.Output)
	}
	if u.CacheRead != 100 {
		t.Errorf("cacheRead = %d, want 100", u.CacheRead)
	}
	if u.Cost != 0.002 {
		t.Errorf("cost = %f, want 0.002", u.Cost)
	}
}

func TestMessageEndUsage_NonAssistant(t *testing.T) {
	raw := json.RawMessage(`{"type":"message_end","message":{"role":"user","content":"hello"}}`)
	_, ok := messageEndUsage(raw)
	if ok {
		t.Error("expected ok=false for non-assistant message")
	}
}

func TestMessageEndUsage_NoUsage(t *testing.T) {
	raw := json.RawMessage(`{"type":"message_end","message":{"role":"assistant","content":"hi"}}`)
	_, ok := messageEndUsage(raw)
	if ok {
		t.Error("expected ok=false for assistant message with no usage")
	}
}

func TestUsageAccumulation(t *testing.T) {
	var u Usage
	u.add(Usage{Input: 100, Output: 50, Cost: 0.01})
	u.add(Usage{Input: 200, Output: 75, CacheRead: 10, Cost: 0.02})
	if u.Input != 300 {
		t.Errorf("input = %d, want 300", u.Input)
	}
	if u.Output != 125 {
		t.Errorf("output = %d, want 125", u.Output)
	}
	if u.CacheRead != 10 {
		t.Errorf("cacheRead = %d, want 10", u.CacheRead)
	}
	if u.Cost != 0.03 {
		t.Errorf("cost = %f, want 0.03", u.Cost)
	}
	if u.Total() != 335 {
		t.Errorf("total = %d, want 335", u.Total())
	}
}
