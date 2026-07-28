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

	// LastRunAt is when the most recent run in this attempt executed.
	// +optional
	LastRunAt metav1.Time `json:"lastRunAt,omitempty"`

	// Message is a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions are standard k8s conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
