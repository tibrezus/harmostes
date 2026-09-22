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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
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
	srv, err := fixture.NewWorld(fixtureNamespace, logger, "../../chart")
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
	if got := cards.Length(); got != 4 {
		t.Errorf("wall cards = %d, want 4 (three review PRs + one deterministic subject)", got)
	}
	cards.Each(func(_ int, s *goquery.Selection) {
		if s.AttrOr("data-subject", "") == "" {
			t.Error("wall card without data-subject")
		}
	})
	reviewMarked := doc.Find(`[data-testid="wall-card"][data-review="true"]`).Length()
	if reviewMarked != 3 {
		t.Errorf("review-marked rows = %d, want 3", reviewMarked)
	}

	// #554 organization: the wall is sectioned by template. The fixture
	// world has one template-backed instance (pr-review → pr-review-instance)
	// and two graph-native workflows (no templateRef → "other workflows").
	sections := testIDSelection(t, doc, "wall-section")
	if got := sections.Length(); got != 2 {
		t.Errorf("wall sections = %d, want 2 (pr-review + other workflows)", got)
	}
	prSec := doc.Find(`[data-testid="wall-section"][data-template="pr-review"]`)
	if prSec.Length() != 1 {
		t.Fatalf("pr-review section missing")
	}
	if got := prSec.Find(`[data-testid="wall-card"]`).Length(); got != 1 {
		t.Errorf("pr-review section cards = %d, want 1 (the template-backed instance)", got)
	}
	otherSec := doc.Find(`[data-testid="wall-section"][data-template="other workflows"]`)
	if got := otherSec.Find(`[data-testid="wall-card"]`).Length(); got != 3 {
		t.Errorf("other-workflows section cards = %d, want 3 (two PRs on pr-review-demo + merge-sync)", got)
	}
	// The graph-native workflow tracks two subjects → its block cell
	// spans both rows.
	demoCell := otherSec.Find(`[data-testid="wall-workflow-cell"]`).FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.Find(`[data-testid="wall-workflow-link"]`).Text() == "pr-review-demo"
	})
	if demoCell.Length() != 1 {
		t.Fatalf("pr-review-demo workflow cell missing")
	}
	if got, _ := demoCell.Attr("rowspan"); got != "2" {
		t.Errorf("pr-review-demo cell rowspan = %q, want 2 (two live PRs)", got)
	}
	// Every workflow with envelopes carries a strip; the strips paint with
	// the run-detail waterfall's own state classes.
	strips := testIDSelection(t, doc, "wall-steps")
	if got := strips.Length(); got != 3 {
		t.Errorf("wall step strips = %d, want 3 (every live workflow has a latest attempt)", got)
	}
	if got := doc.Find(`.wall-steps .rg-timing-bar.rg-state-ok`).Length(); got == 0 {
		t.Error("no ok-painted segments — strips must reuse the waterfall palette")
	}
	if doc.Find(`.wall-steps .rg-timing-bar.rg-state-running`).Length() == 0 {
		t.Error("no running segment — the in-flight attempt's agent node must paint live")
	}
	// Segments are in dependency order: within the pr-review-demo strip the
	// prepare segment's x is left of the agent segment's x (4 nodes:
	// prepare → agent → gate → deploy).
	demoStrip := demoCell.Find(`[data-testid="wall-steps"]`)
	if demoStrip.Length() != 1 {
		t.Fatalf("pr-review-demo strip missing")
	}
	xs := []int{}
	demoStrip.Find("rect").Each(func(_ int, s *goquery.Selection) {
		x, _ := s.Attr("x")
		v, err := strconv.Atoi(x)
		if err != nil {
			t.Errorf("segment x %q not an int: %v", x, err)
		}
		xs = append(xs, v)
	})
	sorted := xs[0] < xs[1] && xs[1] < xs[2] && xs[2] < xs[3]
	if len(xs) != 4 || !sorted {
		t.Errorf("pr-review-demo strip segments out of dependency order: %v", xs)
	}
}

// The terminal review attempt renders the full graph (4 nodes) and the
// timing waterfall (node lanes only — one per envelope, agent widest).
func TestComponent_RunDetail_TerminalGraphAndWaterfall(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	nodes := testIDSelection(t, doc, "graph-node")
	if nodes.Length() != 5 {
		t.Errorf("graph nodes = %d, want 5 (trigger + prepare, agent, gate, deploy)", nodes.Length())
	}
	// #541 identity cards: the canvas carries workflow semantics.
	trigger := doc.Find(`[data-testid="graph-node"][data-node="trigger"]`)
	if trigger.Length() != 1 {
		t.Fatal("the virtual trigger node must join the canvas (#541)")
	}
	if trigTxt := strings.TrimSpace(trigger.Text()); !strings.Contains(trigTxt, "webhook") {
		t.Errorf("trigger card must name its kind (webhook), got %q", trigTxt)
	}
	if cause := doc.Find(`[data-testid="trigger-edge"]`); cause.Length() != 1 {
		t.Errorf("cause edges = %d, want 1 (trigger → first root, dashed)", cause.Length())
	}
	// #547 structure: the masthead fact grid replaces the stacked dl rows
	// (claim + objective live at a glance), the ledger renders as tables.
	facts := doc.Find(`[data-testid="fact-grid"] .ds-fact`)
	if facts.Length() < 4 {
		t.Errorf("fact grid cells = %d, want ≥4 (outcome, state, claim, PR)", facts.Length())
	}
	if txt := doc.Find(`[data-testid="fact-grid"]`).Text(); !strings.Contains(txt, "demo-rezuscloud/harmostes#42") {
		t.Errorf("fact grid must carry the claim PR, got %q", txt)
	}
	if txt := doc.Find(`[data-testid="fact-grid"]`).Text(); !strings.Contains(txt, "verdict posted") {
		t.Errorf("fact grid must carry the claim state, got %q", txt)
	}
	runsTable := doc.Find(`[data-testid="runs-table"] tbody tr`)
	if runsTable.Length() != 3 {
		t.Errorf("runs table rows = %d, want 3 (fixture attempt runs)", runsTable.Length())
	}
	if txt := runsTable.Text(); !strings.Contains(txt, "13.1m") {
		t.Errorf("runs table must carry humanized run durations (the agent run's 13m05s wall span), got %q", txt)
	}
	if n := doc.Find(`[data-testid="runs-table"] a[href*="/session"]`); n.Length() != 3 {
		t.Errorf("per-run session links = %d, want 3 (agent-enabled attempt)", n.Length())
	}
	// Log drawer (#551): the open buttons TARGET the full-width drawer below
	// the table — logs must never render inside the ledger's action cell.
	if n := doc.Find(`[data-testid="runs-table"] button[hx-target="#runlogs-drawer"]`); n.Length() != 3 {
		t.Errorf("log open buttons targeting #runlogs-drawer = %d, want 3", n.Length())
	}
	drawer := doc.Find(`[data-testid="runlogs-drawer"]`)
	if drawer.Length() != 1 {
		t.Fatalf("log drawer container = %d, want exactly 1 (below the runs table)", drawer.Length())
	}
	if inCell := doc.Find(`[data-testid="runs-table"] td [data-testid="runlogs-drawer"]`); inCell.Length() != 0 {
		t.Errorf("log drawer must live OUTSIDE the runs table cells")
	}
	drawerY, _ := drawer.Attr("class")
	if !strings.Contains(drawerY, "runlogs-drawer") {
		t.Errorf("drawer missing runlogs-drawer class")
	}
	res := doc.Find(`[data-testid="noderes-table"] tbody tr`)
	if res.Length() != 4 {
		t.Errorf("node-results rows = %d, want 4", res.Length())
	}
	if txt := res.Text(); !strings.Contains(txt, "13.0m") {
		t.Errorf("node results must carry humanized envelope durations, got %q", txt)
	}
	agentCardText := strings.TrimSpace(doc.Find(`[data-testid="graph-node"][data-node="agent"]`).Text())
	if txt := agentCardText; !strings.Contains(txt, "maxFixes 3") {
		t.Errorf("agent card must carry the fix-loop budget (maxFixes 3), got %q", txt)
	}
	if txt := agentCardText; !strings.Contains(txt, "gate pr-review") {
		t.Errorf("agent card must name its gate plugin, got %q", txt)
	}

	lanes := testIDSelection(t, doc, "timing-lane")
	if lanes.Length() != 4 {
		t.Errorf("timing lanes = %d, want 4 (prepare, agent, gate, deploy — no overhead lane since #557)", lanes.Length())
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
	// A mid-flight attempt's waterfall shows EXACTLY the settled work: one
	// lane per envelope, nothing else. The fixture (attempt-...-43c2) has
	// one envelope (prepare/ok); agent/gate/deploy have none and must not
	// appear — positive pins, because a pass-on-nothing loop cannot catch a
	// lane regrowing (r34 round 2, finding 2).
	lanes := testIDSelection(t, doc, "timing-lane")
	if lanes.Length() != 1 {
		t.Errorf("timing lanes = %d, want 1 (prepare only — one envelope in the fixture)", lanes.Length())
	}
	if lanes.AttrOr("data-label", "") != "prepare" {
		t.Errorf("timing lane label = %q, want prepare", lanes.AttrOr("data-label", ""))
	}
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
	if nodes.Length() != 3 {
		t.Errorf("graph nodes = %d, want 3 (trigger + prepare, deploy)", nodes.Length())
	}
	// The deterministic attempt's trigger card is its schedule (the fixture
	// merge-sync-demo source is kind: schedule, cron 0 */6 * * *).
	trigger := doc.Find(`[data-testid="graph-node"][data-node="trigger"]`)
	if txt := strings.TrimSpace(trigger.Text()); !strings.Contains(txt, "cron 0 */6 * * *") {
		t.Errorf("schedule trigger card must show its cron expression, got %q", txt)
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
	srv, err := fixture.NewWorld(fixtureNamespace, logger, "../../chart")
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
	srv, err := fixture.NewWorld(fixtureNamespace, logger, "../../chart")
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
	if got := doc.Find(`[data-testid="wall-card"]`).Length(); got != 4 {
		t.Errorf("wall cards visible to the injected dev user = %d, want 4", got)
	}
}

// The run detail composes the Temporal band (#533, ADR-0012 §4 amended):
// Workflow Code and Event Timeline side by side under the header, both
// server-rendered on first paint — no tabs, no lazy pane — with the
// Execution Graph as its own section below. The tabs are GONE: this pins
// the composition the e2e tier drives.
func TestComponent_RunDetail_CodeTimelineBand(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	// Absence checks go through doc.Find directly — testIDSelection treats
	// not-found as a failure of the lookup itself.
	if doc.Find(`[data-testid="graph-tab"]`).Length() != 0 {
		t.Error("graph tab must be gone (the band replaced the tabs)")
	}
	if doc.Find(`[data-testid="event-timeline-tab"]`).Length() != 0 {
		t.Error("event timeline tab must be gone (the pane is first-render)")
	}

	code := testIDSelection(t, doc, "workflow-code-pane")
	if code.Length() != 1 {
		t.Fatal("workflow code pane must exist on run detail")
	}
	if island := doc.Find("#code-island"); island.Length() != 1 {
		t.Error("the code island must mount inside the code pane")
	} else if ro, ok := island.Attr("data-readonly"); !ok || ro != "true" {
		t.Errorf("run-detail island data-readonly = %q (ok=%v), want true", ro, ok)
	}

	// The pane carries the initial server-rendered narrative: the same
	// fragment the SSE stream re-renders (reload equals live from byte one).
	pane := testIDSelection(t, doc, "event-timeline-pane")
	if pane.Length() != 1 {
		t.Fatal("event timeline pane must exist on run detail")
	}
	if _, hidden := pane.Attr("hidden"); hidden {
		t.Error("event timeline pane must be visible on first paint (not hidden)")
	}
	if rows := pane.Find("[data-testid=\"timeline-row\"]").Length(); rows != 21 {
		t.Errorf("initial timeline rows = %d, want 21 (the fixture narrative, server-rendered)", rows)
	}

	if testIDSelection(t, doc, "run-graph-section").Length() != 1 {
		t.Error("execution graph section must exist below the band")
	}
}

// The Workflow Code pane shows the run's RESOLVED document (#533): identity
// + spec in the canonical document shape — kind Workflow, the live CR's
// name, and the compiled graph of the fixture's pr-review-demo.
func TestComponent_RunDetail_WorkflowCodeDocument(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	src := doc.Find("#code-island-source")
	if src.Length() != 1 {
		t.Fatal("code island source template must exist")
	}
	yamlDoc := src.Text()
	for _, want := range []string{
		"apiVersion: harmostes.dev/v1alpha1",
		"kind: Workflow",
		"name: pr-review-demo",
	} {
		if !strings.Contains(yamlDoc, want) {
			t.Errorf("resolved workflow document missing %q", want)
		}
	}
	// Graph-native fixture: the resolved spec carries the explicit graph —
	// the exact shape the worker compiled for this attempt.
	if !strings.Contains(yamlDoc, "prepare") || !strings.Contains(yamlDoc, "deploy") {
		t.Error("resolved document must carry the workflow's graph nodes (what this run executed)")
	}
	// Document discipline: status never enters the document form.
	if strings.Contains(yamlDoc, "status:") {
		t.Error("status must not render in the workflow document (live state lives in the timeline/graph panes)")
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

// #538: the template library is a table (principle 1) with the instance
// count per template and the creation CTA; the workflows catalog groups
// speak template vocabulary with the gate demoted to meta.
func TestComponent_TemplateLibraryTable(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/templates")

	table := doc.Find("[data-testid=\"tpl-table\"]")
	if table.Length() != 1 {
		t.Fatal("the template library must be a table")
	}
	rows := table.Find("tbody tr")
	if rows.Length() < 1 {
		t.Errorf("template rows = %d, want ≥1 (the fixture world ships the chart's pr-review)", rows.Length())
	}
	first := rows.First()
	if link := first.Find("[data-testid=\"tpl-row-link\"]"); link.Length() != 1 {
		t.Error("each row links its template")
	} else if href, _ := link.Attr("href"); href != "/templates/pr-review" {
		t.Errorf("first row links %q, want /templates/pr-review (alphabetical)", href)
	}
	if cta := first.Find("[data-testid=\"tpl-new-workflow\"]"); cta.Length() != 1 {
		t.Error("the creation CTA rides every row (creation starts from the definition)")
	} else if href, _ := cta.Attr("href"); href != "/workflows/new?template=pr-review" {
		t.Errorf("CTA href = %q, want the pre-filled template", href)
	}
	if first.Find(".pg-mini-node").Length() == 0 {
		t.Error("the pipeline shape renders in the row")
	}
	// The fixture world's pr-review-instance composes from pr-review —
	// the count column is the templateRef census, not gate-name keying.
	if !strings.Contains(first.Text(), "1 active") {
		t.Errorf("row must show the instance census (pr-review: 1 active), got %q", strings.TrimSpace(first.Find("td").Eq(2).Text()))
	}
	if doc.Find(".tpl-card").Length() != 0 {
		t.Error("the tpl-card grid is retired")
	}
}

func TestComponent_WorkflowsGroupsSpeakTemplates(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/workflows")

	// The group title LINKS the template it groups by — the relationship
	// the page exists to show.
	title := doc.Find(".gate-group-title a").First()
	if title.Length() != 1 {
		t.Fatal("group titles must link their template")
	}
	if href, _ := title.Attr("href"); !strings.Contains(href, "/templates/") {
		t.Errorf("group title href = %q, want /templates/<name>", href)
	}
	meta := doc.Find(".gate-group-gatename").First().Text()
	if !strings.Contains(meta, "template") {
		t.Errorf("group meta = %q, want template vocabulary (gate demoted to meta)", meta)
	}
	if doc.Find(".page-tab").Length() != 0 {
		t.Error("the in-page tab pair is gone (the nav owns Workflows|Templates)")
	}
}

// The gate envelope of the terminal demo attempt carries attempt=2 — the
// fixture's transient-retry surfacing (ADR-0012 §9): the panel JSON must
// carry the Attempts field and the waterfall title must name the retry.
func TestComponent_RunDetail_TransientRetrySurfacing(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/runs/attempt-pr-review-demo-42a1")

	// The panel data ships as JSON in #run-graph-data-initial (the inline
	// script the hover/click handlers read).
	data := doc.Find("#run-graph-data-initial")
	if data.Length() != 1 {
		t.Fatal("run graph initial data must exist")
	}
	// NodeDataJSON marshals the NodeData map directly (no wrapper key).
	var nodes map[string]struct {
		Attempts int    `json:"attempts"`
		Duration string `json:"duration"`
	}
	if err := json.Unmarshal([]byte(data.Text()), &nodes); err != nil {
		t.Fatalf("decode run graph data: %v", err)
	}
	gate, ok := nodes["gate"]
	if !ok {
		t.Fatal("gate node missing from panel data")
	}
	if gate.Attempts != 2 {
		t.Errorf("gate attempts = %d, want 2 (fixture's retried envelope)", gate.Attempts)
	}

	// The waterfall title names the retry — scanner-first placement. The
	// title renders as an SVG <title> child of the bar rect (hover text).
	var retryTitle string
	doc.Find(`[data-testid="timing-lane"] rect > title`).Each(func(_ int, s *goquery.Selection) {
		if txt := s.Text(); strings.Contains(txt, "retry ×2") {
			retryTitle = txt
		}
	})
	if retryTitle == "" {
		t.Error("waterfall must carry a 'retry ×2' title for the retried gate segment")
	} else if !strings.Contains(retryTitle, "40.0s") {
		t.Errorf("retry title = %q, want it to keep the duration", retryTitle)
	}
}
