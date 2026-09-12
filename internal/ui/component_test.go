// Component tests: the fixture world rendered through the real HTTP surface,
// asserted structurally with goquery. They pin the DOM contract that the
// Playwright E2E layer will drive — every selector here is a data-testid
// hook, so a hook rename breaks this test and not a browser run (milestone ⑤
// of #290).
//
// These live in package ui_test (not ui) so they import the fixture package,
// which itself imports ui — the same construction path the `-fixture` binary
// flag uses. What the tests see is exactly what a developer running
// `harmostes-ui -fixture` sees.
package ui_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/tibrezus/harmostes/internal/ui/fixture"
)

const fixtureNamespace = "fixture-ns"

// newFixtureServer builds the fixture world and an httptest server fronting
// the real Routes() — auth middleware included.
func newFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := fixture.NewWorld(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	ts := httptest.NewServer(srv.Server().Routes())
	t.Cleanup(ts.Close)
	return ts
}

// getAsFixtureUser fetches a page as the fixture owner and parses it.
func getAsFixtureUser(t *testing.T, ts *httptest.Server, path string) *goquery.Document {
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
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d\n%s", path, resp.StatusCode, body)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func testIDSelection(t *testing.T, doc *goquery.Document, id string) *goquery.Selection {
	t.Helper()
	sel := doc.Find(fmt.Sprintf(`[data-testid="%s"]`, id))
	if sel.Length() == 0 {
		t.Errorf("no [data-testid=%s] elements found", id)
	}
	return sel
}

// The wall answers "what is running, in what template, where is it" with no
// clicks: every fixture subject is a card, review subjects carry the ⟡ mark.
func TestComponent_Wall_RendersAllFixtureSubjects(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/")

	cards := testIDSelection(t, doc, "wall-card")
	if got := cards.Length(); got != 3 {
		t.Errorf("wall cards = %d, want 3 (two review PRs + one deterministic subject)", got)
	}
	cards.Each(func(_ int, s *goquery.Selection) {
		if s.AttrOr("data-subject", "") == "" {
			t.Error("wall card without data-subject")
		}
	})
	reviewMarked := doc.Find(`[data-testid="wall-card"][data-review="true"]`).Length()
	if reviewMarked != 2 {
		t.Errorf("review-marked rows = %d, want 2", reviewMarked)
	}
}

// The terminal review attempt renders the full graph (4 nodes) and the
// timing waterfall (overhead + 4 node lanes, agent widest).
func TestComponent_RunDetail_TerminalGraphAndWaterfall(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	nodes := testIDSelection(t, doc, "graph-node")
	if nodes.Length() != 4 {
		t.Errorf("graph nodes = %d, want 4 (prepare, agent, gate, deploy)", nodes.Length())
	}

	lanes := testIDSelection(t, doc, "timing-lane")
	if lanes.Length() != 5 {
		t.Errorf("timing lanes = %d, want 5 (queue+pod + prepare, agent, gate, deploy)", lanes.Length())
	}

	// The agent node's bar must dominate: the 13m agent vs a 5s prepare and a
	// 40s gate. Compare rendered rect widths within each lane.
	widthOf := func(label string) int {
		w := -1
		lanes.Each(func(_ int, s *goquery.Selection) {
			if s.AttrOr("data-label", "") != label {
				return
			}
			s.Find("rect").Each(func(_ int, rect *goquery.Selection) {
				rw, err := parseWidth(rect)
				if err == nil && rw > w {
					w = rw
				}
			})
		})
		return w
	}
	agentW, prepareW, gateW := widthOf("agent"), widthOf("prepare"), widthOf("gate")
	if agentW <= prepareW || agentW <= gateW {
		t.Errorf("agent bar (%d) must be widest; prepare=%d gate=%d", agentW, prepareW, gateW)
	}
}

func parseWidth(s *goquery.Selection) (int, error) {
	var w float64
	if _, err := fmt.Sscanf(s.AttrOr("width", ""), "%f", &w); err != nil {
		return -1, err
	}
	return int(w), nil
}

// The mid-flight attempt shows exactly one running node (with its pulse) and
// the rest pending behind the completed prepare — the live position made
// visible.
func TestComponent_RunDetail_LivePositionOnRunningAttempt(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-43c2")

	nodes := testIDSelection(t, doc, "graph-node")
	byState := map[string]int{}
	nodes.Each(func(_ int, s *goquery.Selection) {
		cls := s.AttrOr("class", "")
		switch {
		case strings.Contains(cls, "rg-node--running"):
			byState["running"]++
		case strings.Contains(cls, "rg-node--ok"):
			byState["ok"]++
		case strings.Contains(cls, "rg-node--pending"):
			byState["pending"]++
		}
	})
	if byState["running"] != 1 || byState["ok"] != 1 || byState["pending"] != 2 {
		t.Errorf("node states = %v, want running:1 ok:1 pending:2", byState)
	}
	if doc.Find(".rg-pulse").Length() != 1 {
		t.Errorf("rg-pulse elements = %d, want 1 (the in-flight node)", doc.Find(".rg-pulse").Length())
	}
	// A mid-flight attempt's waterfall may only show settled work. The
	// invariant: no lane for a node without an envelope — whether the
	// overhead lane renders depends on the attempt's creation gap, which is
	// not this contract.
	unsettled := map[string]bool{"agent": true, "gate": true, "deploy": true}
	doc.Find(`[data-testid="timing-lane"]`).Each(func(_ int, s *goquery.Selection) {
		if label := s.AttrOr("data-label", ""); unsettled[label] {
			t.Errorf("timing lane %q rendered for a mid-flight attempt; only settled nodes may appear", label)
		}
	})
}

// The runs list surfaces all three attempts with their phases, including the
// superseded terminal state.
func TestComponent_RunsList_Phases(t *testing.T) {
	ts := newFixtureServer(t)
	// window=all: the phase-coverage contract must not depend on wall-clock
	// distance to the fixture clock origin.
	doc := getAsFixtureUser(t, ts, "/runs?window=all")

	links := testIDSelection(t, doc, "run-link")
	if links.Length() < 3 {
		t.Errorf("run links = %d, want at least 3", links.Length())
	}
	phases := map[string]bool{}
	links.Each(func(_ int, s *goquery.Selection) {
		phases[s.AttrOr("data-phase", "")] = true
	})
	for _, want := range []string{"validated", "reconciling", "superseded"} {
		if !phases[want] {
			t.Errorf("phase %q missing from runs list (found %v)", want, phases)
		}
	}
}

// The workflow catalog lists both fixture workflows.
func TestComponent_WorkflowsList(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/workflows")
	body := doc.Text()
	for _, name := range []string{"pr-review-demo", "merge-sync-demo"} {
		if !strings.Contains(body, name) {
			t.Errorf("workflow %q not listed", name)
		}
	}
}

// The deterministic merge-sync attempt renders without agent-specific chrome
// (no session link) — the deterministic/review rendering split from the
// prune milestone, now pinned against fixture data.
func TestComponent_RunDetail_DeterministicAttempt(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-merge-sync-demo-e5f6")

	nodes := testIDSelection(t, doc, "graph-node")
	if nodes.Length() != 2 {
		t.Errorf("graph nodes = %d, want 2 (prepare, deploy)", nodes.Length())
	}
	// The pinned contract is AgentEnabled gating the Session link: no anchor
	// may target this attempt's session route.
	sessionRoute := regexp.MustCompile(`^/runs/attempt-merge-sync-demo-e5f6/runs/[^/]+/session$`)
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		if sessionRoute.MatchString(s.AttrOr("href", "")) {
			t.Errorf("deterministic attempt renders a session link to %s; must not", s.AttrOr("href", ""))
		}
	})
}

// Liveness/readiness of the fixture server itself: the healthz route sits
// outside auth and must answer on the fixture world exactly as on a cluster.
func TestComponent_FixtureServer_Healthz(t *testing.T) {
	ts := newFixtureServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

// The default 24h window — what every real visitor's first /runs hit uses —
// must surface the fixture world. This is the regression pin for the fixture
// clock: if base ever becomes a fixed date again, this test goes red one day
// after the commit instead of rotting silently.
func TestComponent_RunsList_DefaultWindow(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs")
	if got := doc.Find(`[data-testid="run-link"]`).Length(); got < 1 {
		t.Errorf("default-window /runs shows %d run links, want at least 1 — fixture clock fell out of the 24h window", got)
	}
}

// The production mount is bare Routes(): an identity-less request must be
// rejected. DevIdentity is a fixture-only wrapper — this test is the guard
// that keeps its auth-bypass nature out of the production path.
func TestComponent_Routes_RejectsAnonymous(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := fixture.NewWorld(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	ts := httptest.NewServer(srv.Server().Routes()) // exactly as production mounts it
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/runs")
	if err != nil {
		t.Fatalf("GET /runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /runs on bare Routes() = %d, want 401", resp.StatusCode)
	}
}

// The `-fixture` zero-setup contract: an unauthenticated request through
// DevIdentity is served as the fixture dev user — the wall renders the
// owner-scoped cards with no headers at all.
func TestComponent_DevIdentity_ZeroSetup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := fixture.NewWorld(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	ts := httptest.NewServer(fixture.DevIdentity(srv.Server().Routes()))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/") // deliberately no identity header
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := doc.Find(`[data-testid="wall-card"]`).Length(); got != 3 {
		t.Errorf("wall cards visible to the injected dev user = %d, want 3", got)
	}
}

// The run detail page exposes the tabbed execution views (ADR-0012 §4): the
// graph tab is the default, the Event Timeline tab lazy-loads its pane, and
// both carry the data-testid contract the e2e tier pins.
func TestComponent_RunDetail_EventTimelineTabs(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	if testIDSelection(t, doc, "graph-tab").Length() != 1 {
		t.Error("graph tab must exist (the default execution view)")
	}
	tab := testIDSelection(t, doc, "event-timeline-tab")
	if tab.Length() != 1 {
		t.Fatal("event timeline tab must exist on run detail")
	}
	if pane := testIDSelection(t, doc, "event-timeline-pane"); pane.Length() != 1 {
		t.Error("event timeline pane must exist (hidden until first activation)")
	}
	href, ok := tab.Attr("hx-get")
	if !ok || href != "/runs/attempt-pr-review-demo-42a1/events" {
		t.Errorf("event timeline tab hx-get = %q (ok=%v), want the fragment route", href, ok)
	}
}

// The events fragment over the fixture world renders the full narrative:
// trigger → claim rows → gate → node/agent rows → terminal, every row with
// the state chip + payload expander contract.
func TestComponent_EventsFragment_FullNarrative(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1/events")

	rows := testIDSelection(t, doc, "timeline-row")
	if rows.Length() < 10 {
		t.Errorf("timeline rows = %d, want the full fixture narrative (≥10)", rows.Length())
	}
	text := doc.Text()
	for _, want := range []string{
		"objective anchored", // ledger trigger row
		"claim armed",        // ledger claim row
		"claim dispatched",   // ledger claim row
		"gate.armed",         // store gate event
		"node.started",       // store node event
		"node.completed",     // store node completion
		"agent turn",         // store agent turn
		"gate.proceed",       // gate verdict transition
		"run.completed",      // terminal
		"verdict posted",     // gate feedback content
	} {
		if !strings.Contains(text, want) {
			t.Errorf("events fragment missing %q", want)
		}
	}
	// Payload expanders exist (store rows carry payloads).
	if testIDSelection(t, doc, "timeline-row-payload").Length() == 0 {
		t.Error("store rows must expose payload expanders")
	}
	// The live attempt's story: in-flight states, not just terminals.
	doc2 := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-43c2/events")
	if !strings.Contains(doc2.Text(), "gate.waiting") {
		t.Error("mid-flight attempt must show gate.waiting")
	}
}

// The Workflow Code island scaffold on template detail (#416): the mount
// point, the serialized document source, and the asset wiring. The editor
// itself is Chromium's job (e2e); here we pin that the page ships everything
// the glue needs — the container, the document, the bundle, the schema key.
func TestComponent_TemplateDetail_CodeIslandScaffold(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")

	island := testIDSelection(t, doc, "code-island")
	if island.Length() != 1 {
		t.Fatal("code island container must exist on template detail")
	}
	if ro, _ := island.Attr("data-readonly"); ro != "true" {
		t.Error("island must mount read-only first (editing lands with #418)")
	}
	src := doc.Find("#code-island-source")
	if src.Length() != 1 {
		t.Fatal("the document source template must be embedded")
	}
	text := src.Text()
	for _, want := range []string{
		"apiVersion: harmostes.dev/v1alpha1",
		"kind: WorkflowTemplate",
		"name: pr-review",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("document YAML missing %q in:\n%s", want, text)
		}
	}
	scripts := doc.Find("script[src*='code-island']")
	if scripts.Length() != 2 {
		t.Errorf("island asset scripts = %d, want 2 (bundle + glue)", scripts.Length())
	}
	// The schema key is the API contract between page and /api/schema —
	// a typo here means silent no-completion, so pin it.
	init := doc.Find("script:not([src])").Text()
	if !strings.Contains(init, "'workflowtemplate'") {
		t.Error("inline init must pass the workflowtemplate schema key")
	}
}
