package timeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/internal/dapr"
)

// fakeDapr is an in-memory Dapr state store honouring the two extra methods
// the timeline seam needs.
type fakeDapr struct {
	mu   sync.Mutex
	vals map[string]string
	ttls map[string]time.Duration
}

func newFakeDapr() *fakeDapr {
	return &fakeDapr{vals: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakeDapr) GetState(_ context.Context, _, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[key], nil
}
func (f *fakeDapr) SaveState(ctx context.Context, store, key, value string) error {
	return f.SaveStateTTL(ctx, store, key, value, 0)
}
func (f *fakeDapr) SaveStateTTL(_ context.Context, _, key, value string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[key] = value
	f.ttls[key] = ttl
	return nil
}
func (f *fakeDapr) DeleteState(_ context.Context, _, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, key)
	return nil
}
func (f *fakeDapr) GetBulkState(_ context.Context, _ string, keys []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := f.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (f *fakeDapr) Publish(context.Context, string, string, string) error { return nil }
func (f *fakeDapr) GetSecret(context.Context, string, string) (map[string]string, error) {
	return nil, nil
}

var _ dapr.Client = (*fakeDapr)(nil)

func TestWriterKeyShapeAndOrder(t *testing.T) {
	fd := newFakeDapr()
	subj := Subject{Kind: "pr", Ref: "tibrez/rhesadox#1566", Title: "CI-tiering", SHA: "4c01cc4f"}
	w := NewWriter(fd, "statestore", "attempt-x", "pr-review-rhesadox", "run-1", subj)
	if err := w.SaveSubject(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Emit(t.Context(), KindNodeCompleted, "prepare", map[string]string{"status": "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	for _, seq := range []string{"000001", "000002", "000003"} {
		key := "timeline/attempt-x/run-1/" + seq
		if _, ok := fd.vals[key]; !ok {
			t.Fatalf("expected key %s", key)
		}
	}
	if _, ok := fd.vals["timeline/attempt-x/subject"]; !ok {
		t.Fatal("subject index key missing")
	}
	if fd.ttls["timeline/attempt-x/run-1/000001"] != DefaultTTL {
		t.Fatalf("attempt events should carry DefaultTTL, got %v", fd.ttls["timeline/attempt-x/run-1/000001"])
	}
}

func TestGateWriterNamespaceAndTTL(t *testing.T) {
	fd := newFakeDapr()
	w := NewGateWriter(fd, "statestore", "pr-review-rhesadox", "attempt-g", Subject{Kind: "pr", Ref: "x#1"})
	if err := w.Emit(t.Context(), KindGateArmed, "", nil); err != nil {
		t.Fatal(err)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	key := "timeline/attempt-g/gate/000001"
	if _, ok := fd.vals[key]; !ok {
		t.Fatalf("expected gate key %s", key)
	}
	if fd.ttls[key] != GateTTL {
		t.Fatalf("gate events should carry GateTTL, got %v", fd.ttls[key])
	}
}

func TestNilWriterIsNoop(t *testing.T) {
	var w *DaprWriter
	if err := w.Emit(t.Context(), KindRunStarted, "", nil); err != nil {
		t.Fatalf("nil writer must be nil-safe, got %v", err)
	}
	if err := w.SaveSubject(t.Context()); err != nil {
		t.Fatalf("nil SaveSubject must be nil-safe, got %v", err)
	}
}

func TestReaderAttemptProbesRunsAndOrders(t *testing.T) {
	fd := newFakeDapr()
	mk := func(attempt, run string) *DaprWriter {
		return NewWriter(fd, "s", attempt, "wf", run, Subject{Kind: "pr", Ref: "r#1"})
	}
	w1 := mk("attempt-y", "run-1")
	for i := 0; i < 3; i++ {
		_ = w1.Emit(t.Context(), KindNodeStarted, "prepare", nil)
	}
	_ = mk("attempt-y", "run-2").Emit(t.Context(), KindRunCompleted, "", map[string]string{"status": "ok"})

	r := NewReader(fd, "s")
	events, err := r.Attempt(t.Context(), "attempt-y", []string{"run-1", "run-2"}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].At.Before(events[i-1].At) {
			t.Fatal("events must be time-ordered")
		}
	}

	gated, _ := r.Attempt(t.Context(), "attempt-y", []string{"run-2"}, Filter{KindPrefix: "run."})
	if len(gated) != 1 || gated[0].Kind != KindRunCompleted {
		t.Fatalf("filter failed: %+v", gated)
	}
}

func TestReaderSubjectsBulk(t *testing.T) {
	fd := newFakeDapr()
	_ = NewWriter(fd, "s", "attempt-a", "wf", "run", Subject{Kind: "pr", Ref: "o/r#7", Title: "T"}).SaveSubject(t.Context())
	r := NewReader(fd, "s")
	subs, err := r.Subjects(t.Context(), []string{"attempt-a", "attempt-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if subs["attempt-a"].Ref != "o/r#7" {
		t.Fatalf("subject index wrong: %+v", subs)
	}
	if _, ok := subs["attempt-missing"]; ok {
		t.Fatal("missing attempt must be absent, not error")
	}
}

func TestFilterLimitKeepsNewest(t *testing.T) {
	events := []Event{
		{At: time.Now().Add(-3 * time.Second), Kind: "node.started"},
		{At: time.Now().Add(-2 * time.Second), Kind: "node.completed"},
		{At: time.Now().Add(-1 * time.Second), Kind: "run.completed"},
	}
	got := applyFilter(events, Filter{Limit: 2})
	if len(got) != 2 || got[0].Kind != "node.completed" {
		t.Fatalf("limit must keep newest, got %+v", got)
	}
}

func TestReaderReadsAllAcrossBatches(t *testing.T) {
	fd := newFakeDapr()
	w := NewWriter(fd, "s", "attempt-z", "wf", "run-1", Subject{Kind: "pr", Ref: "r#1"})
	const n = 100 // crosses the 64-key probe batch
	for i := 0; i < n; i++ {
		if err := w.Emit(t.Context(), KindNodeStarted, "n", nil); err != nil {
			t.Fatal(err)
		}
	}
	events, err := NewReader(fd, "s").Attempt(t.Context(), "attempt-z", []string{"run-1"}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != n {
		t.Fatalf("reader off-by-one/batch truncation: wrote %d, read %d", n, len(events))
	}
}

func TestGateEventsAndHoles(t *testing.T) {
	fd := newFakeDapr()
	gw := NewGateWriter(fd, "s", "wf", "attempt-g2", Subject{Kind: "pr", Ref: "r#2"})
	_ = gw.Emit(t.Context(), KindGateArmed, "", nil)
	_ = gw.Emit(t.Context(), KindGateProceed, "", nil)
	r := NewReader(fd, "s")
	events, err := r.GateEvents(t.Context(), "attempt-g2", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != KindGateArmed {
		t.Fatalf("gate events: %+v", events)
	}

	// Hole: delete the middle event of an attempt run — the walk must not
	// stop at the hole.
	w := NewWriter(fd, "s", "attempt-h", "wf", "run-9", Subject{})
	for i := 0; i < 4; i++ {
		_ = w.Emit(t.Context(), KindNodeStarted, "n", nil)
	}
	fd.mu.Lock()
	delete(fd.vals, "timeline/attempt-h/run-9/000002") // TTL-expired hole
	fd.mu.Unlock()
	events, err = r.Attempt(t.Context(), "attempt-h", []string{"run-9"}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("hole truncated the walk: got %d events, want 3", len(events))
	}
}
