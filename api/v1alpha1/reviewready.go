package v1alpha1

import (
	"time"
)

// OneShotRunBound was the hard wall-clock ceiling every workflow run was
// killed at (30m). ADR-0008 retired the default death: runs complete or fail
// on their own; a finite bound is opt-in per workflow (spec.runBound, riding
// the Attempt snapshot). The constant survives only as the reference point
// for DispatchTimeout's legacy floor (below) — it is no longer applied to
// jobs.
const OneShotRunBound = 30 * time.Minute

// MaxDeadDispatchesPerHead is the dead-dispatch circuit breaker (#328): a
// head whose dispatched reviews died without a verdict this many times is
// not re-armed automatically. Every recovery mechanism involved (job-death
// release, dispatch timeout, backlog re-arm) is individually correct, but
// composed they loop forever on reviews whose honest duration exceeds
// OneShotRunBound — observed live: 19 dead runs over 12 hours on one PR.
// The breaker converts the silent burn into a bounded, visible standdown.
// It resets on a new head push or an explicit label wake (human override).
const MaxDeadDispatchesPerHead = 3

// MinDispatchMargin is the minimum delivery/queue margin a configured
// DispatchTimeout must leave over OneShotRunBound (the legacy finite bound).
// Enforced (not merely documented) so no configurable value could
// re-dispatch while a run might still be alive under a finite bound.
// Under the ADR-0008 default (unlimited runs) DispatchTimeout is a pure
// backstop: deaths are fact-based — the sweep counts a death only when the
// attempt's Job is observably gone (#331/#332) — so a long live run is
// never presumed dead.
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
// (OneShotRunBound + 15m delivery/queue margin). A configured value must
// leave a margin of at least MinDispatchMargin over OneShotRunBound;
// anything else (unparsable, non-positive, or inside the margin) degrades
// to the default — honoring it would let the gate re-dispatch while a run
// is still alive, silently breaking exactly-once (#255).
func (r *ReviewReadySpec) DispatchTimeoutDuration() time.Duration {
	def := OneShotRunBound + 15*time.Minute
	if r == nil || r.DispatchTimeout == "" {
		return def
	}
	if d, err := time.ParseDuration(r.DispatchTimeout); err == nil && d >= OneShotRunBound+MinDispatchMargin {
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
}
