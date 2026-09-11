package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

// OneShotRunBound is the hard wall-clock ceiling the worker's one-shot
// consumer wraps every workflow run in (consumer.go's context.WithTimeout).
// It is the premise of DispatchTimeout's exactly-once construction: no
// live run can outlive it, so a dispatch older than DispatchTimeout
// without a verdict is provably dead. Keep in sync with the consumer.
const OneShotRunBound = 30 * time.Minute

// ReleaseReason is the release-reason vocabulary recorded on a review
// claim's ReleaseReason. Shared between the gate's classifyRelease
// (producer) and the arm path's era rules (consumer) — bare literals
// across that boundary silently disable the churn guard on a rename
// (#344 r3 P2). Branching sites consume the cancellation set through
// IsCancellationRelease; producers must write the constants, not
// re-typed literals (the #402 cancel pass DELETES Jobs for cancellation
// reasons — a producer writing the wrong constant changes what gets
// killed). "consumed"/"standdown" remain open-string: terminal classes
// nothing branches on.
const (
	// ReleaseReasonDispatchLost: the sweep released the claim before any
	// dispatch (never-consummated era — revival keeps the era clock).
	ReleaseReasonDispatchLost = "dispatch-lost"
	// ReleaseReasonHorizon: the horizon expired the era (revival resets the
	// clock — it is born expired).
	ReleaseReasonHorizon = "horizon"
	// ReleaseReasonDispatchTimeout: a DISPATCHED claim died without a verdict
	// (timer bound) — the dead-dispatch class: the breaker counts it and the
	// #331 hold keys on it. Distinct from DispatchLost by design.
	ReleaseReasonDispatchTimeout = "dispatch-timeout"
	// ReleaseReasonSuperseded: the PR's head moved — a newer claim arms at
	// the new SHA. Cancellation class (#402): the in-flight Job is deleted.
	ReleaseReasonSuperseded = "superseded"
	// ReleaseReasonPRClosed: the PR itself was closed or merged mid-review.
	// Cancellation class (#402): the in-flight Job is deleted. Deliberately
	// DISTINCT from pointer-invalid/reaped — those are bookkeeping-only
	// releases whose Jobs, if any, self-clean via the run bound and TTL.
	ReleaseReasonPRClosed = "pr-closed"
)

// Non-cancellation bookkeeping reasons (#402 r3): releases that must NEVER
// trigger the cancel pass. pointer-invalid = the claim's PR pointer cannot
// be parsed (scope/normalization quirk) — the underlying review may be
// perfectly alive and verdict-bearing. reaped = the janitor ended a
// stuck-reconciling attempt on age — its honest ledger message must not be
// overwritten by a cancellation claim.
const (
	ReleaseReasonPointerInvalid = "pointer-invalid"
	ReleaseReasonReaped         = "reaped"
)

// IsCancellationRelease reports whether a release reason marks a review the
// gate decided is obsolete — the #402 cancel-on-supersede pass deletes those
// claims' in-flight Jobs and finalizes their ledger. The predicate is THE
// definition of the cancellation set: the gate's pass, the ledger
// finalizer's phase mapping, and any future consumer branch through it, so
// a reason rename cannot silently stop cancelling while the ledger still
// records the release. Deliberately OUT of the set: horizon and standdown
// (the verdict may still land — ADR-0006 lets it post at the pinned head),
// pointer-invalid and reaped (bookkeeping-only releases — the review may be
// alive and verdict-bearing; deleting its Job would kill work a human asked
// for), and dispatch-lost/dispatch-timeout (no live Job left to cancel).
func IsCancellationRelease(reason string) bool {
	return reason == ReleaseReasonSuperseded || reason == ReleaseReasonPRClosed
}

// MaxDeadDispatchesPerHead is the dead-dispatch circuit breaker (#328): a
// head whose dispatched reviews died without a verdict this many times is
// not re-armed automatically. Every recovery mechanism involved (job-death
// release, dispatch timeout, backlog re-arm) is individually correct, but
// composed they loop forever on reviews whose honest duration exceeds
// OneShotRunBound — observed live: 19 dead runs over 12 hours on one PR.
// The breaker converts the silent burn into a bounded, visible standdown.
// It resets on a new head push or an explicit label wake (human override).
// Override breadth on Forgejo (#423): the granular label webhook cannot
// say WHICH label changed, so the override fires when the review label is
// present at ANY label touch — a bot adding an unrelated label to a
// breaker-open head resets the counter and spends a fresh dispatch.
// Deliberately accepted (r2 P6 of #424): GitHub's "labeled" had the same
// breadth but was effectively unused; Forgejo is the fleet's default host,
// so this IS the reachable path. The chosen containment is payload-label
// threading (#408: consume the changed label from the webhook payload, and
// bound overrides to ≤1 per head per Horizon) — NOT in this change; until
// it lands, MaxDeadDispatchesPerHead is defeatable by anyone with label
// permission on the PR, and the override-add reason on
// harmostes_review_gate_total is the audit trail for it.
const MaxDeadDispatchesPerHead = 3

// MaxDispatchLostReleases bounds era stickiness for NEVER-DISPATCHED
// claims (#343 fix 3): a head whose armed-queued claim was released
// dispatch-lost this many times consecutively is not re-armed
// automatically — the release/revive cycle must converge into a visible
// standdown instead of flapping one Attempt forever. Resets on a new head
// push (fresh claim), an explicit label wake (human override), or a
// successful dispatch (the chain the counter measures is
// consecutive-since-last-dispatch). Aged or not, every never-dispatched
// release IS ReleaseReasonDispatchLost (r6 P1): "we could not dispatch"
// never masquerades as the horizon's "we stopped asking", and only
// genuine verdict-window expiry (ReleaseReasonHorizon on DISPATCHED
// claims) arms the dismissal guard.
//
// In wall-clock terms (r8 review): 3 sweeps at the configured PollInterval
// — ~15 minutes at the chart default — of degraded dispatching before the
// refusal; a bad API-server hour consumes the budget in a quarter of it
// and the labeled PR waits for a human. That is the intended convergence:
// bounded, visible, and cheaper than unbounded churn; the arm-error and
// sweep-abort reasons on harmostes_review_gate_total are what tell you it
// was weather; override-add/override-remove/override-unknown on the same
// counter are the Forgejo override audit trail (#423). The concrete arithmetic lives next to pollInterval in
// chart/values.yaml, where the interval is chosen.
//
// One budget, three constants (r11 nit): gateSweepDeadline + reDispatchGrace
// (internal/worker) is the OTHER pressure that feeds this counter — a sweep
// aborted by its deadline strands armed-undispatched claims that the next
// sweep releases dispatch-lost. The ordering gateSweepDeadline <
// reDispatchGrace is CI-pinned (TestGateSweepDeadlineInsideReDispatchGrace);
// retuning any of the three alone re-opens the loop.
const MaxDispatchLostReleases = 3

// MinDispatchMargin is the minimum delivery/queue margin a configured
// DispatchTimeout must leave over OneShotRunBound. Enforced (not merely
// documented) so no configurable value can re-dispatch while a run may
// still be alive — the exactly-once invariant holds for every config
// (#255).
const MinDispatchMargin = 5 * time.Minute

// ReviewReadySpec configures the event-armed Review-Ready Gate (ADR-0006):
// the deterministic decision that a Pull Request may enter adversarial
// review. Git hosts send consolidated pull_request events (GitHub semantics,
// action-dispatched) to the per-instance webhook; the handler stays dumb
// (verify → parse → annotate) and this gate holds all decision logic.
//
// The gate arms when the trigger label is present on an open PR and proceeds
// only when every merge-rule required context is green at the head SHA —
// the repository's merge rules are the single definition of "CI green", so
// review readiness and merge readiness read the same contract. The gate
// never triggers CI and is invisible to repo-local CI-dispatch labels.
//
// Set on a WorkflowTemplate (flows to instances via ApplyTemplateDefaults);
// a Workflow may override it.
//
// Instance requirements (fail-closed otherwise): spec.config.repos must
// name the watched repos (host/owner/name — spec.config is instance-owned,
// a template cannot declare it), and the source kind should not be
// "webhook" (an armed gate re-evaluates on the poll interval; the isDue
// carve-out covers kind:webhook too, but schedule/event kinds are the
// natural fit).
type ReviewReadySpec struct {
	// Label is the single review-request label (e.g. "needs-review").
	// Human-ticking the label in the UI and the dev-workflow skill are
	// equivalent ingress. The label is consumed only by a posted verdict
	// (the deploy plugin removes it after posting at the reviewed SHA).
	Label string `json:"label,omitempty"` // default "needs-review"

	// Horizon bounds how long an armed gate keeps waiting on pending CI
	// (e.g. "6h" — enough for a slow matrix queued behind other work).
	// On expiry the gate stands down armed; the next label event re-arms.
	Horizon string `json:"horizon,omitempty"` // duration string; default "6h"

	// DispatchTimeout bounds how long a dispatched review may stay in
	// flight without a verdict before the gate presumes the run dead and
	// stands down (the backlog pass re-arms the still-labeled PR on the
	// next sweep — recovery needs no external label toggle). The bound
	// must leave a delivery/queue margin of at least MinDispatchMargin
	// over OneShotRunBound; a smaller value is rejected (falls back to
	// the default) because it would re-dispatch while a run may still be
	// alive, silently breaking exactly-once. Without it a dead dispatch
	// (helm-roll kill, wedged worker) is held "in flight" until the full
	// Horizon (observed live: 6h single-slot deadlock, #248).
	DispatchTimeout string `json:"dispatchTimeout,omitempty"` // duration string; default "45m"

	// MaxConcurrent bounds this workflow's live claims (ADR-0007): 0 means
	// the fleet default (chart HARMOSTES_MAX_CONCURRENT, 3). Attempts beyond
	// capacity queue — the gate's armed marker is the durable queue and
	// sweeps dispatch as slots free.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`

	// RunBound is this workflow's per-run Job wall clock (#348/#333): the
	// ActiveDeadlineSeconds every dispatched run Job carries. Reviews whose
	// honest methodology exceeds OneShotRunBound (observed live: 3/3 wall
	// deaths on a focused diff, #333) raise it here without raising the
	// fleet-wide bound — a dead run burns runBound × MaxDeadDispatchesPerHead
	// before the breaker stands the head down, so the cap is deliberate.
	// Per-node agent timeouts (graph nodes' timeout) govern the node's
	// internal budget and are NOT this wall: a node timeout larger than
	// RunBound is an authoring error the wall will enforce.
	RunBound string `json:"runBound,omitempty"` // duration string; default "" → OneShotRunBound (30m)
}

// MaxRunBound caps a configured RunBound: beyond it a dead run burns more
// than 6h of slot time before the breaker trips (runBound ×
// MaxDeadDispatchesPerHead) — a review that needs more than 2h wall needs
// methodology work (#336), not a bigger wall.
const MaxRunBound = 2 * time.Hour

// RunBoundDuration parses RunBound with the default applied. Invalid
// (unparsable, non-positive, above MaxRunBound) degrades to
// OneShotRunBound — same fail-closed style as DispatchTimeoutDuration.
func (r *ReviewReadySpec) RunBoundDuration() time.Duration {
	if r == nil || r.RunBound == "" {
		return OneShotRunBound
	}
	if d, err := time.ParseDuration(r.RunBound); err == nil && d > 0 && d <= MaxRunBound {
		return d
	}
	return OneShotRunBound
}

// EffectiveMaxConcurrent resolves the live-claim capacity: the spec override
// when set, else the fleet default. Nil-receiver safe (workflows without
// reviewReady always take the fleet default).
func (r *ReviewReadySpec) EffectiveMaxConcurrent(fleetDefault int) int {
	if r == nil || r.MaxConcurrent <= 0 {
		return fleetDefault
	}
	return r.MaxConcurrent
}

// HorizonDuration parses Horizon with the default applied.
func (r *ReviewReadySpec) HorizonDuration() time.Duration {
	if r == nil || r.Horizon == "" {
		return 6 * time.Hour
	}
	if d, err := time.ParseDuration(r.Horizon); err == nil && d > 0 {
		return d
	}
	return 6 * time.Hour
}

// DispatchTimeoutDuration parses DispatchTimeout with the default applied
// (the EFFECTIVE run bound + 15m delivery/queue margin). A configured value
// must leave a margin of at least MinDispatchMargin over the effective run
// bound — OneShotRunBound, or RunBound when this workflow raises the wall
// (#348/#333: a 45m wall with a 45m dispatch timeout would re-dispatch
// while the run may still be alive). Anything else (unparsable,
// non-positive, or inside the margin) degrades to the default — honoring it
// would let the gate re-dispatch while a run is still alive, silently
// breaking exactly-once (#255). Enforced, not documented.
func (r *ReviewReadySpec) DispatchTimeoutDuration() time.Duration {
	bound := r.RunBoundDuration()
	def := bound + 15*time.Minute
	if r == nil || r.DispatchTimeout == "" {
		return def
	}
	if d, err := time.ParseDuration(r.DispatchTimeout); err == nil && d >= bound+MinDispatchMargin {
		return d
	}
	return def
}

// EffectiveLabel returns Label with the default applied.
func (r *ReviewReadySpec) EffectiveLabel() string {
	if r == nil || r.Label == "" {
		return "needs-review"
	}
	return r.Label
}

// ReviewReadyStatus is the armed state of the Review-Ready Gate. It lives on
// the Workflow status so it survives runs, is visible in the UI, and costs
// nothing while idle (an unarmed gate performs zero API calls; the trigger
// annotations wake it).
type ReviewReadyStatus struct {
	// LiveClaims counts dispatched reviews in flight (ADR-0007: the
	// Attempt is the claim — this is the aggregate for the UI header).
	LiveClaims int `json:"liveClaims,omitempty"`

	// Capacity is the effective maxConcurrent this cycle evaluated against.
	Capacity int `json:"capacity,omitempty"`

	// LastDecision is the gate's last outcome: proceed | waiting | standdown | idle.
	LastDecision string `json:"lastDecision,omitempty"`

	// LastReason is the human-readable reason behind LastDecision.
	LastReason string `json:"lastReason,omitempty"`

	// LastSweepAbortAt is when a sweep last died before completing its
	// arm→dispatch handoff (r30, #379 acceptance): claims stranded by an
	// abort are released "sweep-aborted" — honest, and NOT a churn strike
	// (only observed dispatches burn the budget). Cleared implicitly by
	// age: the release pass trusts it for reDispatchGrace.
	LastSweepAbortAt *metav1.Time `json:"lastSweepAbortAt,omitempty"`
}
