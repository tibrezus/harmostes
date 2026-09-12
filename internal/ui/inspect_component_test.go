package ui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/tibrezus/harmostes/internal/ui/fixture"
)

// Component-tier DOM contract for the node inspector + version switcher
// (#419). The fixture template carries a 2-revision history: head (agent
// enabled, pr-fetch) and r1 (deterministic-only, pr-fetch-stale).

// getRaw fetches a path as the fixture user and returns the status code.
func getRaw(t *testing.T, ts *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("request %s: %v", path, err)
	}
	req.Header.Set("X-Harmostes-Dev-User", fixture.DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func islandSource(t *testing.T, doc *goquery.Document) string {
	t.Helper()
	return strings.TrimSpace(doc.Find("#code-island-source").Text())
}

// TestComponent_TemplateDetail_Inspector pins the default panel (agent) and
// the node-link wiring: fields render from the schema-derived descriptors
// with the template's current values, and the topology nodes navigate.
func TestComponent_TemplateDetail_Inspector(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")

	if n := doc.Find(`[data-testid="inspector"]`).Length(); n != 1 {
		t.Fatalf("inspector section count = %d, want 1", n)
	}
	form := doc.Find(`[data-testid="inspector-form"]`)
	if v, _ := form.Attr("data-node"); v != "agent" {
		t.Errorf("default inspector node = %q, want agent", v)
	}

	// Fields carry typed inputs + initial values for the change-diffing
	// client script. The model is the template's own.
	model := doc.Find(`input[name="agent.model"]`)
	if v, _ := model.Attr("value"); v != "mistral-small-latest" {
		t.Errorf("agent.model value = %q, want the template's model", v)
	}
	if _, ok := model.Attr("data-initial"); !ok {
		t.Error("fields must carry data-initial — the client diffs against it")
	}
	// bool kind renders a checkbox, checked (head agent is enabled).
	if chk := doc.Find(`input[name="agent.enabled"]`); chk.Length() == 1 {
		if _, ok := chk.Attr("checked"); !ok {
			t.Error("head agent.enabled should render checked (effective value)")
		}
	} else {
		t.Error("agent.enabled checkbox missing")
	}
	if n := doc.Find(`[data-testid="inspector-apply"]`).Length(); n != 1 {
		t.Error("Apply button missing on the head revision")
	}

	// Node navigation: the agent topology node links to its panel.
	link, ok := doc.Find(`[data-testid="topology-node"][data-node="agent"] a`).Attr("href")
	if !ok || link != "?node=agent" {
		t.Errorf("agent node link = %q, want ?node=agent", link)
	}

	// ?node=prepare swaps the panel to that node's fields.
	doc = getAsFixtureUser(t, ts, "/templates/pr-review?node=prepare")
	if doc.Find(`input[name="prepare.plugin.name"]`).Length() != 1 {
		t.Error("prepare panel missing its plugin field")
	}
	if doc.Find(`input[name="agent.model"]`).Length() != 0 {
		t.Error("prepare panel leaks agent fields")
	}

	// Unknown node param is a 404, not a silent fallback.
	resp := getRaw(t, ts, "/templates/pr-review?node=bogus")
	if resp != http.StatusNotFound {
		t.Errorf("unknown node: status = %d, want 404", resp)
	}
}

// TestComponent_TemplateDetail_VersionSwitcher pins projection consistency:
// ?rev=N switches pipeline, topology, document AND inspector values
// together; historical revisions render the inspector disabled; unknown
// revisions 404 (a mistyped URL must not silently show the wrong version).
func TestComponent_TemplateDetail_VersionSwitcher(t *testing.T) {
	ts := newFixtureServer(t)

	// The switcher renders on the head view (history exists) with head
	// selected, and the island carries the head document.
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")
	if opts := doc.Find(`[data-testid="rev-switch-option"]`); opts.Length() != 2 {
		t.Fatalf("switcher options = %d, want 2 (r1 + head)", opts.Length())
	}
	if src := islandSource(t, doc); !strings.Contains(src, "mistral-small-latest") {
		t.Error("head island document lacks the head model")
	}

	// r1: every projection switches. The historical spec was
	// deterministic-only (2 nodes, no agent, stale fetch plugin).
	doc = getAsFixtureUser(t, ts, "/templates/pr-review?rev=1")
	if n := doc.Find(`[data-testid="topology-node"]`).Length(); n != 2 {
		t.Errorf("r1 topology nodes = %d, want 2", n)
	}
	if src := islandSource(t, doc); strings.Contains(src, "mistral-small-latest") {
		t.Error("r1 document leaked the head model — projections must switch together")
	}
	if !strings.Contains(islandSource(t, doc), "pr-fetch-stale") {
		t.Error("r1 document lacks the stale fetch plugin")
	}
	// Inspector: disabled fields, r1's values, historical chip.
	if doc.Find(`input[name="agent.model"][disabled]`).Length() != 1 {
		t.Error("historical inspector must render disabled fields")
	}
	if doc.Find(`input[name="agent.enabled"][disabled]`).Length() != 1 {
		t.Error("historical inspector missing the enabled field")
	}
	if n := doc.Find(`[data-testid="rev-historical"]`).Length(); n != 1 {
		t.Error("historical chip missing")
	}
	if n := doc.Find(`[data-testid="inspector-apply"]`).Length(); n != 0 {
		t.Error("historical revision must not offer Apply")
	}

	// Head via explicit ?rev=2 is NOT historical.
	doc = getAsFixtureUser(t, ts, "/templates/pr-review?rev=2")
	if n := doc.Find(`[data-testid="rev-historical"]`).Length(); n != 0 {
		t.Error("explicit head selection marked historical")
	}

	// Unknown revision → 404.
	if got := getRaw(t, ts, "/templates/pr-review?rev=9"); got != http.StatusNotFound {
		t.Errorf("unknown revision: status = %d, want 404", got)
	}
	if got := getRaw(t, ts, "/templates/pr-review?rev=bogus"); got != http.StatusNotFound {
		t.Errorf("non-numeric revision: status = %d, want 404", got)
	}

	// A history-less template renders no switcher at all.
	doc = getAsFixtureUser(t, ts, "/templates/fork-maintenance")
	if n := doc.Find(`[data-testid="rev-switch"]`).Length(); n != 0 {
		t.Errorf("history-less template renders a %d-option switcher", doc.Find(`[data-testid="rev-switch-option"]`).Length())
	}
}

// TestComponent_InspectAPI pins the transform endpoint's HTTP contract:
// valid edits return the transformed document; rejections are 400s with the
// typed reason. The endpoint is a pure function over the posted document —
// it never touches the cluster, so no workflow name is involved.
func TestComponent_InspectAPI(t *testing.T) {
	ts := newFixtureServer(t)

	post := func(body string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/inspect", strings.NewReader(body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Harmostes-Dev-User", fixture.DevUser)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /api/inspect: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Happy path: one edit, the response carries the transformed document.
	code, body := post(`{"document":"kind: WorkflowTemplate\nspec:\n  agent:\n    model: old\n","edits":[{"path":"agent.model","value":"new"}]}`)
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body %s", code, body)
	}
	if !strings.Contains(body, `"document"`) || !strings.Contains(body, "model: new") {
		t.Errorf("response lacks transformed document: %s", body)
	}

	// Rejections: 400 + reason.
	for name, body := range map[string]string{
		"unknown path":   `{"document":"kind: WorkflowTemplate\n","edits":[{"path":"nope","value":"x"}]}`,
		"empty edits":    `{"document":"kind: WorkflowTemplate\n","edits":[]}`,
		"broken json":    `{"document":`,
		"broken yaml":    `{"document":"not: [valid","edits":[{"path":"description","value":"x"}]}`,
		"type violation": `{"document":"kind: WorkflowTemplate\n","edits":[{"path":"agent.enabled","value":"yes"}]}`,
	} {
		if code, respBody := post(body); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, code, respBody)
		}
	}
}
