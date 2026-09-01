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
	srv, err := fixture.NewServer(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
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
	defer resp.Body.Close()
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
	reviewMarked := 0
	doc.Find(`[data-testid="wall-card-title"]`).Each(func(_ int, s *goquery.Selection) {
		if strings.HasPrefix(strings.TrimSpace(s.Text()), "⟡") {
			reviewMarked++
		}
	})
	if reviewMarked != 2 {
		t.Errorf("review-marked titles = %d, want 2", reviewMarked)
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
	// A mid-flight attempt's waterfall may only show settled work: the
	// overhead lane plus the completed prepare — never future nodes.
	lanes := doc.Find(`[data-testid="timing-lane"]`)
	allowed := map[string]bool{"queue+pod": true, "prepare": true}
	lanes.Each(func(_ int, s *goquery.Selection) {
		if label := s.AttrOr("data-label", ""); !allowed[label] {
			t.Errorf("timing lane %q rendered for a mid-flight attempt; settled lanes only", label)
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

// The wall-usage metadata: platform registry must tolerate fixture attempts
// whose bindings are unknown platforms (graceful degradation is part of the
// observe-only contract).
func TestComponent_FixtureServer_Healthz(t *testing.T) {
	ts := newFixtureServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

// The `-fixture` zero-setup contract: an unauthenticated request through
// DevIdentity is served as the fixture dev user — the wall renders the
// owner-scoped cards with no headers at all.
func TestComponent_DevIdentity_ZeroSetup(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := fixture.NewServer(fixtureNamespace, logger)
	if err != nil {
		t.Fatalf("fixture server: %v", err)
	}
	ts := httptest.NewServer(fixture.DevIdentity(srv.Routes()))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/") // deliberately no identity header
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
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
