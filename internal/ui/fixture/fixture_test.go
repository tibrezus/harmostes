package fixture

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/crdwalk"
	"github.com/tibrezus/harmostes/internal/timeline"
	"github.com/tibrezus/harmostes/internal/ui"
)

// The world parses and carries the fixture owner on every object.
func TestFixture_Objects(t *testing.T) {
	objs, err := Objects("fixture-ns")
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("objects = %d, want 3 workflows", len(objs))
	}
	names := map[string]bool{}
	for _, o := range objs {
		wf, ok := o.(*v1alpha1.Workflow)
		if !ok {
			t.Fatalf("object %T is not a Workflow", o)
		}
		if wf.Labels[v1alpha1.OwnerLabel] != ui.DevOwnerPrefix+DevUser {
			t.Errorf("workflow %s missing owner label", wf.Name)
		}
		// Graph-native workflows carry their graph; the thin instance (r1 of
		// the world) instead carries only a templateRef — its merged shape is
		// resolved at render time (#417).
		if wf.Spec.Graph == nil && wf.Spec.TemplateRef == "" {
			t.Errorf("workflow %s has neither graph nor templateRef", wf.Name)
		}
		names[wf.Name] = true
	}
	for _, want := range []string{"pr-review-demo", "merge-sync-demo", "pr-review-instance"} {
		if !names[want] {
			t.Errorf("workflow %q missing", want)
		}
	}
}

// The three attempts cover the narrative states with honest owner labels.
func TestFixture_Attempts(t *testing.T) {
	atts, err := Attempts("fixture-ns")
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(atts))
	}
	phases := map[string]int{}
	for _, o := range atts {
		a := o.(*v1alpha1.Attempt)
		if a.Labels[v1alpha1.OwnerLabel] != ui.DevOwnerPrefix+DevUser {
			t.Errorf("attempt %s missing owner label", a.Name)
		}
		if a.Spec.WorkflowRef == "" || a.Spec.Objective.Kind == "" {
			t.Errorf("attempt %s missing objective/workflowRef", a.Name)
		}
		phases[a.Status.Phase]++
	}
	if phases["validated"] != 1 || phases["reconciling"] != 1 || phases["superseded"] != 1 {
		t.Errorf("phase distribution %v, want one of each terminal+running", phases)
	}
}

// NewWorld serves the world through the real Routes() — the same handler
// the binary mounts; `-fixture` and the component tests ride identical code.
func TestFixture_NewWorld_ServesWorld(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewWorld("fixture-ns", logger, "../../../chart")
	if err != nil {
		t.Fatalf("NewWorld: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs", nil)
	req.Header.Set("X-Harmostes-Dev-User", DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /runs = %d, want 200", resp.StatusCode)
	}
}

// The fixture store grows from the cloud events its own ingress receives —
// the SSE-convergence seam (#421): ingest → hub wake → re-render sees the
// row. Node-level events map to store rows keyed <attempt-base>-<node>;
// anything foreign or unmappable passes through without a row.
func TestFixTimeline_Ingest(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWorld("fixture-ns", logger, "../../../chart")
	if err != nil {
		t.Fatalf("NewWorld: %v", err)
	}
	ts := httptest.NewServer(w.Routes())
	defer ts.Close()

	post := func(ce string) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/dapr/events", "application/json", strings.NewReader(ce))
		if err != nil {
			t.Fatalf("POST /dapr/events: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST /dapr/events = %d, want 200 (passthrough)", resp.StatusCode)
		}
	}

	att := "attempt-pr-review-demo-43c2"
	run := "pr-review-demo-43c2-agent"
	attempt := func() []timeline.Event {
		evs, err := w.timeline.Attempt(context.Background(), att, []string{run}, timeline.Filter{})
		if err != nil {
			t.Fatalf("Attempt: %v", err)
		}
		return evs
	}
	before := len(attempt())

	post(`{"data":{"event":"node.completed","pipeline":"pr-review-demo","attempt":"` + att + `","node":"agent","nodeType":"agent","status":"green","feedback":"e2e verdict"}}`)
	after := attempt()
	if len(after) != before+1 {
		t.Fatalf("ingest produced %d rows, want %d", len(after), before+1)
	}
	got := after[len(after)-1] // ingested rows carry now-timestamps: last
	if got.Kind != timeline.KindNodeCompleted || got.Node != "agent" || got.Run != run {
		t.Errorf("ingested row = %+v, want node.completed for %s", got, run)
	}
	if !strings.Contains(string(got.Payload), `"status":"green"`) {
		t.Errorf("payload missing status: %s", got.Payload)
	}

	// A foreign attempt and an unmappable event kind change nothing.
	post(`{"data":{"event":"node.completed","attempt":"attempt-somewhere-else","node":"agent"}}`)
	post(`{"data":{"event":"pipeline.started","attempt":"` + att + `"}}`)
	if n := len(attempt()); n != before+1 {
		t.Errorf("rows after foreign/unmappable events = %d, want %d", n, before+1)
	}
}

// The fixture serves the REAL chart CRDs (#436): what GET /api/schema
// returns must equal crdwalk.LoadSchema's reading of chart/crds — the
// e2e tier then validates against exactly what dev runs, by construction
// rather than by a lockstep helper.
func TestFixture_ServesChartCRDs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWorld("fixture-ns", logger, "../../../chart")
	if err != nil {
		t.Fatalf("NewWorld: %v", err)
	}
	ts := httptest.NewServer(w.Server().Routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/schema", nil)
	req.Header.Set("X-Harmostes-Dev-User", DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/schema: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	// The workflow half must carry the chart's node-type enum verbatim —
	// the topology palette's vocabulary (#417).
	dir := filepath.Join("..", "..", "..", "chart", "crds") + string(filepath.Separator)
	chartSchema, err := crdwalk.LoadSchema(dir, v1alpha1.WorkflowCRDFile)
	if err != nil {
		t.Fatalf("crdwalk: %v", err)
	}
	walk := func(node any, path ...string) any {
		for _, k := range path {
			if mm, ok := node.(map[string]any); ok {
				node = mm[k]
				continue
			}
			if list, ok := node.([]any); ok {
				i := 0
				for _, c := range k {
					if c < '0' || c > '9' {
						i = -1
						break
					}
					i = i*10 + int(c-'0')
				}
				if i >= 0 && i < len(list) {
					node = list[i]
					continue
				}
				return nil
			}
			return nil
		}
		return node
	}
	wantEnum := walk(chartSchema, "properties", "spec", "properties", "graph",
		"properties", "nodes", "items", "properties", "type", "enum")
	var served map[string]any
	if err := json.Unmarshal(body["workflow"], &served); err != nil {
		t.Fatalf("decode workflow half: %v", err)
	}
	gotEnum := walk(served, "properties", "spec", "properties", "graph",
		"properties", "nodes", "items", "properties", "type", "enum")
	if gotEnum == nil {
		t.Fatal("served schema has no graph.nodes.type enum — the chart CRD did not load")
	}
	wantJSON, _ := json.Marshal(wantEnum)
	gotJSON, _ := json.Marshal(gotEnum)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("served enum != chart enum:\nserved: %s\nchart:  %s", gotJSON, wantJSON)
	}
}

// The fixture's pr-review template IS the chart's — loaded from
// chart/values.yaml, the same file the MR bridge edits. The revision
// annotation's head copy (r2) must equal it: the reader treats the last
// recorded entry as the head only when it matches the live spec (#451).
func TestFixture_TemplateLockstepWithChartValues(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := NewWorld("fixture-ns", logger, "../../../chart")
	if err != nil {
		t.Fatalf("NewWorld: %v", err)
	}
	ts := httptest.NewServer(w.Server().Routes())
	defer ts.Close()

	// The template detail page serves the head document from the loaded
	// spec — assert through the served page, not the struct.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/templates/pr-review", nil)
	req.Header.Set("X-Harmostes-Dev-User", DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET template: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	pageBytes, _ := io.ReadAll(resp.Body)
	page := string(pageBytes)
	for _, marker := range []string{
		"litellm/ali/anthropic/qwen3.8-flash", // the chart's agent model
		"name: workspace",                     // the chart's prepare plugin
		"name: post-review",                   // the chart's deploy plugin
		"name: wiki",                          // the chart's third scope field
	} {
		if !strings.Contains(page, marker) {
			t.Errorf("served head document lacks chart marker %q", marker)
		}
	}

	// Annotation head-copy consistency: exactly 2 revisions (r1 + the head
	// copy equal to the live spec — a phantom r3 would mean drift).
	if opts := strings.Count(page, `data-testid="rev-switch-option"`); opts != 2 {
		t.Errorf("switcher options = %d, want 2 — annotation head must equal the live spec", opts)
	}
}
