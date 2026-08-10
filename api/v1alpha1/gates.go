package v1alpha1

// ---------------------------------------------------------------------------
// Gate → Objective Kind mapping (single source of truth)
// ---------------------------------------------------------------------------
//
// NAMING RULE: the gate name IS the workflow archetype identity. It must match
// its purpose — the label is just the display form. A gate name must never
// describe a validation artifact ("resolved") when the workflow's purpose is
// different ("maintenance"). If you can't guess what a gate does from its
// name, the name is wrong.
//
// The gate name appears in:
//   - spec.agent.gate.plugin.name  (Workflow YAML)
//   - {gate}-{targetSlug}         (Workflow CR name)
//   - this map                     (Objective Kind derivation)
//   - the gate catalog             (UI display, creation form)
//
// Adding a new gate archetype means adding one entry here AND one entry in the
// gate catalog (internal/ui/gates.go). The name must be identical in both.

// GateObjectiveKinds maps gate plugin names to Objective Kinds. This is the
// single source of truth for gate→kind derivation (ADR-0005). The fork-source
// override (Spec.Source.Fork) is handled separately in attempt.DeriveKind.
var GateObjectiveKinds = map[string]string{
	"wiki-lint":        ObjectiveKindDocumentationSync,
	"review-validate":  ObjectiveKindPRReview,
	"fork-maintenance": ObjectiveKindForkSync,
	"noop":             ObjectiveKindDocumentationSync,
}

// ObjectiveKindForGate returns the Objective Kind for a gate plugin name.
// Falls back to documentation-sync (the dominant fleet archetype) when the
// gate name is unrecognized or empty.
func ObjectiveKindForGate(gateName string) string {
	if kind, ok := GateObjectiveKinds[gateName]; ok {
		return kind
	}
	return ObjectiveKindDocumentationSync
}
