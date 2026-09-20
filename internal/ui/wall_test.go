package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func wallTestServer(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	s := newAttemptTestServer(t, objs...)
	s.dapr = &usageStubDapr{}
	return s
}

// usageStubDapr answers EXACTLY the durable usage:last key for the seeded
// workflow and counts everything else as a miss — a suffix match would mask
// a namespaced-vs-bare key regression (the lookup must normalize the ref).
type usageStubDapr struct {
	DaprClient
	reads int
}

func (d *usageStubDapr) GetStateFromStore(_ context.Context, _, key string, value any) (bool, error) {
	d.reads++
	if key != "pr-review-x:usage:last" {
		return false, nil
	}
	b, _ := json.Marshal(map[string]any{"input": 100, "output": 50, "attempts": 2})
	return true, json.Unmarshal(b, value)
}

func wallReviewAttempt(name, wf string) *v1alpha1.Attempt {
	now := metav1.Now()
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Labels: map[string]string{
				v1alpha1.OwnerLabel:      "alice",
				"harmostes.dev/workflow": wf,
			},
			CreationTimestamp: now,
		},
		Spec: v1alpha1.AttemptSpec{
			// Production writes the namespaced ref (attempt/claim.go) — the
			// wall must normalize it to the bare CR name for cache/usage keys.
			WorkflowRef: "harmostes/" + wf,
			Objective:   v1alpha1.ObjectiveSpec{Kind: v1alpha1.ObjectiveKindPRReview},
		},
		Status: v1alpha1.AttemptStatus{
			Phase:     "running",
			LastRunAt: now,
			Runs:      []v1alpha1.RunRecord{{Name: name + "-run", Phase: "running", StartedAt: now}},
			Review:    &v1alpha1.ReviewClaimStatus{PR: "1710", HeadSHA: "abcdef1234567890", DispatchedAt: &now},
		},
	}
}

// The wall IS `/` now: real page (no redirect), grouped rollups with the
// deterministic facts — subject, phase/claim, head SHA, drill-down link.
func TestWallRendersGroups(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	s := wallTestServer(t, att)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"wall-grid",
		"1710",                               // review subject
		"in flight",                          // claim state badge
		"abcdef1",                            // short head SHA
		"pr-review-x",                        // workflow ref
		`href="/runs/attempt-pr-review-x-1"`, // drill-down
		"EventSource('/api/wall/events')",    // the live wire
	} {
		if !strings.Contains(body, want) {
			t.Errorf("wall missing %q", want)
		}
	}
}

// Agent metadata hydrates from the durable usage:last record when the event
// cache is cold (UI restart) — one state-store read per workflow, once.
func TestWallUsageHydratesFromStateStore(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	s := wallTestServer(t, att)
	stub := s.dapr.(*usageStubDapr)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "↑100 ↓50") {
		t.Errorf("wall missing hydrated usage:\n%s", rec.Body.String())
	}
	first := stub.reads

	// Second render: cache warm, no further state-store reads.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Authentik-Username", "alice")
	s.Routes().ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "↑100 ↓50") {
		t.Error("cached usage lost on second render")
	}
	if stub.reads != first {
		t.Errorf("usage reads = %d, want %d (cache must serve)", stub.reads, first)
	}
}

// noteWallEvent parses the JSON-roundtripped event Outputs into the cache.
func TestNoteWallEvent(t *testing.T) {
	s := newAttemptTestServer(t)
	s.noteWallEvent(Event{
		Event:    "node.completed",
		Pipeline: "wf-a",
		NodeType: "agent",
		Outputs: map[string]any{
			"usage":    map[string]any{"input_tokens": float64(42), "output_tokens": float64(7)},
			"model":    "zai/glm-5.2",
			"turns":    float64(3),
			"attempts": float64(1),
		},
	})
	u := s.wallMeta["wf-a"]
	if u == nil {
		t.Fatal("wallMeta not populated")
	}
	if u.InputTokens != 42 || u.OutputTokens != 7 || u.Model != "zai/glm-5.2" || u.Turns != 3 {
		t.Errorf("cached usage = %+v", u)
	}
	// No usage in Outputs → cache untouched.
	s.noteWallEvent(Event{Event: "node.started", Pipeline: "wf-b"})
	if _, ok := s.wallMeta["wf-b"]; ok {
		t.Error("event without usage must not populate cache")
	}
}

// The wall stream re-renders on lifecycle events and never touches the
// state store (cache-only fragments).
func TestWallSSEReRendersOnEvent(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	s := wallTestServer(t, att)
	stub := s.dapr.(*usageStubDapr)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/wall/events", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	reader := bufio.NewReader(resp.Body)
	var buf bytes.Buffer
	tmp := make([]byte, 8192)
	readUntil := func(marker string) {
		t.Helper()
		deadline, _ := ctx.Deadline()
		for time.Now().Before(deadline) {
			if strings.Contains(buf.String(), marker) {
				return
			}
			n, err := reader.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if err != nil {
				t.Fatalf("sse stream ended while waiting for %q: %v", marker, err)
			}
		}
		t.Fatalf("deadline waiting for %q in stream", marker)
	}

	// Initial paint arrives before any event. (#554: the fragment no
	// longer carries a .wall-grid wrapper — the section marker is the
	// first paint's fingerprint.)
	readUntil(`data-testid="wall-section"`)

	// A lifecycle event through the REAL ingress (dapr → hub + wall cache)
	// must produce a second fragment carrying the fresh agent metadata.
	evBody, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"event":    "node.completed",
			"pipeline": "pr-review-x",
			"nodeType": "agent",
			"outputs": map[string]any{
				"usage":    map[string]any{"input_tokens": 42, "output_tokens": 7},
				"model":    "test-model",
				"turns":    3,
				"attempts": 1,
			},
		},
	})
	pre := stub.reads
	preFrag := buf.Len()
	post, err := http.Post(srv.URL+"/dapr/events", "application/json", bytes.NewReader(evBody))
	if err != nil {
		t.Fatalf("dapr event post: %v", err)
	}
	_ = post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("dapr event status = %d", post.StatusCode)
	}
	readUntil("↑42 ↓7")
	if got := strings.Count(buf.String()[preFrag:], "event: wall"); got < 1 {
		t.Error("no second fragment after event")
	}
	if s.wallMeta["pr-review-x"] == nil {
		t.Error("wall cache not populated by ingress event")
	}
	if stub.reads != pre {
		t.Errorf("sse re-render read the state store %d time(s) — fragments must be cache-only", stub.reads-pre)
	}
}

// Nav shrinks to the three confirmed surfaces; killed destinations stay dead.
func TestNavIsFourEntries(t *testing.T) {
	s := wallTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	body := rec.Body.String()

	// #538: Templates joined the nav (Live / Runs / Workflows / Templates) —
	// definitions are first-class beside executions (donor pattern); the
	// #290 three-entry shrink is deliberately superseded.
	for _, want := range []string{`href="/"`, `href="/runs"`, `href="/workflows"`, `href="/templates"`} {
		if !strings.Contains(body, `class="ds-sidebar-link`) || !strings.Contains(body, want) {
			t.Errorf("nav missing %q", want)
		}
	}
	for _, gone := range []string{`href="/map"`, `href="/timeline"`, `href="/metrics"`, `href="/sessions"`} {
		if strings.Contains(body, gone) {
			t.Errorf("nav still links %q", gone)
		}
	}
}

// Standalone destinations whose engines were absorbed (#290 kill list) 404.
// /metrics left the kill list in #418: the route is back, deliberately, as
// the UI's own Prometheus endpoint (harmostes_ui_writes_total, #436) —
// different purpose, same path; pinned by TestMetricsServesWritesCounter.
func TestStandaloneDestinationsRemoved(t *testing.T) {
	s := wallTestServer(t)
	for _, path := range []string{
		"/map", "/timeline", "/sessions", "/api/metrics",
		"/api/timeline/events",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

// relTime: the wall's coarse relative ages.
func TestRelTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		rfc  string
		want string
	}{
		{now.Format(time.RFC3339), "now"},
		{now.Add(-5 * time.Minute).Format(time.RFC3339), "5m ago"},
		{now.Add(-3 * time.Hour).Format(time.RFC3339), "3h ago"},
		{now.Add(-2 * 24 * time.Hour).Format(time.RFC3339), "2d ago"},
		{"", ""},
		{"not-a-time", ""},
	}
	for _, c := range cases {
		if got := relTime(c.rfc, now); got != c.want {
			t.Errorf("relTime(%q) = %q, want %q", c.rfc, got, c.want)
		}
	}
}

// Repeated dispatch-losses — the signal that motivated the alert line —
// aggregate into ONE banner instead of repeating as indistinguishable rows.
// A single lost dispatch stays quiet.
func TestWallAlertAggregatesDispatchLosses(t *testing.T) {
	lost := func(name, wf, pr string) *v1alpha1.Attempt {
		att := wallReviewAttempt(name, wf)
		att.Status.Review = &v1alpha1.ReviewClaimStatus{
			PR:            pr,
			Released:      true,
			ReleaseReason: "dispatch-lost",
		}
		return att
	}

	t.Run("two losses collapse into one alert", func(t *testing.T) {
		s := wallTestServer(t,
			lost("attempt-pr-review-a-1", "pr-review-a", "1700"),
			lost("attempt-pr-review-b-1", "pr-review-b", "1701"),
		)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "data-testid=\"wall-alert\"") {
			t.Error("wall missing the dispatch-loss alert line")
		}
		if got := strings.Count(body, "dispatch lost"); got != 2 {
			t.Errorf("dispatch-lost chips = %d, want 2 (one per subject, no more)", got)
		}
	})

	t.Run("one loss stays quiet", func(t *testing.T) {
		s := wallTestServer(t, lost("attempt-pr-review-a-1", "pr-review-a", "1700"))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)

		if strings.Contains(rec.Body.String(), "data-testid=\"wall-alert\"") {
			t.Error("a single loss must not raise the alert line")
		}
	})
}

// wallSteps — the strip math. Widths ∝ wall-clock share, pending nodes as
// slots (bounded when timing exists, equal shares when nothing has run),
// dependency order left to right, externals omitted. Mutating any of these
// rules must fail here, not on prod.
func TestWallStepsProportionalToDuration(t *testing.T) {
	now := metav1.Now()
	latest := map[string]v1alpha1.NodeResultEnvelope{
		"prepare": {NodeID: "prepare", Status: "ok", ProducedAt: now, DurationMs: 1000},
		"agent":   {NodeID: "agent", Status: "ok", ProducedAt: now, DurationMs: 8000},
		"gate":    {NodeID: "gate", Status: "ok", ProducedAt: now, DurationMs: 1000},
	}
	views := []graphNodeView{
		{ID: "agent", Label: "agent", Status: graphStateOK, StatusText: "✓ ok"},
		{ID: "deploy", Label: "deploy", Status: graphStatePending, StatusText: "· pending"},
		{ID: "gate", Label: "gate", Status: graphStateOK, StatusText: "✓ ok"},
		{ID: "prepare", Label: "prepare", Status: graphStateOK, StatusText: "✓ ok"},
		{ID: "trigger", Label: "trigger", Status: graphStateExternal},
	}
	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "prepare", Type: "plugin"}, {ID: "agent", Type: "agent"},
			{ID: "gate", Type: "plugin"}, {ID: "deploy", Type: "plugin"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "prepare", To: "agent"}, {From: "agent", To: "gate"}, {From: "gate", To: "deploy"},
		},
	}
	steps, w := wallSteps(views, gs, latest, 140)
	if len(steps) != 4 {
		t.Fatalf("steps = %d, want 4 (externals omitted)", len(steps))
	}
	if w != 140 {
		t.Errorf("strip width = %d, want exactly 140 (no ragged svg boxes)", w)
	}
	// Dependency order: prepare, agent, gate, deploy.
	wantOrder := []string{"prepare", "agent", "gate", "deploy"}
	for i, want := range wantOrder {
		if steps[i].ID != want {
			t.Fatalf("step %d = %s, want %s", i, steps[i].ID, want)
		}
	}
	// Left to right.
	for i := 1; i < len(steps); i++ {
		if steps[i].X <= steps[i-1].X {
			t.Errorf("segment %s (x=%d) not right of %s (x=%d)", steps[i].ID, steps[i].X, steps[i-1].ID, steps[i-1].X)
		}
	}
	// The agent holds ~80% of timed wall clock — it must dominate the
	// timed width (pending slots take a bounded 30% budget).
	byID := map[string]wallStep{}
	for _, s := range steps {
		byID[s.ID] = s
	}
	if byID["agent"].Width <= byID["gate"].Width {
		t.Errorf("agent width %d must exceed gate width %d (8s vs 1s)", byID["agent"].Width, byID["gate"].Width)
	}
	if byID["deploy"].Width == 0 || byID["deploy"].Width > 140*3/10 {
		t.Errorf("pending slot width %d outside the bounded budget", byID["deploy"].Width)
	}
	if byID["deploy"].Status != graphStatePending {
		t.Errorf("envelope-less node status = %q, want pending", byID["deploy"].Status)
	}
	// Titles carry label + humanized duration + state glyph.
	if !strings.Contains(byID["agent"].Title, "agent") || !strings.Contains(byID["agent"].Title, "8.0s") {
		t.Errorf("agent title %q missing label/duration", byID["agent"].Title)
	}
}

// No envelopes at all → the strip is the workflow's SHAPE: equal shares,
// still dependency-ordered. A shape must render even before timing exists.
func TestWallStepsUntimedRendersShape(t *testing.T) {
	views := []graphNodeView{
		{ID: "agent", Label: "agent", Status: graphStateRunning, StatusText: "↻ running"},
		{ID: "deploy", Label: "deploy", Status: graphStatePending},
		{ID: "prepare", Label: "prepare", Status: graphStateOK, StatusText: "✓ ok"},
	}
	gs := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{{ID: "prepare"}, {ID: "agent"}, {ID: "deploy"}},
		Edges: []v1alpha1.EdgeSpec{{From: "prepare", To: "agent"}, {From: "agent", To: "deploy"}},
	}
	steps, w := wallSteps(views, gs, map[string]v1alpha1.NodeResultEnvelope{}, 140)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	if steps[0].ID != "prepare" || steps[1].ID != "agent" || steps[2].ID != "deploy" {
		t.Fatalf("order = %s,%s,%s — dependency order required", steps[0].ID, steps[1].ID, steps[2].ID)
	}
	// The strip tiles exactly.
	if w != 140 {
		t.Errorf("strip width = %d, want 140", w)
	}
	share := (140 - 4) / 3 // 2 gaps of 2px
	for i, s := range steps {
		// Equal shares; the last segment absorbs the rounding remainder
		// so the strip tiles exactly.
		want := share
		if i == len(steps)-1 {
			want = 140 - 4 - 2*share
		}
		if s.Width != want {
			t.Errorf("untimed step %s width = %d, want %d", s.ID, s.Width, want)
		}
	}
}
