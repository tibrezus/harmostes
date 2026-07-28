// Package v1alpha1 — ConnectionProfile CRD (ADR-0003).
//
// A ConnectionProfile is a trusted internal configuration entry that defines
// HOW Harmostes speaks to a host family (GitHub / GitLab / Forgejo / Codeberg /
// generic HTTP): API base URL, webhook verification mode, auth transport. It is
// the per-host-family mechanics, kept out of Workflow specs (which declare only
// *which surfaces* they bind to). Changed only through development or trusted
// internal config — not by workflow authors or nodes at runtime.
//
// Bindings reference a profile by name; the profile makes a host family
// addressable. Multiple bindings (repo, issues, wiki) on the same host may
// share one profile.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// HostFamily — the small set of host families Harmostes speaks to natively.
const (
	HostFamilyGitHub   = "github"
	HostFamilyGitLab   = "gitlab"
	HostFamilyForgejo  = "forgejo"
	HostFamilyCodeberg = "codeberg"
	HostFamilyGeneric  = "generic-http" // arbitrary HTTP/git host without a dedicated profile
)

// Auth transport modes (how a binding's credential is presented).
const (
	AuthBearerHeader = "bearer-header" // Authorization: Bearer <token>
	AuthSecretHeader = "secret-header" // a named header carries a shared secret/token
	AuthBasic        = "basic"         // HTTP basic auth
)

// Webhook verification modes.
const (
	WebhookVerifyHMACSHA256Header  = "hmac-sha256-header"  // GitHub/Forgejo-style X-...-Signature: sha256=...
	WebhookVerifySecretTokenHeader = "secret-token-header" // GitLab-style X-Gitlab-Token equality
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cprofile
// +kubebuilder:printcolumn:name=Host-family,type=string,JSONPath=.spec.hostFamily
// +kubebuilder:printcolumn:name=API,type=string,JSONPath=.spec.apiBaseURL
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp

// ConnectionProfile defines how Harmostes speaks to a host family.
type ConnectionProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionProfileSpec   `json:"spec,omitempty"`
	Status ConnectionProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionProfileList is a list of ConnectionProfiles.
type ConnectionProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConnectionProfile `json:"items"`
}

// ConnectionProfileSpec declares a host family's mechanics.
type ConnectionProfileSpec struct {
	// HostFamily is the canonical host family (HostFamily* constant).
	HostFamily string `json:"hostFamily"`

	// APIBaseURL is the base URL for API calls (no trailing slash). For GitHub:
	// "https://api.github.com". For a self-hosted Forgejo: its API root.
	APIBaseURL string `json:"apiBaseURL"`

	// WebBaseURL is the base URL for web/permalink URLs (e.g. issue/PR pages).
	// May equal APIBaseURL for hosts that unify them.
	// +optional
	WebBaseURL string `json:"webBaseURL,omitempty"`

	// AuthTransport is how the binding credential is presented (Auth* constant).
	AuthTransport string `json:"authTransport"`

	// AuthHeaderName is the header name when AuthTransport == AuthSecretHeader.
	// Ignored for bearer/basic.
	// +optional
	AuthHeaderName string `json:"authHeaderName,omitempty"`

	// WebhookVerify is the signature verification mode for inbound webhooks
	// from this host family (WebhookVerify* constant).
	// +optional
	WebhookVerify string `json:"webhookVerify,omitempty"`

	// WebhookSecretRef names the Kubernetes Secret holding the webhook signing
	// secret (key "secret") used to verify inbound signatures for this family.
	// +optional
	WebhookSecretRef *SecretRef `json:"webhookSecretRef,omitempty"`
}

// ConnectionProfileStatus reports reachability/health of the profile.
type ConnectionProfileStatus struct {
	// ObservedGeneration is the generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Ready is true when the profile has been validated as reachable/authed.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message is a human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions are standard k8s conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy
// ---------------------------------------------------------------------------

func (in *ConnectionProfile) DeepCopyInto(out *ConnectionProfile) { deepCopy(in, out) }
func (in *ConnectionProfile) DeepCopy() *ConnectionProfile {
	if in == nil {
		return nil
	}
	out := new(ConnectionProfile)
	deepCopy(in, out)
	return out
}
func (in *ConnectionProfile) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ConnectionProfileList) DeepCopyInto(out *ConnectionProfileList) { deepCopy(in, out) }
func (in *ConnectionProfileList) DeepCopy() *ConnectionProfileList {
	if in == nil {
		return nil
	}
	out := new(ConnectionProfileList)
	deepCopy(in, out)
	return out
}
func (in *ConnectionProfileList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*ConnectionProfile)(nil)
	_ runtime.Object = (*ConnectionProfileList)(nil)
)

// ConnectionProfileResource returns the GroupResource for ConnectionProfile.
func ConnectionProfileResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource("connectionprofiles").GroupResource()
}
