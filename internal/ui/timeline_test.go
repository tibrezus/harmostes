package ui

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/timeline"
)

// fakeTimelineReader drives loadTimeline without Dapr.
type fakeTimelineReader struct {
	attempts map[string][]timeline.Event // attempt → run events
	gates    map[string][]timeline.Event // attempt → gate events
	subjects map[string]timeline.Subject
}

func (f *fakeTimelineReader) Attempt(_ context.Context, attempt string, _ []string, _ timeline.Filter) ([]timeline.Event, error) {
	return f.attempts[attempt], nil
}

func (f *fakeTimelineReader) GateEvents(_ context.Context, attempt string, _ timeline.Filter) ([]timeline.Event, error) {
	return f.gates[attempt], nil
}

func (f *fakeTimelineReader) Subjects(_ context.Context, _ []string) (map[string]timeline.Subject, error) {
	return f.subjects, nil
}

var _ timeline.Reader = (*fakeTimelineReader)(nil)

func mkEvent(at time.Time, kind, node string, payload any) timeline.Event {
	p, _ := json.Marshal(payload)
	return timeline.Event{At: at, Kind: kind, Node: node, Payload: p,
		Subject: timeline.Subject{Kind: "pr", Ref: "github.com/tibrezus/harmostes#228", Title: "T"}}
}

func TestEventSummaryKindSwitches(t *testing.T) {
	cases := map[string]struct {
		kind    string
		payload any
		want    string
	}{
		"node feedback first line": {timeline.KindNodeCompleted, map[string]any{"feedback": "line one\nline two"}, "line one"},
		"node status fallback":     {timeline.KindNodeCompleted, map[string]any{"status": "green"}, "green"},
		"run status":               {timeline.KindRunCompleted, map[string]any{"status": "ok"}, "ok"},
		"gate reason":              {timeline.KindGateWaiting, map[string]any{"reason": "ci red at head"}, "ci red at head"},
		"agent turn label":         {timeline.KindAgentTurn, map[string]any{"label": "initial task"}, "initial task"},
		"agent tool name":          {timeline.KindAgentTool, map[string]any{"tool": "bash"}, "bash"},
		"no payload":               {timeline.KindRunStarted, nil, ""},
	}
	for name, tc := range cases {
		ev := mkEvent(time.Now(), tc.kind, "", tc.payload)
		if got := eventSummary(ev); got != tc.want {
			t.Errorf("%s: eventSummary = %q, want %q", name, got, tc.want)
		}
	}
}

func TestEventTailExtractsPluginLines(t *testing.T) {
	ev := mkEvent(time.Now(), timeline.KindPluginTail, "prepare", map[string]any{
		"plugin": "workspace", "tail": []string{"a", "b"},
	})
	tail := eventTail(ev)
	if len(tail) != 2 || tail[0] != "a" {
		t.Fatalf("eventTail = %v", tail)
	}
	if eventTail(mkEvent(time.Now(), timeline.KindRunStarted, "", nil)) != nil {
		t.Fatal("non-tail kinds must return nil")
	}
}
