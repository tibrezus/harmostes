package ui

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

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
	GatePluginName string `json:"gatePluginName,omitempty"`
	GateConfigMap  string `json:"gateConfigMap,omitempty"`

	SkillPath string `json:"skillPath"`
	TaskName  string `json:"taskName"`

	// BindingTemplates defines the External System Bindings (ADR-0003) this
	// gate archetype needs. Each template declares a binding name, role,
	// surface kind, and granted capabilities. The Target is filled from the
	// workflow creation form at runtime via deriveBindings().
	BindingTemplates []BindingTemplate `json:"bindingTemplates,omitempty"`

	// Gate contract.
	ExitGreen  int    `json:"exitGreen"`
	ExitFailed int    `json:"exitFailed"`
	Feedback   string `json:"feedback"`

	// TargetFromPrepareRepos is true when the workflow target repo is found in
	// the prepare plugin's config.repos[] array.
	TargetFromPrepareRepos bool `json:"targetFromPrepareRepos,omitempty"`
}

// BindingTemplate is a declarative External System Binding that a gate
// archetype needs. The Target is populated at workflow creation time from
// the repo URL and branch the user provides.
type BindingTemplate struct {
	Name        string   // binding name (sourceRepo, workspaceRepo, etc.)
	BindingRole string   // v1alpha1.BindingRole* constant
	SurfaceKind string   // v1alpha1.SurfaceKind* constant
	Granted     []string // capability tokens (e.g. repository.read, repository.push)
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
		BindingTemplates: []BindingTemplate{
			{Name: "workspaceRepo", BindingRole: v1alpha1.BindingRoleWorkspaceRepo, SurfaceKind: v1alpha1.SurfaceKindRepository,
				Granted: []string{"repository.read", "repository.push"}},
		},
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
		BindingTemplates: []BindingTemplate{
			{Name: "sourceRepo", BindingRole: v1alpha1.BindingRoleSourceRepo, SurfaceKind: v1alpha1.SurfaceKindRepository,
				Granted: []string{"repository.read"}},
			{Name: "review", BindingRole: v1alpha1.BindingRoleReview, SurfaceKind: v1alpha1.SurfaceKindReview,
				Granted: []string{"review.comment.write"}},
		},
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
		BindingTemplates: []BindingTemplate{
			{Name: "workspaceRepo", BindingRole: v1alpha1.BindingRoleWorkspaceRepo, SurfaceKind: v1alpha1.SurfaceKindRepository,
				Granted: []string{"repository.read", "repository.push"}},
		},
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
		BindingTemplates: []BindingTemplate{
			{Name: "workspaceRepo", BindingRole: v1alpha1.BindingRoleWorkspaceRepo, SurfaceKind: v1alpha1.SurfaceKindRepository,
				Granted: []string{"repository.read", "repository.push"}},
		},
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

// deriveBindings populates External System Binding targets from the workflow
// creation form. Each binding template from the gate archetype gets a Target
// filled from the repo URL and branch the user provided.
func deriveBindings(gate *GateArchetype, repoURL, branch string) []v1alpha1.ExternalSystemBinding {
	if len(gate.BindingTemplates) == 0 {
		return nil
	}
	host, object := parseGitURL(repoURL)
	bindings := make([]v1alpha1.ExternalSystemBinding, 0, len(gate.BindingTemplates))
	for _, tmpl := range gate.BindingTemplates {
		bindings = append(bindings, v1alpha1.ExternalSystemBinding{
			Name:        tmpl.Name,
			BindingRole: tmpl.BindingRole,
			SurfaceKind: tmpl.SurfaceKind,
			Granted:     tmpl.Granted,
			Target:      v1alpha1.BindingTarget{Host: host, Object: object, Branch: branch},
		})
	}
	return bindings
}

// parseGitTarget extracts the host and owner/repo from a git URL.
// "https://github.com/rezuscloud/signoz.git" → ("github.com", "rezuscloud/signoz")
// "git@github.com:rezuscloud/signoz.git" → ("github.com", "rezuscloud/signoz")
func parseGitURL(rawURL string) (host, object string) {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return "", ""
	}
	// SSH form: git@host:path
	if i := strings.Index(s, ":"); i > 0 && strings.Contains(s[:i], "@") {
		host = s[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		object = strings.TrimSuffix(s[i+1:], ".git")
		return host, object
	}
	// HTTPS form
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host, strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	}
	return "", s
}

// ---------------------------------------------------------------------------
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
