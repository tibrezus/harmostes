// Package fixture seeds a deterministic synthetic world through the same
// construction path production uses (ui.New over a controller-runtime
// client.Client). It powers three consumers at once — `harmostes-ui -fixture`
// local development, the goquery component tests, and (later) the Playwright
// E2E target — so all three exercise identical data. Milestone ⑤ of #290.
// The world is deliberately synthetic: nothing here copies a real
// repository's workflow spec.
package fixture

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakectrl "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
	"github.com/tibrezus/harmostes/internal/ui"
)

// DevUser is the identity the fixture world belongs to. `harmostes-ui
// -fixture` defaults the dev-user bypass to it when unset; tests send it as
// the X-Harmostes-Dev-User header.
const DevUser = "fixture-user"

// base is the fixture clock origin: process start, truncated to the hour.
// Every world timestamp is an offset from it, so the world stays inside the
// runs list' default 24h activity window forever — a fixed date would age
// the fixture out of /runs one day after the commit (the #304 review proved
// it with a 31-day clock shift; the first attempt at this fix was itself
// clobbered by a stale in-memory copy — pinned by TestComponent_RunsList_
// DefaultWindow so it cannot silently regress again).
var base = time.Now().UTC().Truncate(time.Hour)

// t offsets base by minutes+seconds and returns a fresh metav1.Time.
func t(min, sec int) metav1.Time {
	return metav1.NewTime(base.Add(time.Duration(min)*time.Minute + time.Duration(sec)*time.Second))
}

// worldYAML declares the fixture workflows as YAML — the same shape an
// operator would check into k8s-config — so the file doubles as tutorial
// material for the Workflow CR.
//
//go:embed world.yaml
var worldYAML []byte

// Objects returns every non-Attempt object in the fixture world (the
// Workflows), parsed from the embedded YAML and retargeted to namespace.
func Objects(namespace string) ([]ctrlclient.Object, error) {
	docs := strings.Split(string(worldYAML), "\n---\n")
	var objs []ctrlclient.Object
	for i, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var wf v1alpha1.Workflow
		if err := yaml.Unmarshal([]byte(doc), &wf); err != nil {
			return nil, fmt.Errorf("fixture world doc %d: %w", i, err)
		}
		wf.Namespace = namespace
		if wf.Labels == nil {
			wf.Labels = map[string]string{}
		}
		wf.Labels[v1alpha1.OwnerLabel] = ui.DevOwnerPrefix + DevUser
		objs = append(objs, &wf)
	}
	return objs, nil
}

// envelope builds a node-result envelope with a measured duration (the
// timing waterfall's input).
func envelope(nodeID, status string, at metav1.Time, durationSec int64) v1alpha1.NodeResultEnvelope {
	return v1alpha1.NodeResultEnvelope{
		NodeID:     nodeID,
		Status:     status,
		ProducedAt: at,
		DurationMs: durationSec * 1000,
	}
}

// prReviewAttempt builds a review-class attempt skeleton against the
// pr-review-demo workflow.
func prReviewAttempt(namespace, name, pr string, created metav1.Time) *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			Labels:            map[string]string{v1alpha1.OwnerLabel: ui.DevOwnerPrefix + DevUser},
			CreationTimestamp: created,
		},
		Spec: v1alpha1.AttemptSpec{
			Objective: v1alpha1.ObjectiveSpec{
				Kind:           v1alpha1.ObjectiveKindPRReview,
				PrimarySubject: v1alpha1.Subject{Binding: "github", Object: pr},
			},
			WorkflowRef: namespace + "/pr-review-demo",
			Owner:       DevUser,
		},
	}
}

// mergeSyncAttempt builds a deterministic (merge-sync) attempt skeleton.
func mergeSyncAttempt(namespace, name string, created metav1.Time) *v1alpha1.Attempt {
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			Labels:            map[string]string{v1alpha1.OwnerLabel: ui.DevOwnerPrefix + DevUser},
			CreationTimestamp: created,
		},
		Spec: v1alpha1.AttemptSpec{
			Objective: v1alpha1.ObjectiveSpec{
				Kind:           "merge-sync",
				PrimarySubject: v1alpha1.Subject{Binding: "github", Object: "demo-rezuscloud/harmostes"},
			},
			WorkflowRef: namespace + "/merge-sync-demo",
			Owner:       DevUser,
		},
	}
}

// Attempts returns the fixture attempts. Three states, one narrative:
//
//  1. pr-review-demo / demo-rezuscloud-harmostes#42 — terminal (validated),
//     full ledger including the 13m agent node, so the run graph and timing
//     waterfall render every node state.
//  2. pr-review-demo / demo-rezuscloud-harmostes#43 — mid-flight: prepare
//     has an envelope and the agent run is in flight, so the live position
//     lands on the agent node.
//  3. merge-sync-demo — superseded (a newer targeted state replaced it),
//     exercising the fourth terminal phase.
func Attempts(namespace string) ([]ctrlclient.Object, error) {
	// --- 1. terminal review attempt -------------------------------------
	terminal := prReviewAttempt(namespace, "attempt-pr-review-demo-42a1", "demo-rezuscloud/harmostes#42", t(0, 0))
	terminal.Status.Phase = v1alpha1.AttemptPhaseValidated
	terminal.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(0, 15), 5),
		envelope("agent", "ok", t(13, 25), 780), // the 13m agent node
		envelope("gate", "ok", t(14, 10), 40),
		envelope("deploy", "skipped", t(14, 15), 0),
	}
	terminal.Status.Runs = []v1alpha1.RunRecord{
		{Name: "pr-review-demo-42a1-prepare", StartedAt: t(0, 5), EndedAt: t(0, 15), Phase: "succeeded"},
		{Name: "pr-review-demo-42a1-agent", StartedAt: t(0, 20), EndedAt: t(13, 25), Phase: "succeeded"},
		{Name: "pr-review-demo-42a1-gate", StartedAt: t(13, 30), EndedAt: t(14, 10), Phase: "succeeded"},
	}
	// Claim transitions the Event Timeline's ledger half projects (ADR-0012
	// §4): armed → dispatched → released(consumed). The gate store events
	// carry the release transition itself.
	armT := t(0, 30)
	dispT := t(0, 45)
	terminal.Status.Review = &v1alpha1.ReviewClaimStatus{
		PR: "demo-rezuscloud/harmostes#42", HeadSHA: "b41fb712abcdef",
		Label: "needs-review", ArmedSince: &armT, DispatchedAt: &dispT,
		Released: true, ReleaseReason: "consumed",
	}

	// --- 2. mid-flight review attempt -----------------------------------
	running := prReviewAttempt(namespace, "attempt-pr-review-demo-43c2", "demo-rezuscloud/harmostes#43", t(30, 0))
	running.Status.Phase = v1alpha1.AttemptPhaseReconciling
	running.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(30, 12), 4),
	}
	running.Status.Runs = []v1alpha1.RunRecord{
		{Name: "pr-review-demo-43c2-prepare", StartedAt: t(30, 5), EndedAt: t(30, 12), Phase: "succeeded"},
		{Name: "pr-review-demo-43c2-agent", StartedAt: t(30, 20), Phase: "running"}, // no EndedAt: in flight
	}
	// In-flight claim: armed + dispatched, NOT released — the live position.
	armT2 := t(29, 0)
	dispT2 := t(29, 30)
	running.Status.Review = &v1alpha1.ReviewClaimStatus{
		PR: "demo-rezuscloud/harmostes#43", HeadSHA: "9c02aa01feedbeef",
		Label: "needs-review", ArmedSince: &armT2, DispatchedAt: &dispT2,
	}

	// --- 3. superseded merge-sync attempt -------------------------------
	superseded := mergeSyncAttempt(namespace, "attempt-merge-sync-demo-e5f6", t(60, 0))
	superseded.Status.Phase = v1alpha1.AttemptPhaseSuperseded
	superseded.Status.NodeResults = []v1alpha1.NodeResultEnvelope{
		envelope("prepare", "ok", t(61, 0), 6),
		envelope("deploy", "ok", t(62, 30), 55),
	}
	superseded.Status.Runs = []v1alpha1.RunRecord{
		{Name: "merge-sync-demo-e5f6-prepare", StartedAt: t(60, 50), EndedAt: t(61, 0), Phase: "succeeded"},
		{Name: "merge-sync-demo-e5f6-deploy", StartedAt: t(61, 30), EndedAt: t(62, 30), Phase: "succeeded"},
	}

	return []ctrlclient.Object{terminal, running, superseded}, nil
}

// Scheme returns the full API scheme (v1alpha1 + core types) the fake
// client needs — the same scheme production uses.
func Scheme() *runtime.Scheme {
	return k8s.Scheme()
}

// DevIdentity injects the fixture dev user into requests that carry no
// explicit identity — the zero-setup contract behind `harmostes-ui -fixture`
// (no Authentik, no headers). Wrap Routes() with it in the binary; tests
// wrap it to pin the contract.
//
// PROD HAZARD: this is an auth bypass by construction. It must only ever
// wrap a fixture-mounted Routes(); the production mount is bare Routes()
// (which 401s identity-less requests — pinned by TestComponent_Routes_
// RejectsAnonymous).
func DevIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Authentik-Username") == "" && r.Header.Get("X-Harmostes-Dev-User") == "" {
			r.Header.Set("X-Harmostes-Dev-User", DevUser)
		}
		next.ServeHTTP(w, r)
	})
}

// World is the fixture's assembled runtime: the UI server over the seeded
// in-memory world, plus the handles its middleware stack drives.
type World struct {
	server   *ui.Server
	timeline *FixTimeline
}

// Server returns the UI server (component tests drive it directly).
func (w *World) Server() *ui.Server { return w.server }

// Routes returns the fixture's full handler stack over the UI server's
// routes: the dapr-ingest store growth first (the fake store must see the
// row before the hub wake re-renders), then dev identity. Production mounts
// bare Routes() and never composes either wrapper (both are fixture-only).
func (w *World) Routes() http.Handler {
	return DevIdentity(daprStoreIngest(w.timeline, w.server.Routes()))
}

// daprStoreIngest grows the fixture's fake timeline store from the cloud
// events production's ingress receives. In the fixture there is no Dapr
// sidecar and no store writer, so the ingress IS the store's write path —
// mirroring production, where the events that reach /dapr/events are the
// ones that end up in the store. The event is appended BEFORE delegation so
// the hub wake's re-render already sees the row (SSE convergence, #421).
func daprStoreIngest(tl *FixTimeline, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/dapr/events" {
			body, err := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if err == nil {
				var ce struct {
					Data ui.Event `json:"data"`
				}
				if json.Unmarshal(body, &ce) == nil {
					tl.Ingest(ce.Data)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// NewServer constructs a ui.Server over an in-memory seeded world: the fake
// controller-runtime client carries the fixture objects, the fake clientset
// backs pod-log reads (no pods — log streaming degrades gracefully). DAPR
// stays unwired; the dapr event endpoint still mutates the in-memory world,
// which is exactly what E2E event-injection tests will use.
func NewWorld(namespace string, logger *slog.Logger) (*World, error) {
	scheme := Scheme()

	objs, err := Objects(namespace)
	if err != nil {
		return nil, fmt.Errorf("fixture objects: %w", err)
	}
	atts, err := Attempts(namespace)
	if err != nil {
		return nil, fmt.Errorf("fixture attempts: %w", err)
	}
	var runtimeObjs []runtime.Object
	for _, o := range append(objs, atts...) {
		runtimeObjs = append(runtimeObjs, o.(runtime.Object))
	}

	k8sClient := fakectrl.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(append(runtimeObjs, fixtureExtras(namespace)...)...).
		WithStatusSubresource(&v1alpha1.Attempt{}).
		Build()

	// The clientset is cluster-scoped by design (client-go fakes have no
	// namespace option in v0.31): the namespace reaches it per call —
	// makeLogFetchFunc does Pods(namespace).GetLogs — so the seam stays
	// exactly as production's. No pods are seeded; log streaming degrades.
	var kubeClient kubernetes.Interface = fake.NewSimpleClientset()

	server, err := ui.New(k8sClient, namespace, logger, kubeClient, nil)
	if err != nil {
		return nil, err
	}
	// Fixture servers exist to exercise the full surface including the write
	// path — dev-identity writes are on by construction here (the production
	// binary never enables them; see Server.SetDevWriteEnabled).
	server.SetDevWriteEnabled(true)
	// Template MR bridge (#420): the fixture world proposes against a
	// fictional source — the e2e tier intercepts the route, so only the
	// panel's DOM and the glue run; a real POST fails honestly at the forge.
	server.SetTemplateSource(&ui.TemplateSource{
		Host: "github", Owner: "golden-owner", Repo: "golden-repo",
		BaseBranch: "main", Path: "chart/values.yaml", ValuesKey: "workflowTemplates",
	})
	// Event Timeline (ADR-0012 §4): the seeded fake reader (no sidecar here) —
	// kept as a *FixTimeline so Routes()' ingest middleware can grow it.
	tl := NewTimelineReader()
	server.SetTimelineReader(tl)
	return &World{server: server, timeline: tl}, nil
}

// fixtureExtras seeds the objects the ADR-0012 write-path surfaces read: one
// WorkflowTemplate (the creation form's catalog entry — the fixture workflows
// are graph-native and reference no template) and the two CRDs the schema
// endpoint serves as a projection of the cluster's actual CRDs (ADR-0012 §2).
// The schemas are compact but structurally truthful: type object, a spec
// object, marker descriptions the E2E tier asserts on.
// templateRevisionForFixture mirrors the ui package's revision annotation
// entry shape (rev/description/spec) — the fixture stamps history without
// importing ui internals.
type templateRevisionForFixture struct {
	Rev         int                           `json:"rev"`
	Description string                        `json:"description"`
	Spec        v1alpha1.WorkflowTemplateSpec `json:"spec"`
}

// graphNodeTypeEnum mirrors the workflow CRD's spec.graph.nodes.type enum —
// the palette vocabulary (#417). Kept in lockstep with chart/crds by
// fixture_test.go.
func graphNodeTypeEnum() []apiextensionsv1.JSON {
	names := []string{"plugin", "agent", "gate", "branch", "dapr-state-get", "dapr-state-set", "dapr-publish", "vela-app", "flux-reconcile", "http-call", "human-gate", "external"}
	out := make([]apiextensionsv1.JSON, len(names))
	for i, n := range names {
		b, _ := json.Marshal(n)
		out[i] = apiextensionsv1.JSON{Raw: b}
	}
	return out
}

func fixtureExtras(namespace string) []runtime.Object {
	// The template's head spec mirrors the dev cluster's pr-review shape
	// (fetch → agent+gate → post), and its revisions annotation carries the
	// history the revisions view diffs (#417): r1 was deterministic-only with
	// an older fetch plugin; the live spec is r2. Structurally truthful —
	// the same contract the MR-bridge (#420) will stamp on write.
	noAgent := false
	tmpl := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: namespace},
		Spec: v1alpha1.WorkflowTemplateSpec{
			Description: "PR review (fixture)",
			Prepare:     v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "pr-fetch"}},
			Agent: v1alpha1.AgentSpec{
				Model:        "mistral-small-latest",
				Skill:        "/skills/pr-review/",
				TaskTemplate: v1alpha1.TaskTemplate{Name: "pr-review"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review"}},
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
			Scope: []v1alpha1.ScopeParam{
				{Name: "repos", Kind: "list", Label: "Repos", Description: "the scope the prepare plugin operates on"},
				{Name: "label", Kind: "string", Label: "Label trigger", Default: "needs-review"},
			},
		},
	}
	revJSON, err := json.Marshal([]templateRevisionForFixture{
		{
			Rev:         1,
			Description: "deterministic-only, stale fetch",
			Spec: v1alpha1.WorkflowTemplateSpec{
				Description: "PR review (fixture, r1)",
				Prepare:     v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "pr-fetch-stale"}},
				Agent:       v1alpha1.AgentSpec{Enabled: &noAgent},
				Deploy:      v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
			},
		},
	})
	if err == nil { // fixture construction error = programming error; degrade to headless
		tmpl.Annotations = map[string]string{ui.RevisionsAnnotation: string(revJSON)}
	}
	workflowCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "workflows.harmostes.dev", ResourceVersion: "42"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "harmostes.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Workflow"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type:        "object",
								Description: "the Workflow spec (fixture projection)",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"templateRef": {Type: "string", Description: "the WorkflowTemplate this workflow instantiates"},
									"source": {
										Type: "object", Description: "the repo the workflow operates on",
										Properties: map[string]apiextensionsv1.JSONSchemaProps{
											"repo":   {Type: "string"},
											"branch": {Type: "string"},
										},
									},
									"graph": {
										Type: "object", Description: "the graph-native pipeline (fixture projection)",
										Properties: map[string]apiextensionsv1.JSONSchemaProps{
											"nodes": {
												Type: "array",
												Items: &apiextensionsv1.JSONSchemaPropsOrArray{
													Schema: &apiextensionsv1.JSONSchemaProps{
														Type: "object",
														Properties: map[string]apiextensionsv1.JSONSchemaProps{
															// The node-type vocabulary the topology
															// palette derives (#417) — mirrors the
															// real CRD enum.
															"type": {Type: "string", Enum: graphNodeTypeEnum()},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			}},
		},
	}
	templateCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "workflowtemplates.harmostes.dev", ResourceVersion: "42"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "harmostes.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "WorkflowTemplate"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type:        "object",
								Description: "the WorkflowTemplate spec (fixture projection)",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"description": {Type: "string", Description: "human summary of what the template does"},
									"scope": {
										Type: "array", Description: "parameters the prepare plugin reads",
										Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"name": {Type: "string"},
												"kind": {Type: "string"},
											}},
										},
									},
								},
							},
						},
					},
				},
			}},
		},
	}
	return []runtime.Object{tmpl, workflowCRD, templateCRD}
}
