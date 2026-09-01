package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func graphTestSpec() v1alpha1.GraphSpec {
	return v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "deploy", Type: "plugin", Label: "deploy"},
			{ID: "prepare", Type: "plugin", Label: "prepare"},
			{ID: "agent", Type: "agent", Label: "agent"},
			{ID: "upstream", Type: "external", Label: "upstream"},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "prepare", To: "agent"},
			{From: "agent", To: "deploy"},
			{From: "prepare", To: "deploy"},
		},
	}
}

func graphEnvelope(nodeID, status string, at metav1.Time) v1alpha1.NodeResultEnvelope {
	return v1alpha1.NodeResultEnvelope{NodeID: nodeID, Status: status, ProducedAt: at}
}

// Latest envelope per node wins; depths form columns; the live position is
// the first envelope-less executable node in topological order when a run is
// in flight; external nodes are never the running position.
func TestLayoutGraphStateMerge(t *testing.T) {
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-time.Minute))
	latest := map[string]v1alpha1.NodeResultEnvelope{
		"prepare": graphEnvelope("prepare", "ok", older),
		"agent":   graphEnvelope("agent", "failed", now), // newer beats older
	}

	// Sanity: two envelopes for the same node — the merge happens in
	// buildRunGraph, so simulate it here the same way.
	results := []v1alpha1.NodeResultEnvelope{
		graphEnvelope("prepare", "failed", older),
		graphEnvelope("prepare", "ok", now),
	}
	merged := map[string]v1alpha1.NodeResultEnvelope{}
	for _, env := range results {
		if cur, ok := merged[env.NodeID]; !ok || env.ProducedAt.After(cur.ProducedAt.Time) {
			merged[env.NodeID] = env
		}
	}
	if merged["prepare"].Status != "ok" {
		t.Errorf("latest envelope = %q, want ok", merged["prepare"].Status)
	}

	nodes, edges, w, h := layoutGraph(graphTestSpec(), latest, true)
	byID := map[string]graphNodeView{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if byID["prepare"].Status != graphStateOK {
		t.Errorf("prepare = %q, want ok", byID["prepare"].Status)
	}
	if byID["agent"].Status != graphStateFailed {
		t.Errorf("agent = %q, want failed", byID["agent"].Status)
	}
	if byID["deploy"].Status != graphStateRunning {
		t.Errorf("deploy = %q, want running (first envelope-less in topo order)", byID["deploy"].Status)
	}
	if byID["upstream"].Status != graphStateExternal {
		t.Errorf("upstream = %q, want external", byID["upstream"].Status)
	}
	// Depths: prepare=0, agent=1, deploy=2 (longest path), upstream=0.
	if byID["prepare"].X >= byID["agent"].X || byID["agent"].X >= byID["deploy"].X {
		t.Errorf("columns not ordered by depth: %d %d %d", byID["prepare"].X, byID["agent"].X, byID["deploy"].X)
	}
	if len(edges) != 3 {
		t.Errorf("edges = %d, want 3", len(edges))
	}
	for _, e := range edges {
		if !strings.HasPrefix(e.Path, "M ") {
			t.Errorf("edge %v→%v path %q not svg", e.From, e.To, e.Path)
		}
	}
	if w <= 0 || h <= 0 {
		t.Errorf("viewport %dx%d", w, h)
	}

	// No run in flight → nothing pulses.
	nodes, _, _, _ = layoutGraph(graphTestSpec(), latest, false)
	for _, n := range nodes {
		if n.Status == graphStateRunning {
			t.Errorf("node %s running with no in-flight run", n.ID)
		}
	}

	// In flight but everything has envelopes → nothing pulses.
	full := map[string]v1alpha1.NodeResultEnvelope{
		"prepare": graphEnvelope("prepare", "ok", now),
		"agent":   graphEnvelope("agent", "ok", now),
		"deploy":  graphEnvelope("deploy", "ok", now),
	}
	nodes, _, _, _ = layoutGraph(graphTestSpec(), full, true)
	for _, n := range nodes {
		if n.Status == graphStateRunning {
			t.Errorf("node %s running with all envelopes present", n.ID)
		}
	}
}

func graphSeedWorkflow(name string) *v1alpha1.Workflow {
	return &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{Graph: ptrGraphSpec(graphTestSpec())},
	}
}

func ptrGraphSpec(gs v1alpha1.GraphSpec) *v1alpha1.GraphSpec { return &gs }

// The run detail page renders the graph: svg, node states, the live pulse,
// the hover payload, and the panel.
func TestRunDetailRendersGraph(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		graphEnvelope("prepare", "ok", metav1.Now()),
	}
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-pr-review-x-1", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="run-graph-svg"`,
		`rg-node--running`, // prepare has an envelope, agent doesn't, run in flight
		`rg-node--ok`,      // prepare
		`data-node="agent"`,
		`id="run-graph-data"`,
		`id="rg-panel"`,
		`rungraph`, // the SSE event name in the page script
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail missing %q", want)
		}
	}
}

// A deleted workflow spec degrades to a placeholder, never an error page.
func TestRunDetailGraphWorkflowMissing(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	s := newAttemptTestServer(t, att)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-pr-review-x-1", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with placeholder", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "workflow spec unavailable") {
		t.Errorf("missing placeholder:\n%.400s", rec.Body.String())
	}
}

// The graph SSE stream: initial paint, wake on lifecycle events for the
// attempt's workflow, 404 for unknown attempts.
func TestRunGraphSSEStream(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		graphEnvelope("prepare", "ok", metav1.Now()),
	}
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// Unknown attempt → 404.
	req404, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/runs/nope/graph/events", nil)
	req404.Header.Set("X-Authentik-Username", "alice")
	resp404, err := http.DefaultClient.Do(req404)
	if err != nil {
		t.Fatalf("404 probe: %v", err)
	}
	resp404.Body.Close()
	if resp404.StatusCode != http.StatusNotFound {
		t.Errorf("unknown attempt status = %d, want 404", resp404.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/runs/attempt-pr-review-x-1/graph/events", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()

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
				t.Fatalf("stream ended waiting for %q: %v", marker, err)
			}
		}
		t.Fatalf("deadline waiting for %q", marker)
	}

	readUntil("run-graph-svg")

	// A lifecycle event for the attempt's workflow wakes a re-render.
	evBody, _ := json.Marshal(map[string]any{
		"data": map[string]any{"event": "node.completed", "pipeline": "pr-review-x", "node": "agent"},
	})
	before := strings.Count(buf.String(), "event: rungraph")
	post, err := http.Post(srv.URL+"/dapr/events", "application/json", bytes.NewReader(evBody))
	if err != nil {
		t.Fatalf("dapr post: %v", err)
	}
	post.Body.Close()
	readUntilMarker := before + 1
	deadline, _ := ctx.Deadline()
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), "event: rungraph") >= readUntilMarker {
			break
		}
		n, err := reader.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
	}
	if strings.Count(buf.String(), "event: rungraph") < readUntilMarker {
		t.Error("no re-render after lifecycle event")
	}
}

// The hover payload survives template escaping: JSON.parse(textContent)
// must round-trip quotes and angles.
func TestRunGraphNodeDataJSONEscaping(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		{
			NodeID:     "prepare",
			Status:     "ok",
			Summary:    `posted "needs-review" <b>verdict</b> & fled`,
			ProducedAt: metav1.Now(),
		},
	}
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)

	req := httptest.NewRequest(http.MethodGet, "/runs/attempt-pr-review-x-1", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	body := rec.Body.String()
	const openTag = `"application/json">`
	start := strings.Index(body, openTag)
	if start < 0 {
		t.Fatal("no data script")
	}
	seg := body[start+len(openTag):]
	end := strings.Index(seg, "</script>")
	if end < 0 {
		t.Fatal("unterminated data script")
	}
	seg = seg[:end]
	var parsed map[string]map[string]any
	if err := json.Unmarshal([]byte(seg), &parsed); err != nil {
		t.Fatalf("embedded JSON not parseable: %v\nseg=%.120s", err, seg)
	}
	got := parsed["prepare"]["summary"]
	if !strings.Contains(got.(string), `"needs-review"`) || !strings.Contains(got.(string), "<b>") {
		t.Errorf("summary round-trip lost content: %q", got)
	}
}

// Multi-tenant gate (review F1/F2 probe): a foreign authenticated user gets
// the same 404 as an unknown attempt — on the graph stream AND on every
// attempt-scoped fragment endpoint — with no existence leak.
func TestAttemptScopeForeignUser404(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	paths := []string{
		"/runs/attempt-pr-review-x-1/graph/events",
		"/runs/attempt-pr-review-x-1/runs/attempt-pr-review-x-1-run/logs",
	}
	for _, path := range paths {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		req.Header.Set("X-Authentik-Username", "bob")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("bob probe %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("bob %s: status = %d, want 404 (existence leak)", path, resp.StatusCode)
		}
		if strings.Contains(string(body), "pr-review-x") {
			t.Errorf("bob %s: body leaks attempt/workflow identifiers", path)
		}
	}

	// The owner still gets through.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+paths[0], nil)
	req.Header.Set("X-Authentik-Username", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("alice probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("alice graph stream status = %d, want 200", resp.StatusCode)
	}
}

// Attempt-scoped wake isolation (#295): a foreign attempt's event must NOT
// re-render this stream (filtered at the hub, before the channel); the
// attempt's own event and a legacy unattributed event must. Absence is
// asserted with a bounded wait longer than the 500ms debounce.
func TestRunGraphSSEAttemptScopedWake(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		graphEnvelope("prepare", "ok", metav1.Now()),
	}
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/runs/attempt-pr-review-x-1/graph/events", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var buf bytes.Buffer
	tmp := make([]byte, 8192)
	wakeWithAttempt := func(attempt string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"data": map[string]any{"event": "node.completed", "pipeline": "pr-review-x", "node": "agent", "attempt": attempt},
		})
		post, err := http.Post(srv.URL+"/dapr/events", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("dapr post: %v", err)
		}
		post.Body.Close()
	}
	readUntilMarkers := func(n int) {
		t.Helper()
		deadline, _ := ctx.Deadline()
		for time.Now().Before(deadline) {
			if strings.Count(buf.String(), "event: rungraph") >= n {
				return
			}
			nr, err := reader.Read(tmp)
			if nr > 0 {
				buf.Write(tmp[:nr])
			}
			if err != nil {
				t.Fatalf("stream ended: %v", err)
			}
		}
		t.Fatalf("deadline waiting for %d renders", n)
	}

	readUntilMarkers(1) // initial paint

	// Foreign attempt: filtered at the hub — no re-render within the wait window.
	wakeWithAttempt("attempt-pr-review-x-2")
	time.Sleep(1500 * time.Millisecond) // > debounce; a wake would have rendered
	if got := strings.Count(buf.String(), "event: rungraph"); got != 1 {
		t.Fatalf("foreign attempt's event re-rendered the stream (%d renders), want 1", got)
	}

	// Own attempt: wakes.
	wakeWithAttempt("attempt-pr-review-x-1")
	readUntilMarkers(2)

	// Legacy unattributed event: wakes conservatively (rolling-deploy safety).
	wakeWithAttempt("")
	readUntilMarkers(3)
}

// The timing waterfall (#298): per-node lanes with wall-clock-proportional
// bars, an overhead lane for queue+pod, and humanized durations — the answer
// to "where did the 13 minutes go" without leaving the run page.
func TestRunDetailTimingWaterfall(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	att.CreationTimestamp = metav1.NewTime(base)

	env := func(node string, produced time.Time, durSec int64) v1alpha1.NodeResultEnvelope {
		e := graphEnvelope(node, "ok", metav1.NewTime(produced))
		e.DurationMs = durSec * 1000
		return e
	}
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		env("prepare", base.Add(10*time.Second), 5),                              // ran 5s, finished T0+10s
		env("agent", base.Add(10*time.Second+13*time.Minute+time.Second), 13*60), // 13m
		env("deploy", base.Add(10*time.Second+13*time.Minute+6*time.Second), 5),
	}
	// External node: has an envelope but must not get a lane.
	ext := graphEnvelope("upstream", "external", metav1.NewTime(base.Add(time.Second)))
	ext.DurationMs = 60 * 1000
	att.Status.NodeResults = append(att.Status.NodeResults, ext)

	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)
	// Through Routes(): the auth middleware materializes the session identity
	// the owner gate reads (a direct handler call skips it).
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/runs/attempt-pr-review-x-1", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(bodyBytes)

	if !strings.Contains(body, "rg-timing") {
		t.Fatal("no timing waterfall in run detail")
	}
	// Anchor on the strip block: node labels appear all over the page.
	stripStart := strings.Index(body, "rg-timing-title")
	stripEnd := stripStart + strings.Index(body[stripStart:], "</svg>")
	strip := body[stripStart:stripEnd]
	// Lane labels are html-escaped (+ becomes &#43;).
	for _, want := range []string{"queue&#43;pod", "prepare", "agent", "deploy"} {
		if !strings.Contains(strip, want) {
			t.Errorf("waterfall missing lane %q; strip=%.400s", want, strip)
		}
	}
	if lanes := strings.Count(strip, "rg-timing-lane"); lanes != 4 {
		t.Errorf("timing lanes = %d, want 4 (overhead + 3 nodes; external excluded)", lanes)
	}
	// The agent's bar must dominate: its lane width exceeds prepare's.
	agentW := extractTimingWidth(t, strip, "agent")
	prepW := extractTimingWidth(t, strip, "prepare")
	if agentW <= prepW {
		t.Errorf("agent bar width %d should dominate prepare %d", agentW, prepW)
	}
	// Durations are humanized (naming.go formatDuration).
	if !strings.Contains(strip, "13.0m") {
		t.Error("agent duration not rendered")
	}
	// Geometry is precomputed in Go (templates cannot multiply): lane offsets
	// must be exact multiples of the 22px lane height, and the viewBox height
	// must equal lanes*22.
	for i, offset := range []string{"translate(0, 0)", "translate(0, 22)", "translate(0, 44)", "translate(0, 66)"} {
		if !strings.Contains(strip, offset) {
			t.Errorf("lane %d geometry missing %q", i, offset)
		}
	}
	if !strings.Contains(strip, `viewBox="0 0 640 88"`) {
		t.Errorf("strip viewBox height wrong: want 88 (4 lanes x 22)")
	}
	// The hover panel carries the node duration too.
	if !strings.Contains(body, `"duration"`) {
		t.Error("nodeData missing duration field")
	}
}

func extractTimingWidth(t *testing.T, body, label string) int {
	t.Helper()
	marker := ">" + label + "</text>"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("lane %s not found", label)
	}
	seg := body[i:]
	wi := strings.Index(seg, "width=\"")
	if wi < 0 {
		t.Fatal("no width after lane label")
	}
	rest := seg[wi+len(`width="`):]
	end := strings.Index(rest, "\"")
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatalf("width parse: %v", err)
	}
	return n
}

// All-zero durations degrade to no strip at all (nothing to proportion).
func TestRunDetailTimingWaterfallZeroDurations(t *testing.T) {
	att := wallReviewAttempt("attempt-pr-review-x-1", "pr-review-x")
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	att.CreationTimestamp = metav1.NewTime(base)
	att.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		graphEnvelope("prepare", "ok", metav1.NewTime(base.Add(10*time.Second))),
		graphEnvelope("agent", "ok", metav1.NewTime(base.Add(20*time.Second))),
	}
	s := newAttemptTestServer(t, graphSeedWorkflow("pr-review-x"), att)
	view := s.buildRunGraph(context.Background(), att)
	if view.Timing != nil || view.TimingH != 0 {
		t.Errorf("zero-duration envelopes should yield an empty strip, got %d lanes h=%d", len(view.Timing), view.TimingH)
	}
}
