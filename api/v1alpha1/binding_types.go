// Package v1alpha1 — External System Bindings (ADR-0003).
//
// A Workflow interacts with external systems — GitHub, GitLab, Forgejo,
// Codeberg — and with their distinct surfaces (repositories, issue trackers,
// wikis, review systems, releases, webhook origins). These types model *which
// external surfaces a workflow is allowed to touch, and how*.
//
// Key invariants (ADR-0003):
//   - Bindings are surface-specific, not host-wide. A repo binding and an issue
//     tracker binding on the same host are distinct bindings.
//   - Each binding declares a Canonical Surface Kind from a small kernel-
//     recognized vocabulary, so the kernel, UI, and validators share semantics.
//   - The set of declared bindings is the Binding Authority Boundary and is
//     STATIC: runtime may create objects within a binding, never new bindings.
//   - Each binding references a trusted Connection Profile that defines how
//     Harmostes speaks to that host family (transport/auth/webhook/API).
//   - Nodes request Surface Capabilities against bindings; the kernel enforces
//     Capability Policy before execution. Binding presence != blanket access.

package v1alpha1

// (This file holds only the binding-related value types. The ExternalSystemBinding
// lives inside WorkflowSpec (optional) and is snapshotted into AttemptSpec; it is
// not itself a top-level CRD. ConnectionProfile IS a CRD — see
// connectionprofile_types.go.)

// Canonical Surface Kind — the small kernel-recognized vocabulary of external
// surface categories (ADR-0003). Values are stable identifiers, not host names.
const (
	SurfaceKindRepository    = "repository"     // git repo (clone/push/fetch)
	SurfaceKindIssueTracker  = "issue-tracker"  // issues / tickets
	SurfaceKindWiki          = "wiki"           // wiki / docs surface
	SurfaceKindReview        = "review"         // pull/merge requests + review threads
	SurfaceKindRelease       = "release"        // release artifacts / tags
	SurfaceKindWebhookOrigin = "webhook-origin" // inbound webhook signing source
)

// BindingRole — the purpose a binding serves in a Workflow (ADR-0003). One
// surface kind may be used under several roles (e.g. a repository surface can
// be the sourceRepo, the workspaceRepo, or a releaseTarget).
const (
	BindingRoleSourceRepo    = "sourceRepo"
	BindingRoleWorkspaceRepo = "workspaceRepo"
	BindingRoleIssueTracker  = "issueTracker"
	BindingRoleWiki          = "wiki"
	BindingRoleReview        = "review"
	BindingRoleReleaseTarget = "releaseTarget"
	BindingRoleWebhookOrigin = "webhookOrigin"
)

// ExternalSystemBinding is a named reference from a Workflow to a specific
// external system surface. Distinct from "platform": a host with repo + issues
// + wiki yields up to three bindings, each with its own Surface Contract.
type ExternalSystemBinding struct {
	// Name is the reference key nodes and objectives use to address this
	// binding (e.g. "sourceRepo", "issueTracker"). Unique within a Workflow.
	Name string `json:"name"`

	// BindingRole is the role this binding plays in the Workflow
	// (BindingRole* constant). Determines how the kernel treats it.
	BindingRole string `json:"bindingRole"`

	// SurfaceKind is the Canonical Surface Kind (SurfaceKind* constant). The
	// kernel, validators, and UI all key off this, not host-specific quirks.
	SurfaceKind string `json:"surfaceKind"`

	// ConnectionProfile names the trusted ConnectionProfile CR that defines how
	// Harmostes speaks to this host family. Workflows declare *which surface*;
	// profiles define *how to talk to it*.
	ConnectionProfile string `json:"connectionProfile"`

	// Target is the surface-specific object identity for this binding
	// (host + object + optional branch). Surface-specific extras live in the
	// open map.
	Target BindingTarget `json:"target"`

	// Granted is the set of Surface Capability tokens this binding authorizes
	// (e.g. "repository.read", "repository.push", "issue-tracker.comment.write").
	// Nodes declare what they Require; the kernel enforces Require ⊆ Granted.
	// This is the per-binding authority scope.
	// +optional
	Granted []string `json:"granted,omitempty"`

	// AuthRef is the Kubernetes Secret holding the credential for this binding.
	// +optional
	AuthRef *SecretRef `json:"authRef,omitempty"`
}

// BindingTarget identifies the object a binding points at on its host.
type BindingTarget struct {
	// Host is the host identifier, e.g. "github.com", "gitlab.com", a Forgejo
	// instance FQDN. The Connection Profile refines how this host is spoken to.
	Host string `json:"host"`

	// Object is the surface-specific identifier: owner/repo for a repository,
	// project path for a wiki, owner/repo#issue namespace for an issue tracker,
	// cluster namespace/name for a release target, etc.
	Object string `json:"object"`

	// Branch is the branch/ref for repository surfaces. Empty for non-repo
	// surfaces.
	// +optional
	Branch string `json:"branch,omitempty"`

	// Extra carries surface-specific identity fields the kernel does not parse
	// (e.g. wiki mode, review thread id, webhook content type). Preserved as
	// arbitrary JSON so the schema stays stable across host families.
	// +optional
	Extra map[string]string `json:"extra,omitempty"`
}

// CapabilityRequirement is a node's request for a Surface Capability against a
// declared binding. The kernel enforces, before execution, that the named
// binding exists and grants the requested capability (ADR-0003).
type CapabilityRequirement struct {
	// Binding is the ExternalSystemBinding.Name this requirement targets.
	Binding string `json:"binding"`

	// Capability is the capability token requested, e.g.
	// "repository.read", "repository.push", "issue-tracker.comment.write",
	// "wiki.page.write", "webhook-origin.verify".
	Capability string `json:"capability"`
}
