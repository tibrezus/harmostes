package attempt

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"log/slog"

	"github.com/tibrezus/harmostes/internal/claim"
)

// ResolveOptions parameterize ResolveOrCreate.
type ResolveOptions struct {
	// Namespace to create the Attempt in.
	Namespace string
	// WorkflowRef is the namespaced name (namespace/name) of the driving Workflow.
	WorkflowRef string
	// Owner is the owning Workflow CR (becomes the Attempt's controller owner so
	// it is garbage-collected with the Workflow).
	Owner *v1alpha1.Workflow
	// Scheme is the runtime scheme used to set the controller owner reference.
	Scheme *runtime.Scheme
}

// ResolveOrCreate returns the Attempt realizing the given Objective, creating
// it if it does not yet exist. Resolution is by deterministic name
// (AttemptName), so a new trigger for an in-flight objective CONTINUES the
// existing Attempt instead of starting a new one (ADR-0005). Returns the
// Attempt and created=true when a new Attempt was created.
func ResolveOrCreate(ctx context.Context, c client.Client, obj v1alpha1.ObjectiveSpec, opts ResolveOptions) (*v1alpha1.Attempt, bool, error) {
	name := AttemptName(ownerName(opts), Identity(obj))
	key := client.ObjectKey{Namespace: opts.Namespace, Name: name}

	var existing v1alpha1.Attempt
	if err := c.Get(ctx, key, &existing); err == nil {
		return &existing, false, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, false, fmt.Errorf("get attempt %s: %w", name, err)
	}

	a := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				v1alpha1.WorkflowLabel:         ownerName(opts),
				v1alpha1.OwnerLabel:            ownerValue(opts.Owner),
				"harmostes.dev/objective-kind": obj.Kind,
			},
		},
		Spec: AttemptSpecFromObjective(obj, opts),
		Status: v1alpha1.AttemptStatus{
			Phase: v1alpha1.AttemptPhaseReconciling,
		},
	}
	if opts.Owner != nil && opts.Scheme != nil {
		if err := controllerutil.SetControllerReference(opts.Owner, a, opts.Scheme); err != nil {
			return nil, false, fmt.Errorf("set attempt owner: %w", err)
		}
	}
	if err := c.Create(ctx, a); err != nil {
		return nil, false, fmt.Errorf("create attempt %s: %w", name, err)
	}
	return a, true, nil
}

// AttemptSpecFromObjective builds an AttemptSpec snapshot from an Objective +
// resolve options (workflow ref, owner, bindings copied from the Workflow).
func AttemptSpecFromObjective(obj v1alpha1.ObjectiveSpec, opts ResolveOptions) v1alpha1.AttemptSpec {
	spec := v1alpha1.AttemptSpec{
		Objective:   obj,
		WorkflowRef: opts.WorkflowRef,
		Owner:       ownerValue(opts.Owner),
	}
	if opts.Owner != nil {
		spec.Bindings = append([]v1alpha1.ExternalSystemBinding(nil), opts.Owner.Spec.Bindings...)
	}
	return spec
}

// Status compaction bounds (#289): attempt status patches are etcd requests
// — past 1.5MB etcd rejects them outright, and deterministic classes (one
// attempt per objective identity, kept forever) grow status without bound.
// The CR keeps a bounded tail window plus monotonic counters.
//
// Retention honesty: the tail + counters on the CR are the DURABLE record.
// The timeline store bridges only ~7 days (DefaultTTL, "evidence, not
// audit") — beyond that, compacted-away ledger entries are genuinely gone;
// CompactedThrough marks the boundary. Known deep-link loss: run names
// compacted beyond MaxStatusRuns 404 on both the log view and the
// /runs/{attempt}/runs/{run} redirect (handleRunLogs, handleWorkflowRunRedirect).
const (
	// MaxStatusNodeResults bounds the NodeResults tail by COUNT. A cycle of
	// an N-node graph with R retries records up to N×R envelopes — nothing
	// bounds graph size — so this cap does NOT guarantee the current cycle
	// fits; on extreme fan-out it may enter the cycle. What keeps the
	// current cycle renderable is the byte budget (which fires first for fat
	// shapes) plus the MinTailEnvelopes floor protecting the live position.
	MaxStatusNodeResults = 200
	// MaxStatusRuns bounds the Runs tail. A running run is by definition
	// recent — the tail always contains every in-flight record.
	MaxStatusRuns = 400
	// MaxStatusEvidence bounds the Evidence tail.
	MaxStatusEvidence = 200
	// MaxStatusBytes is the byte budget for the serialized NodeResults tail
	// (estimated, see envelopeBytes): eviction continues from the head while
	// the estimate exceeds it. The 1.5MB etcd request limit covers the WHOLE
	// status; runs/evidence tails are ~100B/entry (≤60KB combined), one
	// Message ≤4KB, conditions bounded — the 1MiB envelope budget keeps the
	// total structurally under the limit instead of by today's-shape luck.
	MaxStatusBytes = 1 << 20
	// MinTailEnvelopes is the floor the byte eviction never breaches —
	// compaction must never empty the ledger (the live position and the
	// current cycle's state resolve from it).
	MinTailEnvelopes = 32
	// MaxStatusPayloadBytes drops oversize opaque payloads at record time.
	// Payload is node-type-specific diagnostics, opaque to the kernel; a
	// single multi-hundred-KB payload defeats count-based caps.
	MaxStatusPayloadBytes = 4 << 10
	// MaxStatusSummaryBytes truncates summaries at record time. Byte-slicing
	// can cut a multi-byte rune; JSON renders it U+FFFD — cosmetic, not
	// corruption of any kernel-read field.
	MaxStatusSummaryBytes = 1 << 10
	// MaxStatusMessageBytes clamps the attempt-level worker message.
	MaxStatusMessageBytes = 4 << 10
)

// envelopeBytes estimates an envelope's serialized size. It deliberately
// counts every slice element (claims, references, artifacts, artifacts' and
// claims' strings) — the whole point is that no field is assumed bounded
// after the record-time clamps; whatever a plugin emits, the budget sees it.
// Claims are NOT clamped or dropped anywhere: they are the promotion-decision
// surface (claim.HasValidated), so their weight is paid here instead.
func envelopeBytes(env *v1alpha1.NodeResultEnvelope) int {
	n := len(env.NodeID) + len(env.RunID) + len(env.Status) + len(env.Summary) + len(env.Payload) + 64
	for _, c := range env.Claims {
		n += len(c.Type) + len(c.Binding) + len(c.ExternalID) + len(c.URL) + 32
	}
	for _, r := range env.References {
		n += len(r.Kind) + len(r.Identifier) + len(r.URL) + 32
	}
	for _, a := range env.Artifacts {
		n += len(a.Name) + len(a.Path) + len(a.Hash) + 32
	}
	return n
}

// compactStatus enforces the tail windows: entries leave from the HEAD
// (oldest side — upserts append), the dropped counts accumulate in the
// matching counters so totals stay derivable (AttemptStatus.TotalRuns et al).
// NodeResults are additionally bounded by the byte budget — eviction there
// continues while the estimated size exceeds MaxStatusBytes, stopping at
// MinTailEnvelopes. Returns the number of dropped entries per list (for the
// write-path log line).
func compactStatus(s *v1alpha1.AttemptStatus) (dropped int) {
	size := 0
	for i := range s.NodeResults {
		size += envelopeBytes(&s.NodeResults[i])
	}
	for len(s.NodeResults) > MinTailEnvelopes && size > MaxStatusBytes {
		size -= envelopeBytes(&s.NodeResults[0])
		if t := s.NodeResults[0].ProducedAt; t.After(s.CompactedThrough.Time) {
			s.CompactedThrough = t
		}
		s.NodeResults = s.NodeResults[1:]
		s.CompactedNodeResults++
		dropped++
	}
	if drop := len(s.NodeResults) - MaxStatusNodeResults; drop > 0 {
		for i := 0; i < drop; i++ {
			if t := s.NodeResults[i].ProducedAt; t.After(s.CompactedThrough.Time) {
				s.CompactedThrough = t
			}
		}
		s.NodeResults = s.NodeResults[drop:]
		s.CompactedNodeResults += drop
		dropped += drop
	}
	if drop := len(s.Runs) - MaxStatusRuns; drop > 0 {
		s.Runs = s.Runs[drop:]
		s.CompactedRuns += drop
		dropped += drop
	}
	if drop := len(s.Evidence) - MaxStatusEvidence; drop > 0 {
		s.Evidence = s.Evidence[drop:]
		s.CompactedEvidence += drop
		dropped += drop
	}
	return dropped
}

// boundEnvelope clamps the per-envelope size drivers at record time: oversize
// opaque payloads are dropped entirely (a truncated byte blob is neither the
// original nor valid anything), oversize summaries are truncated (prose
// survives a cut). Count caps imply byte caps only if per-entry size is
// bounded too (#289).
func boundEnvelope(env *v1alpha1.NodeResultEnvelope) {
	if len(env.Payload) > MaxStatusPayloadBytes {
		env.Payload = nil
	}
	if len(env.Summary) > MaxStatusSummaryBytes {
		env.Summary = env.Summary[:MaxStatusSummaryBytes]
	}
}

// RecordRunStarted appends (or upserts) a RunRecord marked running into the
// Attempt's status. Called by the controller right after it schedules a worker
// Job. Idempotent on run name: re-scheduling the same run updates rather than
// duplicates.
func RecordRunStarted(ctx context.Context, c client.Client, namespace, attemptName, runName string) error {
	now := metav1.Now()
	return mutateStatus(ctx, c, namespace, attemptName, func(a *v1alpha1.Attempt) {
		upsertRun(&a.Status.Runs, v1alpha1.RunRecord{Name: runName, StartedAt: now, Phase: "running"})
		a.Status.LastRunAt = now
	})
}

// RunOutcome is the terminal result of one run, recorded by the worker.
type RunOutcome struct {
	RunName   string
	Phase     string // succeeded | failed
	Envelopes []v1alpha1.NodeResultEnvelope
	Message   string
}

// RecordRunOutcome upserts the run's terminal phase, appends its Node Result
// Envelopes + Evidence, sets LastRunAt, and maps the run outcome to the Attempt
// phase (ADR-0005 + ADR-0004 promotion): a failed run → AttemptPhaseFailed; a
// succeeded run that produced at least one deterministically-validated claim →
// AttemptPhaseValidated; otherwise a succeeded run → AttemptPhaseReconciling
// (nothing was deterministically confirmed). Best-effort caller: errors are
// returned but must not abort the run.
func RecordRunOutcome(ctx context.Context, c client.Client, namespace, attemptName string, outcome RunOutcome) error {
	now := metav1.Now()
	return mutateStatus(ctx, c, namespace, attemptName, func(a *v1alpha1.Attempt) {
		upsertRun(&a.Status.Runs, v1alpha1.RunRecord{
			Name: outcome.RunName, Phase: outcome.Phase, EndedAt: now,
		})
		// Upsert (not append): envelopes already recorded incrementally as
		// nodes completed are replaced in place; new ones append. Blind append
		// would duplicate every envelope the OnNodeResult hook already landed.
		for i := range outcome.Envelopes {
			boundEnvelope(&outcome.Envelopes[i])
			upsertNodeResult(&a.Status.NodeResults, outcome.Envelopes[i])
		}
		for _, env := range outcome.Envelopes {
			a.Status.Evidence = appendUniqueEvidence(a.Status.Evidence, env.References)
		}
		a.Status.LastRunAt = now
		switch outcome.Phase {
		case "failed":
			a.Status.Phase = v1alpha1.AttemptPhaseFailed
		default:
			// ADR-0004 promotion: a successful run that produced a
			// deterministically-validated claim completes the objective's
			// reconciliation — the targeted state was confirmed by a validator.
			// Without a validated claim, success alone does not validate
			// (nothing was deterministically confirmed); the attempt keeps
			// reconciling, allowing a failed attempt to recover on a later run.
			if claim.HasValidated(outcome.Envelopes) {
				a.Status.Phase = v1alpha1.AttemptPhaseValidated
			} else {
				a.Status.Phase = v1alpha1.AttemptPhaseReconciling
			}
		}
		if outcome.Message != "" {
			// One unbounded field defeats the cap (#289 r2): the worker
			// message is prose — truncation is safe.
			a.Status.Message = outcome.Message
			if len(a.Status.Message) > MaxStatusMessageBytes {
				a.Status.Message = a.Status.Message[:MaxStatusMessageBytes]
			}
		}
	})
}

// mutateStatus is the shared get-mutate-patch loop for Attempt status. It
// re-reads on each call so concurrent recorders don't clobber each other.
func mutateStatus(ctx context.Context, c client.Client, namespace, attemptName string, mutate func(*v1alpha1.Attempt)) error {
	var a v1alpha1.Attempt
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: attemptName}, &a); err != nil {
		return fmt.Errorf("get attempt %s: %w", attemptName, err)
	}
	base := a.DeepCopy()
	mutate(&a)
	// Structural compaction (#289): every status writer funnels through a
	// get-mutate-patch helper (this one, or claim.go's patchAttemptStatus) —
	// enforcing the tail windows HERE makes the bound a property of the
	// write path, not call-site discipline. Raw Status().Patch callers must
	// not append to the bounded lists (today none do: the controller stamps
	// ObservedGeneration/LastRunAt/conditions only).
	if dropped := compactStatus(&a.Status); dropped > 0 {
		// Compaction must never be silent again — the #289 failure ran
		// unnoticed for 8 days.
		slog.Warn("attempt status compacted", "attempt", attemptName, "dropped", dropped,
			"through", a.Status.CompactedThrough.Time)
	}
	return c.Status().Patch(ctx, &a, client.MergeFrom(base))
}

// UpsertNodeResult records one node execution's envelope incrementally, as
// the node completes rather than batched at outcome. The ledger is keyed by
// (NodeID, RunID): a new key appends — preserving history across node retries
// (one pod = one run = one envelope, ADR-0007) — and an existing key replaces
// in place, so both incremental arrival and the outcome upsert are idempotent.
// Best-effort caller: errors are returned but must not abort the run.
func UpsertNodeResult(ctx context.Context, c client.Client, namespace, attemptName string, env v1alpha1.NodeResultEnvelope) error {
	boundEnvelope(&env)
	return mutateStatus(ctx, c, namespace, attemptName, func(a *v1alpha1.Attempt) {
		upsertNodeResult(&a.Status.NodeResults, env)
	})
}

// upsertNodeResult is the pure ledger merge: replace-in-place on a (NodeID,
// RunID) match, append otherwise.
func upsertNodeResult(envelopes *[]v1alpha1.NodeResultEnvelope, env v1alpha1.NodeResultEnvelope) {
	for i := range *envelopes {
		if (*envelopes)[i].NodeID == env.NodeID && (*envelopes)[i].RunID == env.RunID {
			(*envelopes)[i] = env
			return
		}
	}
	*envelopes = append(*envelopes, env)
}

// upsertRun inserts or updates a RunRecord by Name (idempotent re-scheduling).
func upsertRun(runs *[]v1alpha1.RunRecord, r v1alpha1.RunRecord) {
	for i, existing := range *runs {
		if existing.Name == r.Name {
			// Preserve StartedAt from the original record if the upsert omits it.
			if r.StartedAt.IsZero() {
				r.StartedAt = existing.StartedAt
			}
			(*runs)[i] = r
			return
		}
	}
	*runs = append(*runs, r)
}

// appendUniqueEvidence appends Evidence References not already present (keyed by
// binding+kind+identifier) so retries don't duplicate evidence rows.
func appendUniqueEvidence(existing []v1alpha1.EvidenceReference, add []v1alpha1.EvidenceReference) []v1alpha1.EvidenceReference {
	seen := make(map[string]bool, len(existing))
	for _, e := range existing {
		seen[evidenceKey(e)] = true
	}
	for _, e := range add {
		k := evidenceKey(e)
		if !seen[k] {
			existing = append(existing, e)
			seen[k] = true
		}
	}
	return existing
}

func evidenceKey(e v1alpha1.EvidenceReference) string {
	return e.Binding + "/" + e.Kind + "/" + e.Identifier
}

func ownerName(opts ResolveOptions) string {
	if opts.Owner != nil {
		return opts.Owner.Name
	}
	// Fall back to parsing the workflowRef (namespace/name).
	if i := lastSlash(opts.WorkflowRef); i >= 0 {
		return opts.WorkflowRef[i+1:]
	}
	return opts.WorkflowRef
}

func ownerValue(wf *v1alpha1.Workflow) string {
	if wf == nil {
		return ""
	}
	if o := wf.Labels[v1alpha1.OwnerLabel]; o != "" {
		return o
	}
	return ""
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
