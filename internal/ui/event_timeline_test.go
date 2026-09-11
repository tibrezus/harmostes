package ui

// event_timeline_test.go — the Event Timeline projection (ADR-0012 §4):
// merge correctness (store + ledger, dedupe, sort), degradation (store
// unreachable → notice row, nil reader → explicit empty-state), and the
// handler contract (owner gate, fragment shape).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/timeline"
)

// fakeReader is a scriptable timeline.Reader for projection tests.
type fakeReader struct {
	attemptEvents []timeline.Event
	gateEvents    []timeline.Event
	attemptErr    error
	gateErr       error
}

func (f *fakeReader) Attempt(ctx context.Context, attempt string, runs []string, f2 timeline.Filter) ([]timeline.Event, error) {
	if f.attemptErr != nil {
		return nil, f.attemptErr
	}
	return f.attemptEvents, nil
}

func (f *fakeReader) GateEvents(ctx context.Context, attempt string, f2 timeline.Filter) ([]timeline.Event, error) {
	if f.gateErr != nil {
		return nil, f.gateErr
	}
	return f.gateEvents, nil
}

func (f *fakeReader) Subjects(ctx context.Context, attempts []string) (map[string]timeline.Subject, error) {
	return map[string]timeline.Subject{}, nil
}

func evAt(kind, run, node string, payload map[string]any, at time.Time) timeline.Event {
	b, _ := json.Marshal(payload)
	return timeline.Event{At: at, Kind: kind, Run: run, Node: node, Attempt: "attempt-x", Payload: b}
}

func tlAttempt() *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-x", Namespace: "test-ns",
			Labels:            map[string]string{v1alpha1.OwnerLabel: "tibrez"},
			CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)),
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "test-ns/wf",
			Objective: v1alpha1.ObjectiveSpec{
				Kind:           v1alpha1.ObjectiveKindPRReview,
				PrimarySubject: v1alpha1.Subject{Binding: "github", Object: "org/repo#7"},
				TargetedState:  "abc123",
			},
		},
		Status: v1alpha1.AttemptStatus{
			Phase: v1alpha1.AttemptPhaseValidated,
			Runs: []v1alpha1.RunRecord{
				{Name: "run-1", StartedAt: metav1.NewTime(time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)),
					EndedAt: metav1.NewTime(time.Date(2026, 9, 11, 12, 9, 0, 0, time.UTC)), Phase: "succeeded"},
			},
		},
	}
}

// The full story renders in order: trigger (ledger) → store events →
// ledger gap-fill; sorted oldest-first throughout.
func TestBuildEventTimeline_MergesAndSorts(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{
		attemptEvents: []timeline.Event{
			evAt(timeline.KindRunStarted, "run-1", "", map[string]any{"source": "gate"},
				time.Date(2026, 9, 11, 12, 5, 0, 0, time.UTC)),
			evAt(timeline.KindNodeCompleted, "run-1", "prepare", map[string]any{"type": "plugin", "status": "green", "durationMs": 90000},
				time.Date(2026, 9, 11, 12, 8, 30, 0, time.UTC)),
			evAt(timeline.KindRunCompleted, "run-1", "", map[string]any{"status": "succeeded", "message": "ok"},
				time.Date(2026, 9, 11, 12, 9, 0, 0, time.UTC)),
		},
	})

	view := s.buildEventTimeline(context.Background(), att)
	if !view.StoreOK || view.Empty != "" {
		t.Fatalf("view should be populated, got empty=%q storeOK=%v", view.Empty, view.StoreOK)
	}

	var kinds []string
	for _, row := range view.Rows {
		kinds = append(kinds, row.Kind)
	}
	// trigger (ledger, creation) → run.started (store) → node.completed
	// (store) → run.completed (store). NO ledger "run started"/"run ended"
	// duplicates: the store holds both boundaries.
	want := []string{rowKindTrigger, timeline.KindRunStarted, timeline.KindNodeCompleted, timeline.KindRunCompleted}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if got := view.Rows[1].State; got != "in flight" {
		t.Errorf("run.started state = %q, want in flight", got)
	}
	if got := view.Rows[2].State; got != "validated" {
		t.Errorf("node.completed(green) state = %q, want validated", got)
	}
	if got := view.Rows[3].Summary; got != "run ended: ok" {
		t.Errorf("run.completed summary = %q, want the payload message", got)
	}
	if view.Rows[2].Payload == "" {
		t.Error("store rows must carry their payload for the expander")
	}
}

// Ledger run boundaries fill ONLY gaps the store's TTL left behind.
func TestBuildEventTimeline_LedgerFillsStoreGaps(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{}) // store empty: TTL'd away

	view := s.buildEventTimeline(context.Background(), att)
	var kinds []string
	for _, row := range view.Rows {
		kinds = append(kinds, row.Kind)
	}
	want := []string{rowKindTrigger, rowKindRunStarted, rowKindRunEnded}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if got := view.Rows[2].State; got != "validated" {
		t.Errorf("ledger run ended(succeeded) state = %q, want validated", got)
	}
}

// Claim transitions project from the ledger with timestamps.
func TestBuildEventTimeline_ClaimRows(t *testing.T) {
	att := tlAttempt()
	armed := metav1.NewTime(time.Date(2026, 9, 11, 12, 1, 0, 0, time.UTC))
	dispatched := metav1.NewTime(time.Date(2026, 9, 11, 12, 2, 0, 0, time.UTC))
	att.Status.Review = &v1alpha1.ReviewClaimStatus{
		PR: "org/repo#7", HeadSHA: "abc123def456",
		ArmedSince: &armed, DispatchedAt: &dispatched,
	}
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{})

	view := s.buildEventTimeline(context.Background(), att)
	if len(view.Rows) < 3 {
		t.Fatalf("want trigger + armed + dispatched rows, got %d", len(view.Rows))
	}
	if view.Rows[1].Kind != rowKindClaimArmed || view.Rows[2].Kind != rowKindClaimDispatch {
		t.Errorf("rows 1,2 = %q,%q; want claim armed then dispatched", view.Rows[1].Kind, view.Rows[2].Kind)
	}
	if view.Rows[1].State != "armed" || view.Rows[2].State != "in flight" {
		t.Errorf("claim states = %q,%q; want armed, in flight", view.Rows[1].State, view.Rows[2].State)
	}
	if !strings.Contains(view.Rows[1].Summary, "abc123d") {
		t.Errorf("armed summary = %q, want the truncated head SHA", view.Rows[1].Summary)
	}
}

// Store unreachable → the page never breaks: ledger facts plus a notice row.
func TestBuildEventTimeline_StoreUnreachableDegrades(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{attemptErr: errors.New("sidecar gone")})

	view := s.buildEventTimeline(context.Background(), att)
	if view.StoreOK {
		t.Error("StoreOK must be false when the store errors")
	}
	found := false
	for _, row := range view.Rows {
		if row.Kind == "store unavailable" && strings.Contains(row.Summary, "sidecar gone") {
			found = true
		}
	}
	if !found {
		t.Error("a notice row naming the failure must render")
	}
}

// Nil reader → explicit empty-state, never a silent blank.
func TestBuildEventTimeline_NilReader(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	view := s.buildEventTimeline(context.Background(), att)
	if view.StoreOK || view.Empty == "" {
		t.Errorf("nil reader must render an explicit empty-state, got %+v", view)
	}
}

// Gate events merge in with the run events, sorted by time.
func TestBuildEventTimeline_GateEventsMerge(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{
		gateEvents: []timeline.Event{
			evAt(timeline.KindGateArmed, "", "gate", map[string]any{"head": "abc123"},
				time.Date(2026, 9, 11, 12, 1, 0, 0, time.UTC)),
			evAt(timeline.KindGateProceed, "", "gate", map[string]any{"reason": "verdict posted"},
				time.Date(2026, 9, 11, 12, 10, 0, 0, time.UTC)),
		},
	})

	view := s.buildEventTimeline(context.Background(), att)
	last := view.Rows[len(view.Rows)-1]
	if last.Kind != timeline.KindGateProceed || last.State != "verdict" {
		t.Errorf("last row = %q (%q), want gate.proceed with verdict chip", last.Kind, last.State)
	}
	if last.Node != "gate" {
		t.Errorf("gate rows must show the gate as actor, got %q", last.Node)
	}
}

// The fragment endpoint renders rows under the data-testid contract, and
// the owner gate applies (foreign attempts are 404s).
func TestHandleRunEvents_FragmentAndOwnerGate(t *testing.T) {
	att := tlAttempt()
	s := adminTestServer(t, att)
	s.SetTimelineReader(&fakeReader{
		attemptEvents: []timeline.Event{
			evAt(timeline.KindRunCompleted, "run-1", "", map[string]any{"status": "succeeded"},
				time.Date(2026, 9, 11, 12, 9, 0, 0, time.UTC)),
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/events", nil).WithContext(
		withIdentity(context.Background(), idWith("tibrez", "")))
	req.SetPathValue("name", "attempt-x")
	rec := httptest.NewRecorder()
	s.handleRunEvents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events fragment: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="event-timeline"`,
		`data-testid="timeline-row"`,
		`data-testid="timeline-row-kind"`,
		`data-testid="timeline-row-time"`,
		`data-testid="timeline-row-details"`,
		`data-testid="timeline-row-payload"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment missing %s", want)
		}
	}

	// Foreign identity: indistinguishable 404, no existence leak.
	req2 := httptest.NewRequest(http.MethodGet, "/runs/attempt-x/events", nil).WithContext(
		withIdentity(context.Background(), idWith("mallory", "")))
	req2.SetPathValue("name", "attempt-x")
	rec2 := httptest.NewRecorder()
	s.handleRunEvents(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("foreign attempt events = %d, want 404", rec2.Code)
	}
}

// eventState: the chip-vocabulary parity (contract place 3 — with the typed
// vocabulary in attempts.go and the state-chip template arm).
func TestEventState_ParityAndFallthrough(t *testing.T) {
	cases := []struct {
		kind, status, want string
	}{
		{rowKindTrigger, "", "queued"},
		{rowKindClaimArmed, "", "armed"},
		{rowKindClaimDispatch, "", "in flight"},
		{rowKindRunStarted, "", "in flight"},
		{rowKindRunEnded, "succeeded", "validated"},
		{rowKindRunEnded, "failed", "failed"},
		{rowKindRunEnded, "running", "in flight"},
		{timeline.KindRunStarted, "", "in flight"},
		{timeline.KindRunCompleted, "succeeded", "validated"},
		{timeline.KindRunCompleted, "failed", "failed"},
		{timeline.KindNodeStarted, "", "in flight"},
		{timeline.KindNodeCompleted, "green", "validated"},
		{timeline.KindNodeCompleted, "red", "failed"},
		{timeline.KindNodeCompleted, "skipped", "skipped"},
		{timeline.KindAgentTurn, "", "in flight"},
		{timeline.KindAgentTool, "", "in flight"},
		{timeline.KindGateArmed, "", "armed"},
		{timeline.KindGateWaiting, "", "queued"},
		{timeline.KindGateProceed, "", "verdict"},
		{timeline.KindGateStanddown, "", "queued"},
		{timeline.KindGateCancel, "", "superseded"},
		{timeline.KindPluginTail, "", "plugin.tail"},
	}
	for _, c := range cases {
		if got := eventState(c.kind, c.status); got != c.want {
			t.Errorf("eventState(%q, %q) = %q, want %q", c.kind, c.status, got, c.want)
		}
	}
	// Unknown kinds fall through verbatim — never a wrong state.
	if got := eventState("cron.fired", ""); got != "cron.fired" {
		t.Errorf("unknown kind must pass through verbatim, got %q", got)
	}
}

// etStateClass: the marker class is exhaustive over the vocabulary the
// state-chip template arms — a chip and its marker can never disagree.
func TestEtStateClass_MatchesChipVocabulary(t *testing.T) {
	chipStates := []string{"failed", "in flight", "reconciling", "verdict", "validated",
		"armed", "queued", "dispatch lost", "superseded"}
	for _, st := range chipStates {
		class := etStateClass(st)
		switch class {
		case "ok", "fail", "run", "neutral":
			// covered below
		default:
			t.Errorf("etStateClass(%q) = %q: not a marker class", st, class)
		}
	}
	if etStateClass("validated") != "ok" || etStateClass("failed") != "fail" ||
		etStateClass("in flight") != "run" || etStateClass("anything-new") != "neutral" {
		t.Error("marker classes diverged from the chip vocabulary")
	}
}
