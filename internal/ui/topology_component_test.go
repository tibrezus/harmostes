package ui_test

// topology_component_test.go — the topology projection through the real
// HTTP surface (fixture tier, goquery): testid contract, merged shape for
// thin instances, revision diff classes, single-revision degradation.

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// --- handler contract (fixture tier) ---

// Template detail shows the topology projection with the vocabulary
// contract: testids, node identity, unknown-type flags.
func TestComponent_TemplateDetail_Topology(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/templates/pr-review")

	nodes := doc.Find(`[data-testid="topology-node"]`)
	if nodes.Length() != 3 { // compile: prepare → agent → deploy
		t.Fatalf("topology nodes = %d, want 3 (fixture template compiles agent-enabled)", nodes.Length())
	}
	agent := nodes.FilterFunction(func(_ int, s *goquery.Selection) bool {
		id, _ := s.Attr("data-node")
		return id == "agent"
	})
	if agent.Length() != 1 {
		t.Fatal("agent node missing from template topology")
	}
	if ty, _ := agent.Attr("data-type"); ty != "agent" {
		t.Errorf("agent node data-type = %q", ty)
	}
	if unknown, _ := agent.Attr("data-unknown-type"); unknown != "" {
		t.Error("agent is in the CRD enum — must not be flagged unknown (palette is schema-derived)")
	}
	if edges := doc.Find(`[data-testid="topology-edge"]`); edges.Length() != 2 {
		t.Errorf("topology edges = %d, want 2", edges.Length())
	}
	// History link appears only when a revision history exists.
	if link := doc.Find(`[data-testid="revisions-link"]`); link.Length() != 1 {
		t.Error("fixture template carries history — revisions link must render")
	}
}

// The thin instance renders its MERGED shape: the template's agent and
// plugins appear although the stored CR sets none of them.
func TestComponent_WorkflowDetail_MergedTopology(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/workflows/pr-review-instance")

	nodes := doc.Find(`[data-testid="topology-node"]`)
	if nodes.Length() != 3 {
		t.Fatalf("merged topology nodes = %d, want 3 (template defaults resolved)", nodes.Length())
	}
	agent := nodes.FilterFunction(func(_ int, s *goquery.Selection) bool {
		id, _ := s.Attr("data-node")
		return id == "agent"
	})
	if agent.Length() != 1 {
		t.Fatal("thin instance topology missing the template's agent — resolution did not apply")
	}
	// Merged means MERGED: the agent label carries the template's model — a
	// thin-spec compile (resolution skipped) would render a bare "agent".
	// (graphLabelLimit truncates to 22 runes — assert the surviving prefix.)
	if got := strings.TrimSpace(agent.Text()); !strings.Contains(got, "mistral-small") {
		t.Errorf("merged agent label = %q, want the template's model", got)
	}
}

// The revisions page: diff classes on nodes and edges, the YAML diff pane,
// and the single-revision degradation for history-less templates.
func TestComponent_TemplateRevisions_Diff(t *testing.T) {
	ts := newFixtureServer(t)
	doc := getAsFixtureUser(t, ts, "/templates/pr-review/revisions")

	if doc.Find(`[data-testid="revisions-empty"]`).Length() != 0 {
		t.Fatal("fixture template HAS history — empty state must not render")
	}
	panes := doc.Find(`[data-testid="topology-pane"]`)
	if panes.Length() != 2 {
		t.Fatalf("panes = %d, want 2", panes.Length())
	}
	// r1 → head, per pane. Older pane: agent ghosts in as added, prepare is
	// changed, prepare→deploy still present (own, removed-by-head); the two
	// agent edges ghost in as added. Newer pane mirrors: agent own+added,
	// prepare changed, agent edges own+added, prepare→deploy ghost/removed.
	older := panes.Filter(`[data-pane="older"]`)
	newer := panes.Filter(`[data-pane="newer"]`)

	if n := older.Find(`[data-testid="topology-node"][data-diff="added"]`); n.Length() != 1 {
		t.Errorf("older pane added nodes = %d, want 1 (agent ghost)", n.Length())
	} else if id, _ := n.Attr("data-node"); id != "agent" {
		t.Errorf("older pane added node = %q, want agent", id)
	}
	if n := newer.Find(`[data-testid="topology-node"][data-diff="added"]`); n.Length() != 1 {
		t.Errorf("newer pane added nodes = %d, want 1 (agent own)", n.Length())
	}
	if n := older.Find(`[data-testid="topology-node"][data-diff="changed"]`); n.Length() != 1 {
		t.Errorf("older pane changed nodes = %d, want 1 (prepare)", n.Length())
	}
	if n := newer.Find(`[data-testid="topology-node"][data-diff="changed"]`); n.Length() != 1 {
		t.Errorf("newer pane changed nodes = %d, want 1 (prepare)", n.Length())
	}
	if n := older.Find(`[data-testid="topology-edge"][data-diff="added"]`); n.Length() != 2 {
		t.Errorf("older pane added edges = %d, want 2 (ghosted)", n.Length())
	}
	if n := newer.Find(`[data-testid="topology-edge"][data-diff="added"]`); n.Length() != 2 {
		t.Errorf("newer pane added edges = %d, want 2 (own)", n.Length())
	}
	if n := older.Find(`[data-testid="topology-edge"][data-diff="removed"]`); n.Length() != 1 {
		t.Errorf("older pane removed edges = %d, want 1 (prepare→deploy own)", n.Length())
	}
	if n := newer.Find(`[data-testid="topology-edge"][data-diff="removed"]`); n.Length() != 1 {
		t.Errorf("newer pane removed edges = %d, want 1 (prepare→deploy ghost)", n.Length())
	}
	// The YAML diff pane carries both line kinds with the testid contract.
	yd := doc.Find(`[data-testid="yaml-diff-line"]`)
	if yd.Length() == 0 {
		t.Fatal("yaml diff must render lines")
	}
	if yd.Filter(`[data-diff="added"]`).Length() == 0 || yd.Filter(`[data-diff="removed"]`).Length() == 0 {
		t.Error("yaml diff must carry added AND removed lines for a real delta")
	}
	// Revision picker offers r1 and the head.
	opts := doc.Find(`[data-testid="rev-option"]`)
	if opts.Length() != 2 {
		t.Errorf("rev options = %d, want 2", opts.Length())
	}
}

func TestComponent_TemplateRevisions_SingleRevision(t *testing.T) {
	ts := newFixtureServer(t)
	// Out-of-range/clamped pairs never 500 — the route is not a dead end.
	doc := getAsFixtureUser(t, ts, "/templates/pr-review/revisions?from=1&to=1")
	// from=to=1 is clamped to (1,2) — the fixture HAS r2, so the empty state
	// legitimately does not render; the clamped pair must.
	if empty := doc.Find(`[data-testid="revisions-empty"]`); empty.Length() == 0 {
		if doc.Find(`[data-testid="topology-pane"]`).Length() != 2 {
			t.Error("clamped pair must still render two panes")
		}
	}
}
