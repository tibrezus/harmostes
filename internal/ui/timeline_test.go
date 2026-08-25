package ui

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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

// The central merge: run events + gate events from multiple attempts,
// newest-first, capped at 200 — through a fake Reader (this is the test
// whose absence let the GateEvents gap through).
func TestLoadTimelineMergesRunsGateAndOrders(t *testing.T) {
	s := newAttemptTestServer(t,
		&v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: "attempt-new", Namespace: "test-ns",
				Labels: map[string]string{v1alpha1.OwnerLabel: "alice", "harmostes.dev/workflow": "wf"},
			},
		},
		&v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: "attempt-old", Namespace: "test-ns",
				Labels: map[string]string{v1alpha1.OwnerLabel: "alice", "harmostes.dev/workflow": "wf"},
			},
		},
		&v1alpha1.Attempt{
			ObjectMeta: metav1.ObjectMeta{
				Name: "attempt-other", Namespace: "test-ns",
				Labels: map[string]string{v1alpha1.OwnerLabel: "alice", "harmostes.dev/workflow": "other"},
			},
		},
	)
	base := time.Now()
	s.timelineReader = &fakeTimelineReader{
		attempts: map[string][]timeline.Event{
			"attempt-new": {mkEvent(base.Add(-1*time.Minute), timeline.KindRunStarted, "", nil)},
			"attempt-old": {mkEvent(base.Add(-2*time.Hour), timeline.KindNodeCompleted, "prepare", map[string]any{"status": "green"})},
		},
		gates: map[string][]timeline.Event{
			"attempt-new": {mkEvent(base.Add(-3*time.Minute), timeline.KindGateArmed, "", nil)},
		},
	}

	rows := s.loadTimeline(t.Context(), "alice", "wf")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (gate + run from new, node from old)", len(rows))
	}
	if rows[0].Kind != timeline.KindRunStarted {
		t.Fatalf("newest first: rows[0] = %s", rows[0].Kind)
	}
	if rows[1].Kind != timeline.KindGateArmed || rows[1].Attempt != "attempt-new" {
		t.Fatalf("gate merge: rows[1] = %+v", rows[1])
	}
	if rows[2].Attempt != "attempt-old" {
		t.Fatalf("multi-attempt merge: rows[2] = %+v", rows[2])
	}
	if rows[0].Ref != "github.com/tibrezus/harmostes#228" || rows[0].Title != "T" {
		t.Fatalf("subject orientation missing: %+v", rows[0])
	}
}

func TestLoadTimelineNoWorkflowIsFree(t *testing.T) {
	s := newAttemptTestServer(t)
	s.timelineReader = &fakeTimelineReader{
		attempts: map[string][]timeline.Event{},
		gates:    map[string][]timeline.Event{},
	}
	if rows := s.loadTimeline(t.Context(), "alice", ""); rows != nil {
		t.Fatalf("no-workflow load must return nil without probing, got %d rows", len(rows))
	}
	if rows := s.loadTimeline(t.Context(), "alice", "wf"); rows != nil {
		t.Fatalf("no attempts → nil, got %d rows", len(rows))
	}
}
