package ui

import (
	"net/http"
	"sort"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
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
//   pr-review  PR review (fetch PR → agent → validate → post review)
//   fork-maintenance Fork maintenance (cherry-pick → agent → resolve → deploy)
//   noop             Passthrough (deterministic-only, no LLM validation)
//
// NAMING RULE: the gate name IS the workflow archetype identity. It must
// match its purpose — the label is just the display form. See
// api/v1alpha1/gates.go for the full rule.

// GateArchetype defines the common structure for all workflows using a gate.
type GateArchetype struct {
	// Name is the archetype identity (gate-centric grouping key in the UI).
	// It does NOT have to match a gate plugin — see GatePluginName.
	Name string `json:"name"`

	// Label is the human-readable gate name for the UI.
	Label string `json:"label"`

	// Description explains what the gate validates.
	Description string `json:"description"`

	// Category groups gates in the UI.
	Category string `json:"category"`

	// Common structure — all workflows using this gate share these defaults.
	PreparePlugin    string `json:"preparePlugin"`    // the prepare plugin name
	PrepareConfigMap string `json:"prepareConfigMap"` // ConfigMap for prepare plugin (empty = builtin)
	DeployPlugin     string `json:"deployPlugin"`     // the deploy plugin name
	DeployConfigMap  string `json:"deployConfigMap"`  // ConfigMap for deploy plugin (empty = builtin)

	// GatePluginName is the actual gate plugin to set in
	// spec.agent.gate.plugin.name. If empty, Name is used (backward compat).
	// This separates the archetype identity from the gate plugin — e.g.
	// the "fork-maintenance" archetype uses the "noop" gate plugin because
	// fork-sync validates internally.
	GatePluginName string `json:"gatePluginName,omitempty"`
	GateConfigMap  string `json:"gateConfigMap,omitempty"` // ConfigMap for gate plugin

	SkillPath string `json:"skillPath"` // path to SKILL.md
	TaskName  string `json:"taskName"`  // task template name

	// Gate contract.
	ExitGreen  int    `json:"exitGreen"`  // exit code for green (always 0)
	ExitFailed int    `json:"exitFailed"` // exit code for failed (always 1)
	Feedback   string `json:"feedback"`   // where the agent reads feedback ("stderr")

	// TargetFromPrepareRepos is true when the workflow target repo is found in
	// the prepare plugin's config.repos[] array (e.g. pr-review reads
	// the reviewed repo from pr-fetch config) rather than spec.source.repo.
	TargetFromPrepareRepos bool `json:"targetFromPrepareRepos,omitempty"`
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
		Name:                   "pr-review",
		Label:                  "PR Review",
		Description:            "Fetch labeled PR → agent reviews → pr-review checks structure → post review to git host",
		Category:               "code-review",
		PreparePlugin:          "pr-fetch",
		PrepareConfigMap:       "harmostes-pr-review",
		DeployPlugin:           "post-review",
		DeployConfigMap:        "harmostes-pr-review",
		GatePluginName:         "pr-review",
		GateConfigMap:          "harmostes-pr-review",
		SkillPath:              "/skills/pr-review/SKILL.md",
		TaskName:               "pr-review",
		ExitGreen:              0,
		ExitFailed:             1,
		Feedback:               "stderr",
		TargetFromPrepareRepos: true,
	},
	{
		Name:             "fork-maintenance",
		Label:            "Fork Maintenance",
		Description:      "Upstream sync → fork-sync merges release branch → conflict-resolver handles disputes → noop gate",
		Category:         "fork-maintenance",
		PreparePlugin:    "fork-sync",
		PrepareConfigMap: "fork-maintenance-plugins",
		DeployPlugin:     "noop",
		GatePluginName:   "noop", // fork-sync validates internally; no separate gate
		SkillPath:        "/skills/fork-maintenance/SKILL.md",
		TaskName:         "resolve-conflict",
		ExitGreen:        0,
		ExitFailed:       1,
		Feedback:         "stderr",
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
// Returns the gate plugin name, or "noop" if not set.
func workflowGate(gatePluginName string) string {
	if gatePluginName == "" {
		return "noop"
	}
	return gatePluginName
}

// deriveArchetype infers the gate archetype from the full workflow spec.
// This is needed because multiple archetypes can share the same gate plugin
// (e.g. both fork-maintenance and noop use the noop gate plugin). We
// disambiguate using the prepare plugin: each archetype has a unique prepare
// plugin except wiki-lint and noop (both rig-emit), which are separated by
// the gate plugin name.
func deriveArchetype(wf *v1alpha1.Workflow) string {
	preparePlugin := wf.Spec.Prepare.Plugin.Name
	gatePlugin := wf.Spec.Agent.Gate.Plugin.Name
	if gatePlugin == "" {
		gatePlugin = "noop"
	}

	// Find archetypes whose prepare plugin matches
	var candidates []*GateArchetype
	for i := range gateCatalog {
		if gateCatalog[i].PreparePlugin == preparePlugin {
			candidates = append(candidates, &gateCatalog[i])
		}
	}

	switch len(candidates) {
	case 1:
		return candidates[0].Name
	case 0:
		// No prepare plugin match — fall back to gate plugin name
		if arch := gateByName(gatePlugin); arch != nil {
			return arch.Name
		}
		return gatePlugin
	default:
		// Multiple candidates — disambiguate by gate plugin
		for _, c := range candidates {
			gp := c.GatePluginName
			if gp == "" {
				gp = c.Name
			}
			if gp == gatePlugin {
				return c.Name
			}
		}
		return gatePlugin
	}
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
