package v1alpha1

// Label keys used by the harmostes.dev system for multi-tenant isolation and
// workflow-to-job linking. Centralised here so the controller and the UI server
// reference the same constant (no drift between the two sides of the label).
const (
	// OwnerLabel identifies which user owns a Workflow CR or a worker Job.
	//
	// The harmostes-ui server stamps it from the authenticated Authentik identity
	// (never trusts client-supplied values). The
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

	// ReviewClaimLabel marks a review-claim Attempt as RELEASED. The label
	// is the gate's list bound: LiveReviewClaims selects (workflow=X,
	// review-claim DoesNotExist) server-side, so the released history — one
	// attempt per reviewed head, retained until the retention-GC horizon
	// (#385), full status with run ledgers — never crosses the wire again.
	//
	// ABSENCE MEANS LIVE, deliberately: the marker is subtractive on the
	// live side, so the pre-upgrade truth (no label) is the live truth —
	// legacy claims holding real slots are visible to the first post-deploy
	// sweep with zero backfill, and no rollout can over-dispatch past them.
	//
	// Invariants (probe-verified; the history lives in the PRs, not here):
	//   - exactly two writers SET the marker (ReleaseClaim,
	//     ReleaseClaimDead), both RV-preconditioned;
	//   - ArmClaim REMOVES it RV-preconditioned BEFORE the status patch —
	//     a failed removal aborts the arm pre-commit and self-heals next
	//     sweep;
	//   - label and status are two resources and CAN transiently disagree;
	//     both divergence directions are guarded client-side: status-
	//     released-but-listed-live is skipped by the Released re-check, and
	//     markClaimReleased stamps the marker only when a FRESH status read
	//     still says Released, so a concurrent revival can never be marked
	//     out of the live list.
	ReviewClaimLabel = "harmostes.dev/review-claim"

	// ReviewClaimReleased is the ReviewClaimLabel value; it is the only
	// value the label ever carries.
	ReviewClaimReleased = "released"

	// ObjectiveKindLabel carries the Objective kind an Attempt was derived
	// from ("pr-review", "fork-sync", …). Set at create (lifecycle.go);
	// consumed by the gate's live-claim filter CLIENT-side (r11: server-side
	// would couple the bound to the worker image's rollout — the label is
	// hygiene, the marker's absence is the correctness leg).
	ObjectiveKindLabel = "harmostes.dev/objective-kind"

	// AttemptLabel links an attempt-runner Job (and its pods, via the Job's
	// pod template) to the Attempt CR that owns the run. Set by BuildJob;
	// consumed by the UI's run-log discovery and the dispatcher's liveness
	// checks.
	AttemptLabel = "harmostes.dev/attempt"
)
