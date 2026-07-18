package v1alpha1

// Label keys used by the harmostes.dev system for multi-tenant isolation and
// workflow-to-job linking. Centralised here so the controller and the UI server
// reference the same constant (no drift between the two sides of the label).
const (
	// OwnerLabel identifies which user owns a Workflow CR or a worker Job.
	//
	// The harmostes-ui server stamps it from the authenticated Authentik identity
	// (never trusts client-supplied values — see ui.SanitizeLabels). The
	// controller propagates it from the Workflow to spawned worker Jobs so the
	// UI's owner-filtered Job queries work end-to-end.
	//
	// Workflows without this label are "unmanaged" — GitOps-created system
	// workflows that are visible in kubectl but not surfaced in the self-service
	// UI.
	OwnerLabel = "harmostes.dev/owner"

	// WorkflowLabel links a worker Job to its parent Workflow CR. Both the
	// controller (Job creation) and the UI (Job filtering) use this constant.
	WorkflowLabel = "harmostes.dev/workflow"
)
