// Package v1alpha1 — Attempt CRD (ADR-0005).
//
// An Attempt is the canonical unit of orchestration history. It is NOT a
// Workflow Run: a run is one execution episode inside an attempt, and a single
// attempt may span many runs, retries, repair loops, validations, and
// publications across multiple external surfaces.
//
// Every Attempt is anchored by an Implementation Objective with a small
// canonical structure (Objective Kind, Objective Subject, Desired Outcome).
// An Attempt is a Reconciliation Goal toward a Targeted State — not event
// handling. Triggers (webhooks/schedules/manual) are wake-ups; runs keep
// happening until the targeted state is validated, superseded by a new target
// state, or terminally failed.
//
// New triggers CONTINUE an existing Attempt while the Objective Identity
// remains the same. Objective Identity = Objective Kind + Primary Subject +
// Targeted State, so same subject + different revision/release/spec = a
// distinct attempt.
//
// Harmostes is the canonical orchestration historian (ADR-0005): external
// systems keep their native artifacts, but the authoritative cross-surface
// timeline lives here, linking external artifacts via Evidence References.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Attempt phases (the reconciliation completion rule, ADR-0005).
const (
	AttemptPhaseReconciling = "reconciling" // in progress; targeted state not yet validated
	AttemptPhaseValidated   = "validated"   // targeted state deterministically confirmed
	AttemptPhaseSuperseded  = "superseded"  // a newer targeted state replaced this one
	AttemptPhaseFailed      = "failed"      // terminally failed; will not retry without a new trigger/objective
)

// Objective Kind — canonical categories of Implementation Objective. Extensible,
// but a small kernel-recognized set keeps the UI and policy consistent.
const (
	ObjectiveKindDocumentationSync = "documentation-sync"
	ObjectiveKindPRReview          = "pr-review"
	ObjectiveKindForkSync          = "fork-sync"
	ObjectiveKindDeploymentChange  = "deployment-change"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=attempt
// +kubebuilder:printcolumn:name=Objective,type=string,JSONPath=.spec.objective.kind
// +kubebuilder:printcolumn:name=Subject,type=string,JSONPath=.spec.objective.primarySubject.object
// +kubebuilder:printcolumn:name=Target,type=string,JSONPath=.spec.objective.targetedState
// +kubebuilder:printcolumn:name=Phase,type=string,JSONPath=.status.phase
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp

// Attempt is one Implementation Attempt in the Canonical Orchestration History.
type Attempt struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AttemptSpec   `json:"spec,omitempty"`
	Status AttemptStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AttemptList is a list of Attempts.
type AttemptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Attempt `json:"items"`
}

// AttemptSpec declares what this attempt is trying to achieve and under what
// authority.
type AttemptSpec struct {
	// Objective is the Implementation Objective this attempt realizes.
	// Anchors the attempt; carries the Targeted State that completes the
	// Objective Identity.
	Objective ObjectiveSpec `json:"objective"`

	// WorkflowRef is the namespaced name (namespace/name) of the driving
	// Workflow CR. Multiple Runs of that workflow execute inside this attempt.
	WorkflowRef string `json:"workflowRef"`

	// Bindings is the snapshot of External System Bindings in force for this
	// attempt (copied from the Workflow at attempt creation — the Binding
	// Authority Boundary is fixed per ADR-0003). Runtime may not expand this.
	// +optional
	Bindings []ExternalSystemBinding `json:"bindings,omitempty"`

	// RunBound is the snapshot of the Workflow's run bound (ADR-0008): the
	// maximum wall-clock duration for ONE run of this attempt. Empty =
	// unlimited — a run completes or fails on its own; the kernel does not
	// kill it. Copied at attempt creation like Bindings: the boundary is
	// fixed per attempt.
	// +optional
	RunBound string `json:"runBound,omitempty"`

	// Cache is the snapshot of the Workflow's cache configuration
	// (ADR-0008): warm toolchain caches (Go build/module, npm) mounted into
	// the run so verification commands pay seconds, not minutes. Copied at
	// attempt creation like Bindings/RunBound.
	// +optional
	Cache *CacheSpec `json:"cache,omitempty"`

	// Owner is the user this attempt belongs to (mirrors Workflow
	// harmostes.dev/owner label; drives UI isolation).
	// +optional
	Owner string `json:"owner,omitempty"`
}

// ObjectiveSpec is the structured Implementation Objective (ADR-0005).
type ObjectiveSpec struct {
	// Kind is the Objective Kind (ObjectiveKind* constant).
	Kind string `json:"kind"`

	// PrimarySubject is the central bound object this objective is primarily
	// about. The Objective Subject always has exactly one Primary Subject.
	PrimarySubject Subject `json:"primarySubject"`

	// RelatedSubjects are additional bound objects that participate but are
	// not the central target (e.g. a wiki space alongside a repo). Never an
	// unstructured bag — explicit, named bindings.
	// +optional
	RelatedSubjects []Subject `json:"relatedSubjects,omitempty"`

	// DesiredOutcome is the intended terminal result of this objective.
	DesiredOutcome string `json:"desiredOutcome"`

	// TargetedState is the specific revision / head SHA / release / desired
	// spec hash / resource version that makes this objective a DISTINCT
	// implementation (completes the Objective Identity). Same subject + a
	// different TargetedState => a different Attempt.
	TargetedState string `json:"targetedState"`
}

// Subject is a bound object participating in an objective.
type Subject struct {
	// Binding is the ExternalSystemBinding.Name this subject lives on.
	Binding string `json:"binding"`

	// Object is the identifier within that binding (e.g. owner/repo, issue
	// number, page path, resource namespace/name).
	Object string `json:"object"`
}

// AttemptStatus is the canonical orchestration history for this attempt.
type AttemptStatus struct {
	// ObservedGeneration is the generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the attempt phase (AttemptPhase* constant). An attempt ends
	// when its targeted state is validated, superseded, or terminally failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Runs are the Workflow Run records executed inside this attempt.
	// +optional
	// +listType=atomic
	Runs []RunRecord `json:"runs,omitempty"`

	// NodeResults are the Node Result Envelopes recorded across runs.
	// +optional
	// +listType=atomic
	NodeResults []NodeResultEnvelope `json:"nodeResults,omitempty"`

	// Evidence aggregates Evidence References discovered/produced across runs.
	// +optional
	// +listType=atomic
	Evidence []EvidenceReference `json:"evidence,omitempty"`

	// CompactedRuns counts RunRecords dropped from the head (oldest side) of
	// Runs by status compaction (#289): the CR keeps a bounded tail window —
	// etcd rejects status patches past 1.5MB, which deterministic classes
	// (one attempt per objective identity, forever) hit after enough runs.
	// Total runs ever = len(Runs) + CompactedRuns.
	// +optional
	CompactedRuns int `json:"compactedRuns,omitempty"`

	// CompactedNodeResults counts NodeResultEnvelopes dropped from the head
	// (oldest side) of NodeResults by status compaction (#289). Total
	// envelopes ever = len(NodeResults) + CompactedNodeResults. The durable
	// node-boundary log is the timeline store; the CR ledger is the render
	// cache (ADR-0005 amendment).
	// +optional
	CompactedNodeResults int `json:"compactedNodeResults,omitempty"`

	// CompactedEvidence counts EvidenceReferences dropped from the head
	// (oldest side) of Evidence by status compaction (#289). Total ever =
	// len(Evidence) + CompactedEvidence.
	// +optional
	CompactedEvidence int `json:"compactedEvidence,omitempty"`

	// CompactedThrough is the ProducedAt of the newest envelope dropped by
	// status compaction (#289): history BEFORE this instant is not in the
	// CR ledger (the visible tail is a window, not the whole story). The
	// timeline store bridges ~7 days beyond it (DefaultTTL — "evidence, not
	// audit"); beyond that the dropped entries are genuinely gone.
	// +optional
	CompactedThrough metav1.Time `json:"compactedThrough,omitempty"`

	// LastRunAt is when the most recent run in this attempt executed.
	// +optional
	LastRunAt metav1.Time `json:"lastRunAt,omitempty"`

	// Message is a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions are standard k8s conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Review holds the Review-Ready Gate's per-claim state (ADR-0007):
	// the Attempt IS the claim — armed horizon, dispatch liveness, and
	// release state live here, never on the Workflow's status slot.
	// +optional
	Review *ReviewClaimStatus `json:"review,omitempty"`
}

// ReviewClaimStatus is the gate's claim on this Attempt: one live claim
// per reviewed PR; head moves supersede (a fresh claim arms at the new
// head — the objective identity changes with the SHA, ADR-0005).
type ReviewClaimStatus struct {
	// PR is the reviewed pointer: host/owner/name#N (normalized).
	PR string `json:"pr,omitempty"`

	// HeadSHA is the reviewed commit — the verdict-window anchor.
	HeadSHA string `json:"headSha,omitempty"`

	// Label is the gate label at arm time.
	Label string `json:"label,omitempty"`

	// ArmedSince is when the gate armed at HeadSHA (horizon start).
	ArmedSince *metav1.Time `json:"armedSince,omitempty"`

	// DispatchedAt marks the review dispatched (Job created); the
	// DispatchTimeout liveness bound runs from here (#248).
	DispatchedAt *metav1.Time `json:"dispatchedAt,omitempty"`

	// Released frees the claim's capacity slot: the PR may re-arm on a
	// later sweep.
	Released bool `json:"released,omitempty"`

	// ReleaseReason: consumed | horizon | dispatch-timeout | superseded | closed.
	ReleaseReason string `json:"releaseReason,omitempty"`

	// DeadDispatches counts dispatched reviews of this claim that provably
	// died without a verdict (dispatch-lost / dispatch-timeout). The gate's
	// breaker (#328) refuses to re-arm a head at MaxDeadDispatchesPerHead;
	// a head change or an explicit label wake resets it.
	DeadDispatches int `json:"deadDispatches,omitempty"`
}

// RunRecord is one Workflow Run (job) executed inside an attempt.
type RunRecord struct {
	// Name is the Job/run name.
	Name string `json:"name"`

	// StartedAt is when the run started.
	// +optional
	StartedAt metav1.Time `json:"startedAt,omitempty"`

	// EndedAt is when the run ended (empty while running).
	// +optional
	EndedAt metav1.Time `json:"endedAt,omitempty"`

	// Phase is the run phase: running | succeeded | failed.
	// +optional
	Phase string `json:"phase,omitempty"`
}

// TotalRuns is runs ever executed on this attempt: the live tail plus every
// compacted-away record (#289).
func (s *AttemptStatus) TotalRuns() int {
	if s == nil {
		return 0
	}
	return len(s.Runs) + s.CompactedRuns
}

// TotalNodeResults is envelopes ever recorded: the live tail plus every
// compacted-away envelope (#289).
func (s *AttemptStatus) TotalNodeResults() int {
	if s == nil {
		return 0
	}
	return len(s.NodeResults) + s.CompactedNodeResults
}

// TotalEvidence is evidence references ever recorded: the live tail plus
// every compacted-away reference (#289).
func (s *AttemptStatus) TotalEvidence() int {
	if s == nil {
		return 0
	}
	return len(s.Evidence) + s.CompactedEvidence
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy
// ---------------------------------------------------------------------------

func (in *Attempt) DeepCopyInto(out *Attempt) { deepCopy(in, out) }
func (in *Attempt) DeepCopy() *Attempt {
	if in == nil {
		return nil
	}
	out := new(Attempt)
	deepCopy(in, out)
	return out
}
func (in *Attempt) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AttemptList) DeepCopyInto(out *AttemptList) { deepCopy(in, out) }
func (in *AttemptList) DeepCopy() *AttemptList {
	if in == nil {
		return nil
	}
	out := new(AttemptList)
	deepCopy(in, out)
	return out
}
func (in *AttemptList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*Attempt)(nil)
	_ runtime.Object = (*AttemptList)(nil)
)

// AttemptResource returns the GroupResource for Attempt.
func AttemptResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource("attempts").GroupResource()
}
