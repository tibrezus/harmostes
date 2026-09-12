package fixture

// timeline.go — the fixture's fake timeline.Reader. The fixture world has no
// Dapr sidecar, so the Event Timeline's store half is served from memory:
// one seeded narrative per fixture attempt, covering EVERY store row type
// the projection renders (ADR-0012 §4 acceptance: trigger → dispatch → node
// events → gate feedback → claim transitions → terminal). Rows the ledger
// supplies (trigger, claim arm/dispatch, TTL-expired run boundaries) come
// from the Attempt CRs — not from here — so the fixture exercises the real
// merge logic end to end.
//
// Timestamps use the shared `base` clock with offsets that interleave with
// the Attempt CRs' RunRecords, so the merged story reads coherently.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tibrezus/harmostes/internal/timeline"
	"github.com/tibrezus/harmostes/internal/ui"
)

// FixTimeline is the fake reader over one seeded narrative — plus the
// fixture's ingest seam: in production the timeline store is written by the
// graph executor through Dapr; here the store GROWS from the same cloud
// events the /dapr/events ingress receives (see daprStoreIngest), so the
// E2E tier can drive real SSE convergence: inject → store row → hub wake →
// fragment re-render — the production event path end to end.
type FixTimeline struct {
	mu     sync.Mutex
	events []timeline.Event
	extra  []timeline.Event
	// seededMax and lastIngest drive the append clock: the seeded world
	// spans ~14h from the process-start hour, so wall-clock now can sit
	// INSIDE it — ingested rows must land after every seeded row, not at
	// now.
	seededMax  time.Time
	lastIngest time.Time
}

// storeKinds maps lifecycle event names onto the store row kinds they
// become. Only node-level events map: a run-level CE carries no node id, and
// the fixture derives the run name from the attempt name + node
// (<workflow>-<suffix>-<node>), mirroring how attempts name their runs.
var storeKinds = map[string]string{
	"node.started":   timeline.KindNodeStarted,
	"node.completed": timeline.KindNodeCompleted,
}

// Ingest appends a store row for a lifecycle event and reports whether one
// was written. Unmappable or foreign events are ignored — the fake store
// only knows its own world.
func (f *FixTimeline) Ingest(ev ui.Event) bool {
	kind, ok := storeKinds[ev.Event]
	if !ok || ev.Attempt == "" || ev.Node == "" || !strings.HasPrefix(ev.Attempt, "attempt-") {
		return false
	}
	payload := map[string]any{}
	if ev.NodeType != "" {
		payload["type"] = ev.NodeType
	}
	if ev.Status != "" {
		payload["status"] = ev.Status
	}
	if ev.DurationMs > 0 {
		payload["durationMs"] = ev.DurationMs
	}
	if ev.Feedback != "" {
		payload["feedback"] = ev.Feedback
	}
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte("{}")
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = f.appendAt()
	}
	row := timeline.Event{
		At: at, Attempt: ev.Attempt,
		Run:  strings.TrimPrefix(ev.Attempt, "attempt-") + "-" + ev.Node,
		Node: ev.Node, Kind: kind, Payload: b,
		Subject: timeline.Subject{Kind: "pr", Ref: "demo-rezuscloud/harmostes#42", Title: "Fixture narrative"},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extra = append(f.extra, row)
	if at.After(f.lastIngest) {
		f.lastIngest = at
	}
	return true
}

// appendAt returns the next ingest timestamp: now, but never at or before
// the last row the store holds (seeded or ingested) — the story appends.
func (f *FixTimeline) appendAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	at := time.Now().UTC()
	if floor := f.seededMax.Add(time.Second); at.Before(floor) {
		at = floor
	}
	if at.Before(f.lastIngest) {
		at = f.lastIngest
	}
	return at
}

// snapshot merges the seeded narrative with ingested rows (oldest first).
func (f *FixTimeline) snapshot() []timeline.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]timeline.Event, 0, len(f.events)+len(f.extra))
	out = append(out, f.events...)
	out = append(out, f.extra...)
	return out
}

// NewTimelineReader returns the fixture's seeded store.
func NewTimelineReader() *FixTimeline {
	ev := func(at time.Time, attempt, run, node, kind string, payload map[string]any) timeline.Event {
		b, err := json.Marshal(payload)
		if err != nil {
			b = []byte("{}")
		}
		return timeline.Event{
			At: at, Attempt: attempt, Run: run, Node: node, Kind: kind,
			Payload: b,
			Subject: timeline.Subject{Kind: "pr", Ref: "demo-rezuscloud/harmostes#42", Title: "Fixture narrative"},
		}
	}

	// Attempt 1 (terminal #42): the full story — gate armed, three runs with
	// node start/completion, a plugin tail, agent turn + tool, gate verdict,
	// terminal run.completed. The ledger adds trigger + claim rows.
	att1 := "attempt-pr-review-demo-42a1"
	events := []timeline.Event{
		ev(t(1, 0).Time, att1, "", "gate", timeline.KindGateArmed,
			map[string]any{"head": "b41fb712abcdef", "pr": "demo-rezuscloud/harmostes#42"}),
		ev(t(6, 0).Time, att1, "pr-review-demo-42a1-prepare", "", timeline.KindRunStarted,
			map[string]any{"source": "gate"}),
		ev(t(7, 0).Time, att1, "pr-review-demo-42a1-prepare", "prepare", timeline.KindNodeStarted,
			map[string]any{"type": "plugin"}),
		ev(t(9, 0).Time, att1, "pr-review-demo-42a1-prepare", "prepare", timeline.KindPluginTail,
			map[string]any{"line": "cloning demo-rezuscloud/harmostes …"}),
		ev(t(14, 30).Time, att1, "pr-review-demo-42a1-prepare", "prepare", timeline.KindNodeCompleted,
			map[string]any{"type": "plugin", "status": "green", "durationMs": 300000}),
		ev(t(15, 0).Time, att1, "pr-review-demo-42a1-prepare", "", timeline.KindRunCompleted,
			map[string]any{"status": "succeeded", "source": "gate"}),
		ev(t(20, 0).Time, att1, "pr-review-demo-42a1-agent", "", timeline.KindRunStarted,
			map[string]any{"source": "gate"}),
		ev(t(21, 0).Time, att1, "pr-review-demo-42a1-agent", "agent", timeline.KindNodeStarted,
			map[string]any{"type": "agent"}),
		ev(t(30, 0).Time, att1, "pr-review-demo-42a1-agent", "agent", timeline.KindAgentTurn,
			map[string]any{"turn": 0, "label": "read the diff", "tokensIn": 1840, "tokensOut": 212}),
		ev(t(45, 0).Time, att1, "pr-review-demo-42a1-agent", "agent", timeline.KindAgentTool,
			map[string]any{"tool": "gh pr diff"}),
		ev(t(60, 0).Time, att1, "pr-review-demo-42a1-agent", "agent", timeline.KindAgentTurn,
			map[string]any{"turn": 1, "label": "post review", "green": true, "tokensIn": 12480, "tokensOut": 986}),
		ev(t(13*60+25, 0).Time, att1, "pr-review-demo-42a1-agent", "agent", timeline.KindNodeCompleted,
			map[string]any{"type": "agent", "status": "green", "durationMs": 468000}),
		ev(t(13*60+35, 0).Time, att1, "pr-review-demo-42a1-agent", "", timeline.KindRunCompleted,
			map[string]any{"status": "succeeded", "source": "gate"}),
		ev(t(13*60+40, 0).Time, att1, "pr-review-demo-42a1-gate", "", timeline.KindRunStarted,
			map[string]any{"source": "gate"}),
		ev(t(13*60+45, 0).Time, att1, "pr-review-demo-42a1-gate", "gate", timeline.KindNodeStarted,
			map[string]any{"type": "gate"}),
		ev(t(14*60+8, 0).Time, att1, "pr-review-demo-42a1-gate", "gate", timeline.KindGateProceed,
			map[string]any{"reason": "verdict posted", "pr": "demo-rezuscloud/harmostes#42"}),
		ev(t(14*60+10, 0).Time, att1, "pr-review-demo-42a1-gate", "gate", timeline.KindNodeCompleted,
			map[string]any{"type": "gate", "status": "green", "durationMs": 30000, "feedback": "all claims validated"}),
		ev(t(14*60+12, 0).Time, att1, "pr-review-demo-42a1-gate", "", timeline.KindRunCompleted,
			map[string]any{"status": "succeeded", "message": "verdict posted on demo-rezuscloud/harmostes#42", "source": "gate"}),
	}

	// Attempt 2 (mid-flight #43): armed, waiting on capacity, prepare done,
	// agent in flight — the live position the SSE stream keeps fresh.
	att2 := "attempt-pr-review-demo-43c2"
	events = append(events,
		ev(t(30, 30).Time, att2, "", "gate", timeline.KindGateArmed,
			map[string]any{"head": "9c02aa01feedbeef", "pr": "demo-rezuscloud/harmostes#43"}),
		ev(t(31, 0).Time, att2, "", "gate", timeline.KindGateWaiting,
			map[string]any{"reason": "capacity"}),
		ev(t(30, 0).Time, att2, "pr-review-demo-43c2-prepare", "", timeline.KindRunStarted,
			map[string]any{"source": "gate"}),
		ev(t(30, 30).Time, att2, "pr-review-demo-43c2-prepare", "prepare", timeline.KindNodeStarted,
			map[string]any{"type": "plugin"}),
		ev(t(30, 45).Time, att2, "pr-review-demo-43c2-prepare", "prepare", timeline.KindNodeCompleted,
			map[string]any{"type": "plugin", "status": "green", "durationMs": 12000}),
		ev(t(30, 50).Time, att2, "pr-review-demo-43c2-prepare", "", timeline.KindRunCompleted,
			map[string]any{"status": "succeeded", "source": "gate"}),
		ev(t(31, 20).Time, att2, "pr-review-demo-43c2-agent", "", timeline.KindRunStarted,
			map[string]any{"source": "gate"}),
		ev(t(31, 30).Time, att2, "pr-review-demo-43c2-agent", "agent", timeline.KindNodeStarted,
			map[string]any{"type": "agent"}),
	)

	var seededMax time.Time
	for _, ev := range events {
		if ev.At.After(seededMax) {
			seededMax = ev.At
		}
	}
	return &FixTimeline{events: events, seededMax: seededMax}
}

// Attempt implements timeline.Reader over the seeded narrative.
func (f *FixTimeline) Attempt(ctx context.Context, attempt string, runs []string, fltr timeline.Filter) ([]timeline.Event, error) {
	runSet := make(map[string]bool, len(runs))
	for _, r := range runs {
		runSet[r] = true
	}
	var out []timeline.Event
	for _, ev := range f.snapshot() {
		if ev.Attempt != attempt || ev.Run == "" || !runSet[ev.Run] {
			continue
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// GateEvents implements timeline.Reader: gate-keyed events for one attempt
// (the real store keeps them under timeline/<attempt>/gate/<seq>, outside
// any run keyset; the fake separates them by empty Run).
func (f *FixTimeline) GateEvents(ctx context.Context, attempt string, fltr timeline.Filter) ([]timeline.Event, error) {
	var out []timeline.Event
	for _, ev := range f.snapshot() {
		if ev.Attempt == attempt && ev.Run == "" {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Subjects implements timeline.Reader: one indexed subject per attempt.
func (f *FixTimeline) Subjects(ctx context.Context, attempts []string) (map[string]timeline.Subject, error) {
	out := make(map[string]timeline.Subject, len(attempts))
	for _, a := range attempts {
		out[a] = timeline.Subject{Kind: "pr", Ref: "demo-rezuscloud/harmostes#42", Title: "Fixture narrative"}
	}
	return out, nil
}
