package ui

import (
	"net/http"
	"sort"
)

// ---------------------------------------------------------------------------
// Gate Catalog — the gate IS the workflow archetype
// ---------------------------------------------------------------------------
//
// A gate defines the entire workflow structure: which prepare plugin produces
// the artifact, which skill guides the agent, which deploy plugin ships the
// result, and of course which gate plugin validates it. The gate is not just
// "one field among many" — it is the organizing principle.
//
// Each gate archetype specifies a COMMON STRUCTURE that all workflows using
// that gate share. The workflow author picks a gate, then provides only the
// target repo. The structure is determined by the gate.
//
// Current gates:
//
//   wiki-lint        Documentation sync (C4/RIG → agent → lint → push)
//   review-validate  PR review (fetch PR → agent → validate → post review)
//   fork-resolved    Fork maintenance (cherry-pick → agent → resolve → deploy)
//   noop             Passthrough (deterministic-only, no LLM validation)

// GateArchetype defines the common structure for all workflows using a gate.
type GateArchetype struct {
	// Name is the gate plugin name (matches spec.agent.gate.plugin.name).
	Name string `json:"name"`

	// Label is the human-readable gate name for the UI.
	Label string `json:"label"`

	// Description explains what the gate validates.
	Description string `json:"description"`

	// Category groups gates in the UI.
	Category string `json:"category"`

	// Common structure — all workflows using this gate share these defaults.
	PreparePlugin string `json:"preparePlugin"` // the prepare plugin name
	DeployPlugin  string `json:"deployPlugin"`  // the deploy plugin name
	SkillPath     string `json:"skillPath"`     // path to SKILL.md
	TaskName      string `json:"taskName"`      // task template name

	// Gate contract.
	ExitGreen  int    `json:"exitGreen"`  // exit code for green (always 0)
	ExitFailed int    `json:"exitFailed"` // exit code for failed (always 1)
	Feedback   string `json:"feedback"`   // where the agent reads feedback ("stderr")
}

// gateCatalog is the built-in registry of known gates. Adding a new gate type
// means adding an entry here — the UI, API, and workflow creation form all
// derive from this catalog.
var gateCatalog = []GateArchetype{
	{
		Name:          "wiki-lint",
		Label:         "Documentation Sync",
		Description:   "Source → RIG extraction → agent writes C4 docs → wiki-lint validates → push to wiki repo",
		Category:      "documentation",
		PreparePlugin: "rig-emit",
		DeployPlugin:  "git-push",
		SkillPath:     "/skills/wiki/SKILL.md",
		TaskName:      "arch-sync",
		ExitGreen:     0,
		ExitFailed:    1,
		Feedback:      "stderr",
	},
	{
		Name:          "review-validate",
		Label:         "PR Review",
		Description:   "Fetch labeled PR → agent reviews → review-validate checks structure → post review to git host",
		Category:      "code-review",
		PreparePlugin: "pr-fetch",
		DeployPlugin:  "post-review",
		SkillPath:     "/skills/pr-review/SKILL.md",
		TaskName:      "pr-review",
		ExitGreen:     0,
		ExitFailed:    1,
		Feedback:      "stderr",
	},
	{
		Name:          "fork-resolved",
		Label:         "Fork Maintenance",
		Description:   "Upstream sync → cherry-pick replay → agent resolves conflicts → fork-resolved validates build → deploy",
		Category:      "fork-maintenance",
		PreparePlugin: "cherry-pick-sync",
		DeployPlugin:  "fork-replace-deploy",
		SkillPath:     "/skills/fork-maintenance/SKILL.md",
		TaskName:      "resolve-conflict",
		ExitGreen:     0,
		ExitFailed:    1,
		Feedback:      "stderr",
	},
	{
		Name:          "noop",
		Label:         "Passthrough",
		Description:   "Deterministic-only: no LLM agent, no validation gate. Prepare runs, deploy pushes.",
		Category:      "passthrough",
		PreparePlugin: "rig-emit",
		DeployPlugin:  "git-push",
		SkillPath:     "",
		TaskName:      "",
		ExitGreen:     0,
		ExitFailed:    1,
		Feedback:      "",
	},
}

// gateByName returns the GateArchetype for the given gate plugin name, or nil.
func gateByName(name string) *GateArchetype {
	for i := range gateCatalog {
		if gateCatalog[i].Name == name {
			return &gateCatalog[i]
		}
	}
	return nil
}

// gateCategoryLabel maps a category key to a human-readable label with an icon.
func gateCategoryLabel(category string) string {
	switch category {
	case "documentation":
		return "📚 Documentation"
	case "code-review":
		return "🔍 Code Review"
	case "fork-maintenance":
		return "🔀 Fork Maintenance"
	case "passthrough":
		return "⚙️ Passthrough"
	default:
		return "📋 Other"
	}
}

// workflowGate derives the gate name from a Workflow CR's spec.
// Returns the gate plugin name, or "unknown" if not set.
func workflowGate(gatePluginName string) string {
	if gatePluginName == "" {
		return "noop"
	}
	return gatePluginName
}

// ---------------------------------------------------------------------------
// API: GET /api/gates — returns the gate catalog
// ---------------------------------------------------------------------------

// handleGateAPIList returns the gate catalog as JSON. The SPA and the
// server-rendered UI both use this to display gate metadata.
func (s *Server) handleGateAPIList(w http.ResponseWriter, r *http.Request) {
	gates := make([]GateArchetype, len(gateCatalog))
	copy(gates, gateCatalog)
	sort.Slice(gates, func(i, j int) bool {
		return gates[i].Label < gates[j].Label
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"gates": gates})
}
