// Package v1alpha1 — Node Result Envelope + Claims (ADR-0004).
//
// Every Node produces a universal Node Result Envelope consumed by the
// orchestration kernel for control-flow, provenance, and policy. The envelope
// has universal top-level fields; node-type-specific detail lives in the nested
// Node Payload. The kernel never reads ad hoc node output — it reads this
// contract.
//
// A Claim inside an envelope is a Reference-Backed Fact: a typed statement tied
// to durable identifiers and associated with a declared External System Binding.
// Narrative assertions ("the docs are correct now") are NOT valid claims.
//
// Claims carry a Trust Class. Claims from Non-Deterministic Nodes are NEVER
// authoritative on their own — they must be promoted by Deterministic
// Validation (incl. of claimed External Side Effects) before the kernel may
// rely on them.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Trust classes for a Claim (ADR-0004).
const (
	ClaimTrustObserved  = "observed"  // emitted by the executing node; not yet authoritative
	ClaimTrustValidated = "validated" // promoted by deterministic validation; authoritative
)

// Node Result status values — the execution outcome recorded in a
// NodeResultEnvelope (ADR-0004). These are the canonical envelope strings,
// distinct from the graph package's internal NodeStatus (green/failed/skipped):
// the kernel maps internal status to these when synthesizing an envelope.
const (
	NodeResultStatusOK      = "ok"
	NodeResultStatusSkipped = "skipped"
	NodeResultStatusFailed  = "failed"
)

// NodeResultEnvelope is the universal structured result of a single Node
// execution. It is a runtime/status concept — recorded into Attempt status —
// not part of a Workflow spec.
type NodeResultEnvelope struct {
	// NodeID is the graph node that produced this result.
	NodeID string `json:"nodeID"`

	// RunID is the Workflow Run (job) that executed this node.
	// +optional
	RunID string `json:"runID,omitempty"`

	// Status is the execution outcome: ok | skipped | failed.
	Status string `json:"status"`

	// Summary is a short human-readable description.
	// +optional
	Summary string `json:"summary,omitempty"`

	// Artifacts are local artifacts produced by the node (paths under the
	// shared workdir). These are LOCAL facts, not external side effects.
	// +optional
	Artifacts []Artifact `json:"artifacts,omitempty"`

	// Claims are the reference-backed facts produced or observed by this node.
	// Each Claim references a declared External System Binding.
	// +optional
	Claims []Claim `json:"claims,omitempty"`

	// Payload is the node-type-specific Node Payload (opaque to the kernel).
	// +optional
	Payload []byte `json:"payload,omitempty"`

	// Provenance records who/what caused this execution.
	// +optional
	Provenance Provenance `json:"provenance,omitempty"`

	// References are evidence links (commits, issue comments, wiki pages,
	// cluster resource conditions) attached to this result.
	// +optional
	References []EvidenceReference `json:"references,omitempty"`

	// ProducedAt is when the node finished producing this envelope.
	// +optional
	ProducedAt metav1.Time `json:"producedAt,omitempty"`

	// DurationMs is how long the node execution took. Zero for instantaneous
	// nodes or envelopes synthesized before this field existed. Enables
	// per-step timing views (issue #298).
	// +optional
	DurationMs int64 `json:"durationMs,omitempty"`
}

// Claim is a typed, reference-backed statement describing an observable
// external fact produced or observed by a node (ADR-0004).
type Claim struct {
	// Type is the canonical claim type, conventionally
	// "<surface>.<object>.<verb>", e.g.:
	//   repository.commit.created
	//   issue-tracker.comment.created
	//   wiki.page.updated
	//   review.comment.created
	//   release.tag.created
	Type string `json:"type"`

	// Binding is the ExternalSystemBinding.Name this claim is asserted against.
	// Must reference a binding declared on the Workflow/Attempt.
	Binding string `json:"binding"`

	// ExternalID is the durable identifier for the claimed fact: commit SHA,
	// issue number, page path, tag name, cluster resource namespace/name, etc.
	ExternalID string `json:"externalID"`

	// URL is an optional permalink to the claimed artifact.
	// +optional
	URL string `json:"url,omitempty"`

	// TrustClass is the authority level of this claim (ClaimTrust* constant).
	// Claims from non-deterministic nodes start observed; only deterministic
	// validation may promote them to validated.
	TrustClass string `json:"trustClass"`

	// ValidatedBy is the node ID of the deterministic validator that promoted
	// this claim to ClaimTrustValidated (empty while observed).
	// +optional
	ValidatedBy string `json:"validatedBy,omitempty"`
}

// Artifact is a local artifact produced by a node.
type Artifact struct {
	// Name is a short label for the artifact.
	Name string `json:"name"`

	// Path is the path under the shared workdir.
	Path string `json:"path"`

	// Hash is an optional content hash for dedup/detect.
	// +optional
	Hash string `json:"hash,omitempty"`
}

// EvidenceReference links an orchestration result to an external artifact
// (ADR-0005). External systems keep their native artifacts; Harmostes links to
// them through these references in its Canonical Orchestration History.
type EvidenceReference struct {
	// Binding is the ExternalSystemBinding.Name the evidence lives on.
	Binding string `json:"binding"`

	// Kind is the canonical kind of evidence: commit | issue-comment |
	// wiki-page | review-comment | release | resource-condition.
	Kind string `json:"kind"`

	// Identifier is the durable id (SHA, issue+comment id, page path, etc.).
	Identifier string `json:"identifier"`

	// URL is an optional permalink.
	// +optional
	URL string `json:"url,omitempty"`
}

// Provenance records the origin of a node execution / attempt.
type Provenance struct {
	// TriggeredBy is the actor that caused the execution: the owner/username,
	// or "system" for autonomous triggers.
	// +optional
	TriggeredBy string `json:"triggeredBy,omitempty"`

	// TriggerSource is the trigger channel: webhook | schedule | controller |
	// manual.
	// +optional
	TriggerSource string `json:"triggerSource,omitempty"`

	// ProducedAt is when the execution happened.
	// +optional
	ProducedAt metav1.Time `json:"producedAt,omitempty"`
}
