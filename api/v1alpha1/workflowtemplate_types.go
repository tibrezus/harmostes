// Package v1alpha1 — WorkflowTemplate CRD.
//
// A WorkflowTemplate carries the common structure (prepare → agent+gate →
// deploy defaults) that Workflows can inherit via spec.templateRef. Templates
// are declared as YAML (GitOps) — the UI discovers them from the cluster and
// renders an auto-generated graph showing what each template does.
//
// This is the Kubernetes-native replacement for the hardcoded gateCatalog:
// adding a new workflow archetype is a YAML change, not a code change.
package v1alpha1

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wftemplate
// +kubebuilder:printcolumn:name=Description,type=string,JSONPath=.spec.description
// +kubebuilder:printcolumn:name=Prepare,type=string,JSONPath=.spec.prepare.plugin.name
// +kubebuilder:printcolumn:name=Gate,type=string,JSONPath=.spec.agent.gate.plugin.name
// +kubebuilder:printcolumn:name=Deploy,type=string,JSONPath=.spec.deploy.plugin.name
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp

// WorkflowTemplate defines the common structure for a class of Workflows.
type WorkflowTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkflowTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// TemplateSourceSpec points at the repo a template is authored in.
// Forge selects the REST dialect (the bridge's client surface); Repo is
// "owner/name"; Path is the template document INSIDE the repo — a standalone
// WorkflowTemplate YAML, not a chart-values fragment. Branch defaults to
// "main". SecretRef optionally names the Secret holding the HMAC secret a
// push webhook must sign with for landing events (absent = the template
// accepts no landings — fail closed, an unsigned landing is untrusted).
type TemplateSourceSpec struct {
	Forge     string     `json:"forge"`               // "github" | "gitlab"
	Repo      string     `json:"repo"`                // "owner/name"
	Path      string     `json:"path"`                // template document inside the repo
	Branch    string     `json:"branch,omitempty"`    // default "main"
	SecretRef *SecretRef `json:"secretRef,omitempty"` // HMAC secret for landing webhooks
}

// Validate rejects a source that cannot be proposed against or correlated:
// an unknown forge (the bridge has exactly two clients), a repo without the
// owner/name shape, or an empty path (there would be nothing to commit to).
func (s TemplateSourceSpec) Validate() error {
	switch s.Forge {
	case "github", "gitlab":
	case "":
		return fmt.Errorf("template source: forge is required (github|gitlab)")
	default:
		return fmt.Errorf("template source: unknown forge %q (github|gitlab)", s.Forge)
	}
	if s.Repo == "" || strings.Count(s.Repo, "/") != 1 || strings.HasPrefix(s.Repo, "/") || strings.HasSuffix(s.Repo, "/") {
		return fmt.Errorf("template source: repo %q must be \"owner/name\"", s.Repo)
	}
	if s.Path == "" {
		return fmt.Errorf("template source: path is required (the template document inside the repo)")
	}
	if s.SecretRef != nil && (s.SecretRef.Name == "" || s.SecretRef.Key == "") {
		return fmt.Errorf("template source: secretRef needs both name and key")
	}
	return nil
}

// WorkflowTemplateList is a list of WorkflowTemplates.
type WorkflowTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowTemplate `json:"items"`
}

// WorkflowTemplateSpec defines the default prepare/agent/gate/deploy structure
// that Workflows using this template inherit.
type WorkflowTemplateSpec struct {
	// Description is a human-readable explanation of what this template does.
	Description string `json:"description"`

	// Scope declares the instance-level parameters this template consumes
	// from Workflow spec.config. It is the single owner of the instance
	// configuration surface: the UI creation form renders from it, and the
	// creation handler builds spec.config exclusively from its params — a
	// template and its prepare plugin define their own dialect end-to-end,
	// so adding a template never touches UI code.
	Scope []ScopeParam `json:"scope,omitempty"`

	// Prepare defines the prepare plugin that produces the working artifact.
	Prepare PrepareSpec `json:"prepare"`

	// Agent defines the agent configuration (skill, gate, maxFixes).
	Agent AgentSpec `json:"agent"`

	// Deploy defines the deploy plugin that ships the result.
	Deploy DeploySpec `json:"deploy"`

	// ReviewReady optionally configures the event-armed Review-Ready Gate
	// (ADR-0006): the deterministic trigger decision for adversarial PR
	// review (label ∧ merge-rule contexts green at the head SHA). Templates
	// for pr-review workflows set this; instances inherit it.
	ReviewReady *ReviewReadySpec `json:"reviewReady,omitempty"`

	// Cache declares the shared PVC caches the per-Attempt Job mounts
	// (#336): a warm GOCACHE/GOMODCACHE turns a cold `go test -race`
	// (minutes: module downloads + full race rebuild) into ~1 min. The
	// template is the natural home — a fleet's review workflows share one
	// claim, and instances inherit it whole-struct like ReviewReady.
	Cache *CacheSpec `json:"cache,omitempty"`

	// Source declares the template's own repo (three-tier 1c, ADR-0011
	// amendment): one source of truth — the repo path the template is
	// authored, versioned, and proposed against. Optional (inert by
	// default): unset = the template lives where the environment's
	// HARMOSTES_TEMPLATE_SOURCE points (the kernel chart values, the
	// pre-three-tier shape). Set = the MR round-trip bridge proposes edits
	// as a standalone template document in THIS repo, and signed push
	// events to it land as TemplateRevision commit identity.
	Source *TemplateSourceSpec `json:"source,omitempty"`

	// PluginsConfigMapRef names the ConfigMap carrying THIS template's own
	// plugin payload (three-tier 1c): an explicit ref — no naming
	// conventions, no namespace-wide label coupling. The dispatcher stamps
	// it into the attempt Job; the materialize init container projects it
	// into the /plugins mount next to the label-aggregated set (deduped by
	// ConfigMap name), where the resolver's /plugins/<cm>/<key> contract
	// finds it like any plugin. Empty = the aggregation/label path alone.
	PluginsConfigMapRef string `json:"pluginsConfigMapRef,omitempty"`

	// Sessions declares the persistent pi-session lineage claim the
	// per-Attempt Job mounts (ADR-0010 follow-up): per-PR compacted
	// context (and SoL-Pi data) survives across attempt Jobs until TTL.
	// Mounting is a deployment fact (the RWX claim must exist) — the
	// template references it, the environment renders it; instances
	// inherit whole-struct like Cache.
	Sessions *SessionsSpec `json:"sessions,omitempty"`

	// Attachments declares the scoped shared storage components the
	// per-Attempt Jobs mount (#516 C2, #704): sandboxes stay isolated
	// per attempt, while named stores are shared at the declared scope
	// (workflow | repository | attempt). The template references the
	// deployment fact (the claim must exist); instances inherit
	// whole-struct like Sessions.
	Attachments []SharedAttachment `json:"attachments,omitempty"`
}

// ScopeParam is one instance-level configuration parameter.
type ScopeParam struct {
	// Name is the key under spec.config the parameter is stored at (and the
	// form field name the UI posts).
	Name string `json:"name"`

	// Kind is the value shape: "string" (single value) or "list" (comma-
	// separated in the form, stored as a JSON array).
	Kind string `json:"kind"` // "string" | "list"

	// Label is the human-readable form label.
	Label string `json:"label,omitempty"`

	// Description is the per-field hint shown under the form input.
	Description string `json:"description,omitempty"`

	// Default is the value used when the form field is left empty (a list
	// default is comma-separated, mirroring the form).
	Default string `json:"default,omitempty"`
}

// ---------------------------------------------------------------------------
// runtime.Object + DeepCopy
// ---------------------------------------------------------------------------

func (in *WorkflowTemplate) DeepCopyInto(out *WorkflowTemplate) { deepCopy(in, out) }
func (in *WorkflowTemplate) DeepCopy() *WorkflowTemplate {
	if in == nil {
		return nil
	}
	out := new(WorkflowTemplate)
	deepCopy(in, out)
	return out
}
func (in *WorkflowTemplate) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *WorkflowTemplateList) DeepCopyInto(out *WorkflowTemplateList) { deepCopy(in, out) }
func (in *WorkflowTemplateList) DeepCopy() *WorkflowTemplateList {
	if in == nil {
		return nil
	}
	out := new(WorkflowTemplateList)
	deepCopy(in, out)
	return out
}
func (in *WorkflowTemplateList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// Compile-time assertions.
var (
	_ runtime.Object = (*WorkflowTemplate)(nil)
	_ runtime.Object = (*WorkflowTemplateList)(nil)
)

// WorkflowTemplateResource returns the GroupResource for WorkflowTemplate.
func WorkflowTemplateResource() schema.GroupResource {
	return SchemeGroupVersion.WithResource("workflowtemplates").GroupResource()
}
