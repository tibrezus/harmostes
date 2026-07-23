// Package v1alpha1 contains the Go types for the harmostes.dev Workflow CRD.
//
// These mirror config/crd/workflows.harmostes.dev.yaml. DeepCopyObject is
// implemented via a JSON round-trip for now (correct for these plain public
// structs at v1alpha1's low reconcile volume); swap for controller-generated
// DeepCopy once controller-gen is wired into the build.
package v1alpha1

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "harmostes.dev"
const Version = "v1alpha1"

// SchemeGroupVersion is the group:version used to register these types.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// SchemeBuilder registers the types with a runtime scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
var AddToScheme = SchemeBuilder.AddToScheme

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &Workflow{}, &WorkflowList{})
	scheme.AddKnownTypes(SchemeGroupVersion, &Pipeline{}, &PipelineList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hw
// +kubebuilder:printcolumn:name=Source,type=string,JSONPath=.spec.source.kind
// +kubebuilder:printcolumn:name=Prepare,type=string,JSONPath=.spec.prepare.plugin.name
// +kubebuilder:printcolumn:name=Gate,type=string,JSONPath=.spec.agent.gate.plugin.name
// +kubebuilder:printcolumn:name=Deploy,type=string,JSONPath=.spec.deploy.plugin.name
// +kubebuilder:printcolumn:name=Gate-status,type=string,JSONPath=.status.gateStatus
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp

// Workflow is a declarative LLM-automation pipeline: monitor a source → prepare
// (deterministic) → agent (LLM + gate, warm-session feedback loop) → deploy
// (deterministic) → state. See ARCHITECTURE.md.
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowSpec   `json:"spec,omitempty"`
	Status WorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowList is a list of Workflows.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

// WorkflowSpec declares one harmostes pipeline.
//
// A Workflow can be defined in two equivalent forms:
//
//  1. **Declarative** (default): the fixed prepare → agent → deploy pipeline.
//     Populate Prepare, Agent, and Deploy. The worker runs worker.Run().
//     This is what all existing production workflows use.
//
//  2. **Graph-native**: an explicit directed graph of nodes + edges. Populate
//     Graph with nodes (any type from the node executor registry) and edges
//     (sequential, conditional, loop-back). The worker runs the graph
//     executor. This allows arbitrary pipeline shapes — branches, parallel
//     paths, custom node types (dapr-state, vela-app, etc.).
//
// If Graph is non-nil, the worker uses the graph executor and ignores the
// Prepare/Agent/Deploy fields. The Source field is always used (for trigger
// configuration: git, schedule, webhook).
type WorkflowSpec struct {
	Source        SourceSpec         `json:"source"`
	WorkspaceRepo *WorkspaceRepoSpec `json:"workspaceRepo,omitempty"` // the repo the pipeline operates on (prepare populates, agent edits, deploy pushes)
	Prepare       PrepareSpec        `json:"prepare,omitempty"`
	Agent         AgentSpec          `json:"agent,omitempty"`
	Deploy        DeploySpec         `json:"deploy,omitempty"`
	Graph         *GraphSpec         `json:"graph,omitempty"` // graph-native mode: explicit nodes + edges (overrides Prepare/Agent/Deploy)
	Events        *EventsSpec        `json:"events,omitempty"`
	Cache         *CacheSpec         `json:"cache,omitempty"`
	Scaling       *ScalingSpec       `json:"scaling,omitempty"`
	Disabled      bool               `json:"disabled,omitempty"`
}

// WorkspaceRepoSpec is the repo a pipeline operates on. The worker fetches it
// into the workdir before running; prepare/agent/gate/deploy all work there; the
// deploy plugin pushes it. For llm-wiki this is the wiki repo; for fork-
// maintenance, the fork.
type WorkspaceRepoSpec struct {
	URL      string     `json:"url"`                // git URL (may embed a token for HTTPS)
	Branch   string     `json:"branch"`             // branch to fetch + (by default) push
	Dir      string     `json:"dir,omitempty"`      // checkout location under WORKDIR (default "repo")
	Shadow   string     `json:"shadow,omitempty"`   // if set, push to this branch instead (parallel/dry-run)
	TokenRef *SecretRef `json:"tokenRef,omitempty"` // per-user git token (Phase C). If set, overrides the shared cluster secret.
}

// SourceSpec is what the workflow monitors.
type SourceSpec struct {
	Kind     string       `json:"kind"`           // git | schedule | event | webhook
	Repo     string       `json:"repo,omitempty"` // Flux GitRepository name, or direct URL
	Branch   string       `json:"branch,omitempty"`
	Revision string       `json:"revision,omitempty"` // pin (git)
	Schedule string       `json:"schedule,omitempty"` // cron (schedule)
	Topic    string       `json:"topic,omitempty"`    // inbound event (event)
	Language string       `json:"language,omitempty"` // lc4: go/zig/… (passed to prepare)
	Fork     *ForkSource  `json:"fork,omitempty"`     // fork-maintenance: the fork to sync into
	Webhook  *WebhookSpec `json:"webhook,omitempty"`  // webhook trigger config (HMAC secret + host URL)
}

// ForkSource identifies the fork a fork-maintenance workflow keeps in sync.
type ForkSource struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// WebhookSpec configures a webhook trigger for a workflow. When enabled,
// external git hosts (GitHub, GitLab, Forgejo) send push events to the
// controller's webhook endpoint, and the controller schedules an immediate run.
//
// Two modes:
//  1. **direct** (testing): HMAC secret specified directly in spec (NOT recommended)
//  2. **secretRef** (production): Secret reference (from Kubernetes Secret)
//
// Controller resolves secretRef and passes secret to HMAC verification.
// This keeps secrets out of git (GitOps-friendly).
type WebhookSpec struct {
	Secret    string     `json:"secret,omitempty"`    // HMAC secret (testing only, NOT recommended for production)
	SecretRef *SecretRef `json:"secretRef,omitempty"` // Secret reference (production)
	URL       string     `json:"url"`                 // Git host URL (for signature verification)
}

// SecretRef references a Kubernetes Secret by name + key.
type SecretRef struct {
	Name string `json:"name"` // Secret name
	Key  string `json:"key"`  // Secret key
}

// PrepareSpec runs a deterministic plugin that produces an artifact.
type PrepareSpec struct {
	Plugin PluginRef       `json:"plugin"`
	Output string          `json:"output,omitempty"` // artifact path/branch/ref produced
	Detect string          `json:"detect,omitempty"` // changed | conflict | always
	Config json.RawMessage `json:"config,omitempty"` // arbitrary config passed to the plugin as HARMOSTES_SPEC
}

// AgentSpec is the framework-native LLM step (NOT a plugin).
type AgentSpec struct {
	Enabled      *bool        `json:"enabled,omitempty"`  // nil/true = run, false = skip (deterministic-only)
	Model        string       `json:"model"`              // e.g. zai/glm-5.2
	Skill        string       `json:"skill"`              // path to SKILL.md
	Tools        []string     `json:"tools,omitempty"`    // tool allowlist
	TaskTemplate TaskTemplate `json:"taskTemplate"`       // the interpretive task
	Gate         GateRef      `json:"gate"`               // validation plugin
	MaxFixes     int          `json:"maxFixes,omitempty"` // default 3
	Timeout      int          `json:"timeout,omitempty"`  // seconds, default 1800
	Scope        string       `json:"scope,omitempty"`    // optional task scope override
}

// TaskTemplate names the prompt text for the agent (lives in a ConfigMap).
type TaskTemplate struct {
	Name      string `json:"name"`
	ConfigMap string `json:"configMap,omitempty"`
	Key       string `json:"key,omitempty"`
}

// GateRef references the gate plugin (exit 0 = green; stderr = feedback).
type GateRef struct {
	Plugin PluginRef `json:"plugin"`
}

// DeploySpec runs a deterministic plugin that publishes a green result.
type DeploySpec struct {
	Plugin PluginRef `json:"plugin"`
}

// PluginRef names a plugin and how to resolve it (built-in | image | configMap).
type PluginRef struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	ConfigMap string   `json:"configMap,omitempty"`
	Args      []string `json:"args,omitempty"`
}

// EventsSpec names the Dapr pub/sub topics the framework publishes.
type EventsSpec struct {
	OnPrepare  string `json:"onPrepare,omitempty"`
	OnResolved string `json:"onResolved,omitempty"`
	OnFailed   string `json:"onFailed,omitempty"`
	OnDeployed string `json:"onDeployed,omitempty"`
}

// CacheSpec declares which caches the worker should mount.
type CacheSpec struct {
	PVC string `json:"pvc,omitempty"`
	Git bool   `json:"git,omitempty"`
	Go  bool   `json:"go,omitempty"`
	NPM bool   `json:"npm,omitempty"`
}

// ScalingSpec selects the trigger model.
type ScalingSpec struct {
	Kind     string `json:"kind,omitempty"` // keda-scaledjob | cronjob
	Schedule string `json:"schedule,omitempty"`
}

// WorkflowStatus is reported by the controller.
type WorkflowStatus struct {
	ObservedGeneration    int64              `json:"observedGeneration,omitempty"`
	LastProcessedRevision string             `json:"lastProcessedRevision,omitempty"`
	LastAgentCommit       string             `json:"lastAgentCommit,omitempty"`
	LastRigHash           string             `json:"lastRigHash,omitempty"` // sha256 of the last processed RIG (deterministic skip)
	GateStatus            string             `json:"gateStatus,omitempty"`  // green | failed | unknown
	LastRunAt             metav1.Time        `json:"lastRunAt,omitempty"`
	Message               string             `json:"message,omitempty"`
	Conditions            []metav1.Condition `json:"conditions,omitempty"`
}

// ---------------------------------------------------------------------------
// DeepCopyObject (JSON round-trip — correct for these plain public structs;
// see package doc re: swapping for controller-generated DeepCopy later).
// ---------------------------------------------------------------------------

func (in *Workflow) DeepCopyInto(out *Workflow) { deepCopy(in, out) }
func (in *Workflow) DeepCopy() *Workflow {
	if in == nil {
		return nil
	}
	out := new(Workflow)
	deepCopy(in, out)
	return out
}
func (in *Workflow) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *WorkflowList) DeepCopyInto(out *WorkflowList) { deepCopy(in, out) }
func (in *WorkflowList) DeepCopy() *WorkflowList {
	if in == nil {
		return nil
	}
	out := new(WorkflowList)
	deepCopy(in, out)
	return out
}
func (in *WorkflowList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// deepCopy round-trips src through JSON into dst. dst must be the same type as src.
func deepCopy(src, dst any) {
	b, err := json.Marshal(src)
	if err != nil {
		panic(fmt.Sprintf("harmostes: DeepCopy marshal failed for %T: %v", src, err))
	}
	if err := json.Unmarshal(b, dst); err != nil {
		// dst may be a partially-initialized zero value whose type differs only in
		// wrapping (e.g. *T vs T); the package only ever calls deepCopy on matching
		// types, so this should be unreachable.
		panic(fmt.Sprintf("harmostes: DeepCopy unmarshal failed for %T: %v", dst, err))
	}
}

// Compile-time assertions that the types implement runtime.Object.
var (
	_ runtime.Object = (*Workflow)(nil)
	_ runtime.Object = (*WorkflowList)(nil)
)
