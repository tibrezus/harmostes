package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// ---------------------------------------------------------------------------
// Identity cards (#541): the canvas carries workflow semantics — what
// triggers this, what the deterministic logic does, what the agent does.
// Identity lines ride ON the card; full config lives behind the click
// (run graph: the enriched #rg-panel; topology: the #419 inspector).
//
// One facts path for both projections: every card is derived from the
// compiled NodeSpec itself (its Config JSON carries model/skill/gate/
// maxFixes for agents, plugin args for prepare/deploy), so the run graph
// and the topology can never disagree about what a node IS. The trigger
// is a synthetic node in the UI projection only (cause, not step — the
// kernel never sees it; ADR-0012 §3 amendment).
// ---------------------------------------------------------------------------

// Card geometry: richer than the old 168×40 label box. Both projections
// share these constants with the layout engine (topology.go).
const (
	cardTitleLimit = 26 // chars, per zone (SVG text does not wrap)
	cardFactLimit  = 36
)

// triggerNodeID is the synthetic root node's ID.
const triggerNodeID = "trigger"

// nodeCard is the identity payload rendered on the card.
type nodeCard struct {
	Chip  string   // type chip: source|prepare|agent|deploy|…
	Title string   // the headline (plugin name, model, trigger kind)
	Facts []string // 0–2 identity lines
}

// defRow is one label→value pair for the panel's definition block — the
// same facts, spelled out (click carries the config).
type defRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// triggerNodeConfig rides the synthetic trigger node's Config so the
// standard NodeSpec → card path serves the trigger like any other node.
type triggerNodeConfig struct {
	Kind     string `json:"kind"`
	Schedule string `json:"schedule,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Topic    string `json:"topic,omitempty"`
	ForkURL  string `json:"forkUrl,omitempty"`
	ForkBr   string `json:"forkBranch,omitempty"`
}

// withTriggerNode prepends the synthetic trigger node (UI projection
// only) and a dashed cause-edge into every root. Compile graphs are
// unchanged — the kernel never sees the trigger; this is display.
func withTriggerNode(gs v1alpha1.GraphSpec, src v1alpha1.SourceSpec) v1alpha1.GraphSpec {
	if len(gs.Nodes) == 0 {
		return gs
	}
	tcfg := triggerNodeConfig{
		Kind:     src.Kind,
		Schedule: src.Schedule,
		Repo:     src.Repo,
		Branch:   src.Branch,
		Topic:    src.Topic,
	}
	if src.Fork != nil {
		tcfg.ForkURL = src.Fork.URL
		tcfg.ForkBr = src.Fork.Branch
	}
	cfg, _ := json.Marshal(tcfg)
	roots := map[string]bool{}
	for _, n := range gs.Nodes {
		roots[n.ID] = true
	}
	for _, e := range gs.Edges {
		delete(roots, e.To)
	}
	rootIDs := make([]string, 0, len(roots))
	for id := range roots {
		rootIDs = append(rootIDs, id)
	}
	sort.Strings(rootIDs) // deterministic edges
	if len(rootIDs) == 0 {
		return gs // cycle-guard: graph with no roots cannot carry a cause
	}
	nodes := append([]v1alpha1.NodeSpec{{
		ID:     triggerNodeID,
		Type:   "source",
		Label:  src.Kind,
		Config: cfg,
	}}, gs.Nodes...)
	edges := make([]v1alpha1.EdgeSpec, 0, len(gs.Edges)+len(rootIDs))
	for _, id := range rootIDs {
		edges = append(edges, v1alpha1.EdgeSpec{From: triggerNodeID, To: id})
	}
	return v1alpha1.GraphSpec{Nodes: nodes, Edges: append(edges, gs.Edges...)}
}

// cardFacts derives the identity card from the compiled node itself.
// spec is the optional fallback: hand-written explicit graphs carry no
// node Config, but the workflow spec still names the model/skill/gate —
// identity must not go blank just because the graph was written by hand.
func cardFacts(n v1alpha1.NodeSpec, spec *v1alpha1.WorkflowSpec) nodeCard {
	switch {
	case n.Type == "source":
		return triggerCard(n)
	case n.Type == "agent":
		card := agentCard(n)
		if spec != nil && card.Title == "agent" && spec.Agent.Model != "" {
			card.Title = truncateRunes(spec.Agent.Model, cardTitleLimit)
			card = agentSpecFacts(card, spec)
		}
		return card
	case n.Type == "gate" || n.ID == "gate":
		card := pluginCard(n)
		if spec != nil && len(n.Config) == 0 && spec.Agent.Gate.Plugin.Name != "" {
			card.Title = truncateRunes(spec.Agent.Gate.Plugin.Name, cardTitleLimit)
		}
		return card
	case n.Type == "plugin":
		card := pluginCard(n)
		if spec != nil && len(n.Config) == 0 {
			switch n.ID {
			case "prepare":
				if spec.Prepare.Plugin.Name != "" {
					card.Title = truncateRunes(spec.Prepare.Plugin.Name, cardTitleLimit)
				}
				if spec.Prepare.Detect != "" {
					card.Facts = append(card.Facts, truncateRunes("detect "+spec.Prepare.Detect, cardFactLimit))
				}
			case "deploy":
				if spec.Deploy.Plugin.Name != "" {
					card.Title = truncateRunes(spec.Deploy.Plugin.Name, cardTitleLimit)
				}
			}
		}
		return card
	default:
		title := n.Label
		if title == "" {
			title = n.ID
		}
		return nodeCard{Chip: n.Type, Title: truncateRunes(title, cardTitleLimit)}
	}
}

// agentSpecFacts fills the card's fact lines from the workflow spec when
// the node carries no compiled config (explicit hand-written graphs).
func agentSpecFacts(card nodeCard, spec *v1alpha1.WorkflowSpec) nodeCard {
	if spec == nil {
		return card
	}
	if spec.Agent.Skill != "" && len(card.Facts) == 0 {
		card.Facts = append(card.Facts, truncateRunes("skill "+path.Base(spec.Agent.Skill), cardFactLimit))
	}
	detail := ""
	if spec.Agent.Gate.Plugin.Name != "" {
		detail = "gate " + spec.Agent.Gate.Plugin.Name
	}
	if spec.Agent.MaxFixes > 0 {
		if detail != "" {
			detail += " · "
		}
		detail += fmt.Sprintf("maxFixes %d", spec.Agent.MaxFixes)
	}
	if detail != "" {
		card.Facts = append(card.Facts, truncateRunes(detail, cardFactLimit))
	}
	if len(spec.Agent.Models) > 0 && len(card.Facts) < 2 {
		card.Facts = append(card.Facts, truncateRunes(fmt.Sprintf("⏱ %d model window(s)", len(spec.Agent.Models)), cardFactLimit))
	}
	return card
}

func triggerCard(n v1alpha1.NodeSpec) nodeCard {
	var cfg triggerNodeConfig
	_ = json.Unmarshal(n.Config, &cfg)
	title := cfg.Kind + " trigger"
	if cfg.Kind == "" {
		title = "trigger"
	}
	var facts []string
	add := func(s string) {
		if s != "" && len(facts) < 2 {
			facts = append(facts, truncateRunes(s, cardFactLimit))
		}
	}
	switch cfg.Kind {
	case "schedule":
		if cfg.Schedule != "" {
			add("cron " + cfg.Schedule)
		}
	case "webhook":
		add("push events")
		add(sourceTarget(cfg))
	case "event":
		add("topic " + cfg.Topic)
	default:
		add(sourceTarget(cfg))
	}
	return nodeCard{Chip: "source", Title: truncateRunes(title, cardTitleLimit), Facts: facts}
}

// sourceTarget renders the repo@branch / fork target common to git-ish kinds.
func sourceTarget(cfg triggerNodeConfig) string {
	switch {
	case cfg.ForkURL != "":
		target := cfg.ForkURL
		if cfg.ForkBr != "" {
			target += "#" + cfg.ForkBr
		}
		return target
	case cfg.Repo != "":
		target := cfg.Repo
		if cfg.Branch != "" {
			target += "@" + cfg.Branch
		}
		return target
	default:
		return ""
	}
}

func agentCard(n v1alpha1.NodeSpec) nodeCard {
	var cfg graph.AgentNodeConfig
	_ = json.Unmarshal(n.Config, &cfg)
	title := cfg.Model
	if title == "" {
		title = "agent"
	}
	card := nodeCard{Chip: "agent", Title: truncateRunes(title, cardTitleLimit)}
	if cfg.Skill != "" {
		card.Facts = append(card.Facts, truncateRunes("skill "+path.Base(cfg.Skill), cardFactLimit))
	}
	detail := ""
	if cfg.Gate != nil && cfg.Gate.Plugin.Name != "" {
		detail = "gate " + cfg.Gate.Plugin.Name
	}
	if cfg.MaxFixes > 0 {
		if detail != "" {
			detail += " · "
		}
		detail += fmt.Sprintf("maxFixes %d", cfg.MaxFixes)
	}
	if detail != "" {
		card.Facts = append(card.Facts, truncateRunes(detail, cardFactLimit))
	}
	return card
}

func pluginCard(n v1alpha1.NodeSpec) nodeCard {
	title := n.Label
	if title == "" {
		title = n.ID
	}
	// A gate node wraps a plugin (exit 0 = green, stderr = feedback):
	// its card names the gate plugin and its claim scope.
	var gate graph.GateNodeConfig
	if n.Type == "gate" || n.ID == "gate" {
		if json.Unmarshal(n.Config, &gate) == nil && gate.Plugin.Name != "" {
			card := nodeCard{Chip: "gate", Title: truncateRunes(gate.Plugin.Name, cardTitleLimit)}
			if len(gate.Validates) > 0 {
				card.Facts = append(card.Facts, truncateRunes(
					fmt.Sprintf("validates %d claim scope(s)", len(gate.Validates)), cardFactLimit))
			}
			return card
		}
		return nodeCard{Chip: "gate", Title: truncateRunes(title, cardTitleLimit)}
	}
	var cfg graph.PluginNodeConfig
	_ = json.Unmarshal(n.Config, &cfg)
	chip := n.ID // prepare/deploy — the step's role, not just "plugin"
	card := nodeCard{Chip: chip, Title: truncateRunes(title, cardTitleLimit)}
	if len(cfg.Args) > 0 {
		card.Facts = append(card.Facts, truncateRunes("args "+joinArgs(cfg.Args), cardFactLimit))
	}
	return card
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

// cardDefs spells the card out as panel rows — the click half of the
// identity/config split. Derived from the same cardFacts path so the
// panel and the canvas can never disagree.
func cardDefs(n v1alpha1.NodeSpec, spec *v1alpha1.WorkflowSpec) []defRow {
	c := cardFacts(n, spec)
	var defs []defRow
	if c.Title != "" && c.Title != n.ID {
		defs = append(defs, defRow{Label: c.Chip, Value: c.Title})
	}
	for i, f := range c.Facts {
		label := "Detail"
		if i == 0 {
			label = "Identity"
		}
		defs = append(defs, defRow{Label: label, Value: f})
	}
	return defs
}

// cardAnchors precomputes the card's text/element coordinates for a node
// box at (x, y) — the templates stay arithmetic-free by contract.
type cardAnchors struct {
	BarX, BarY, BarH int // status accent bar (left edge, full height)
	ChipX, ChipY     int
	TitleX, TitleY   int
	Fact1X, Fact1Y   int
	Fact2X, Fact2Y   int
	StatX, StatY     int // runtime status chip (run graph only)
	PulseX, PulseY   int
}

func cardAnchorsAt(x, y int) cardAnchors {
	return cardAnchors{
		BarX:   x,
		BarY:   y + 4,
		BarH:   graphNodeH - 8,
		ChipX:  x + 14,
		ChipY:  y + 21,
		TitleX: x + 14,
		TitleY: y + 45,
		Fact1X: x + 14,
		Fact1Y: y + 64,
		Fact2X: x + 14,
		Fact2Y: y + 81,
		StatX:  x + graphNodeW - 12,
		StatY:  y + 21,
		PulseX: x + graphNodeW - 16,
		PulseY: y + graphNodeH - 14,
	}
}

// runSessionModel reads the run's session metadata for the window-resolved
// model (#494): the worker pins the model in-memory only, but session meta
// persists the run's actual model. Best-effort — the card's identity is
// the spec; this is the runtime truth for the panel.
func (s *Server) runSessionModel(ctx context.Context, workflowRef, run string) string {
	if s.dapr == nil || run == "" {
		return ""
	}
	var meta struct {
		Model string `json:"model"` // SessionRecord.Model — the pinned truth
	}
	found, err := s.dapr.GetStateFromStore(ctx, "statestore",
		fmt.Sprintf("%s:%s:session", workflowCRName(workflowRef), run), &meta)
	if err != nil || !found {
		return ""
	}
	return meta.Model
}
