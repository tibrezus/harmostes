package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/observability"
)

// ---------------------------------------------------------------------------
// KubeClient interface
// ---------------------------------------------------------------------------

// KubeClient is a narrow interface for k8s resource management used by
// deployment node executors (vela-app, flux-reconcile). It wraps
// controller-runtime's client with unstructured objects for testability —
// the executors depend on this interface, not on client.Client directly.
type KubeClient interface {
	// GetResource retrieves a resource as a raw map (the unstructured content).
	GetResource(ctx context.Context, apiVersion, kind, name, namespace string) (map[string]any, error)

	// ApplyResource creates or updates a resource. The spec is set as the
	// "spec" field of the unstructured object. Name and namespace are set on
	// the metadata.
	ApplyResource(ctx context.Context, apiVersion, kind, name, namespace string, spec map[string]any) error

	// DeleteResource removes a resource. No-op if not found.
	DeleteResource(ctx context.Context, apiVersion, kind, name, namespace string) error

	// AnnotateResource adds or updates annotations on an existing resource.
	AnnotateResource(ctx context.Context, apiVersion, kind, name, namespace string, annotations map[string]string) error
}

// controllerRuntimeKubeClient wraps controller-runtime's client.Client to
// implement KubeClient using unstructured objects.
type controllerRuntimeKubeClient struct {
	client client.Client
}

// NewKubeClient wraps a controller-runtime client into a KubeClient.
func NewKubeClient(c client.Client) KubeClient {
	return &controllerRuntimeKubeClient{client: c}
}

func (w *controllerRuntimeKubeClient) GetResource(ctx context.Context, apiVersion, kind, name, namespace string) (map[string]any, error) {
	obj := unstructuredFor(apiVersion, kind)
	if err := w.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		return nil, err
	}
	return obj.UnstructuredContent(), nil
}

func (w *controllerRuntimeKubeClient) ApplyResource(ctx context.Context, apiVersion, kind, name, namespace string, spec map[string]any) error {
	obj := unstructuredFor(apiVersion, kind)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.Object["spec"] = spec

	// Try create first; if it exists, update.
	err := w.client.Create(ctx, obj)
	if err == nil {
		return nil
	}
	if !meta.IsNoMatchError(err) && !isAlreadyExists(err) {
		// Re-check: maybe it was created by another process.
		existing := unstructuredFor(apiVersion, kind)
		if getErr := w.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existing); getErr != nil {
			return fmt.Errorf("apply %s/%s: create failed (%v) and get failed (%v)", kind, name, err, getErr)
		}
		existing.Object["spec"] = spec
		return w.client.Update(ctx, existing)
	}
	if isAlreadyExists(err) {
		existing := unstructuredFor(apiVersion, kind)
		if getErr := w.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, existing); getErr != nil {
			return fmt.Errorf("apply %s/%s: get after already-exists: %w", kind, name, getErr)
		}
		existing.Object["spec"] = spec
		return w.client.Update(ctx, existing)
	}
	return err
}

func (w *controllerRuntimeKubeClient) DeleteResource(ctx context.Context, apiVersion, kind, name, namespace string) error {
	obj := unstructuredFor(apiVersion, kind)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	return client.IgnoreNotFound(w.client.Delete(ctx, obj))
}

func (w *controllerRuntimeKubeClient) AnnotateResource(ctx context.Context, apiVersion, kind, name, namespace string, annotations map[string]string) error {
	obj := unstructuredFor(apiVersion, kind)
	if err := w.client.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, obj); err != nil {
		return err
	}
	existingAnnotations := obj.GetAnnotations()
	if existingAnnotations == nil {
		existingAnnotations = map[string]string{}
	}
	for k, v := range annotations {
		existingAnnotations[k] = v
	}
	obj.SetAnnotations(existingAnnotations)
	return w.client.Update(ctx, obj)
}

// unstructuredFor creates an Unstructured with the given apiVersion and kind.
func unstructuredFor(apiVersion, kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u
}

func isAlreadyExists(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "already exists"))
}

// ---------------------------------------------------------------------------
// VelaAppExecutor
// ---------------------------------------------------------------------------

const velaAppAPIVersion = "core.oam.dev/v1beta1"

// VelaAppExecutor runs a "vela-app" node — create/update/delete/wait a
// KubeVela Application CR.
type VelaAppExecutor struct {
	client KubeClient
}

// NewVelaAppExecutor creates a vela-app node executor.
func NewVelaAppExecutor(client KubeClient) *VelaAppExecutor {
	return &VelaAppExecutor{client: client}
}

func (e *VelaAppExecutor) Type() string        { return "vela-app" }
func (e *VelaAppExecutor) Deterministic() bool { return true }

func (e *VelaAppExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.vela-app")
	defer span.End()
	span.SetAttributes(attribute.String("harmostes.node.id", node.ID))

	if e.client == nil {
		return errNoKubeClient(span)
	}

	cfg, err := parseConfig[VelaAppConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = env.Namespace
	}
	if cfg.Name == "" {
		err := fmt.Errorf("vela-app node %q: config.name is required", node.ID)
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	span.SetAttributes(
		attribute.String("harmostes.vela.action", cfg.Action),
		attribute.String("harmostes.vela.name", cfg.Name),
		attribute.String("harmostes.vela.namespace", ns),
	)

	switch cfg.Action {
	case "apply":
		if err := e.client.ApplyResource(ctx, velaAppAPIVersion, "Application", cfg.Name, ns, cfg.Application); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("vela apply: %v", err)}, err
		}
		return NodeResult{
			Status:  StatusGreen,
			Outputs: NodeOutputs{"applied": true, "name": cfg.Name},
		}, nil

	case "delete":
		if err := e.client.DeleteResource(ctx, velaAppAPIVersion, "Application", cfg.Name, ns); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("vela delete: %v", err)}, err
		}
		return NodeResult{
			Status:  StatusGreen,
			Outputs: NodeOutputs{"applied": false, "name": cfg.Name},
		}, nil

	case "wait":
		timeout := parseTimeout(cfg.Timeout, 5*time.Minute)
		ready, revision, err := pollReady(ctx, e.client, velaAppAPIVersion, "Application", cfg.Name, ns, timeout)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("vela wait: %v", err)}, err
		}
		if !ready {
			return NodeResult{
				Status:   StatusFailed,
				Feedback: fmt.Sprintf("vela Application %s/%s not Ready after %s", ns, cfg.Name, timeout),
				Outputs:  NodeOutputs{"ready": false, "revision": revision},
			}, nil
		}
		return NodeResult{
			Status:  StatusGreen,
			Outputs: NodeOutputs{"applied": true, "ready": true, "revision": revision},
		}, nil

	default:
		err := fmt.Errorf("vela-app node %q: unknown action %q (want apply|delete|wait)", node.ID, cfg.Action)
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}
}

// ---------------------------------------------------------------------------
// FluxReconcileExecutor
// ---------------------------------------------------------------------------

// fluxKindToAPIVersion maps Flux resource kinds to their apiVersion.
var fluxKindToAPIVersion = map[string]string{
	"helmrelease":    "helm.toolkit.fluxcd.io/v2",
	"kustomization":  "kustomize.toolkit.fluxcd.io/v1",
	"gitrepository":  "source.toolkit.fluxcd.io/v1",
	"ocirepository":  "source.toolkit.fluxcd.io/v1beta2",
	"helmrepository": "source.toolkit.fluxcd.io/v1",
	"helmchart":      "source.toolkit.fluxcd.io/v1",
	"bucket":         "source.toolkit.fluxcd.io/v1beta2",
	"alert":          "notification.toolkit.fluxcd.io/v1beta3",
	"provider":       "notification.toolkit.fluxcd.io/v1beta3",
	"receiver":       "notification.toolkit.fluxcd.io/v1",
}

// fluxReconcileAtKey is the annotation Flux watches for reconciliation triggers.
const fluxReconcileAtKey = "fluxcd.io/reconcileAt"

// FluxReconcileExecutor runs a "flux-reconcile" node — annotates a Flux-managed
// resource with fluxcd.io/reconcileAt to trigger reconciliation, then optionally
// polls until Ready=True.
type FluxReconcileExecutor struct {
	client KubeClient
}

// NewFluxReconcileExecutor creates a flux-reconcile node executor.
func NewFluxReconcileExecutor(client KubeClient) *FluxReconcileExecutor {
	return &FluxReconcileExecutor{client: client}
}

func (e *FluxReconcileExecutor) Type() string        { return "flux-reconcile" }
func (e *FluxReconcileExecutor) Deterministic() bool { return true }

func (e *FluxReconcileExecutor) Execute(ctx context.Context, node v1alpha1.NodeSpec, env NodeEnv) (NodeResult, error) {
	ctx, span := observability.Tracer().Start(ctx, "graph.node.flux-reconcile")
	defer span.End()
	span.SetAttributes(attribute.String("harmostes.node.id", node.ID))

	if e.client == nil {
		return errNoKubeClient(span)
	}

	cfg, err := parseConfig[FluxReconcileConfig](node.Config)
	if err != nil {
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	// Parse resource kind/name: "helmrelease/my-service" → kind=helmrelease, name=my-service.
	parts := strings.SplitN(cfg.Resource, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		err := fmt.Errorf("flux-reconcile node %q: resource must be kind/name (got %q)", node.ID, cfg.Resource)
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}
	kind, name := parts[0], parts[1]

	apiVersion, ok := fluxKindToAPIVersion[strings.ToLower(kind)]
	if !ok {
		err := fmt.Errorf("flux-reconcile node %q: unknown Flux resource kind %q (supported: %s)", node.ID, kind, fluxKinds())
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = env.Namespace
	}

	// Capitalize kind for the k8s API (helmrelease → HelmRelease).
	kindTitled := titleFluxKind(kind)

	span.SetAttributes(
		attribute.String("harmostes.flux.resource", cfg.Resource),
		attribute.String("harmostes.flux.apiVersion", apiVersion),
		attribute.String("harmostes.flux.kind", kindTitled),
		attribute.String("harmostes.flux.namespace", ns),
		attribute.Bool("harmostes.flux.wait", cfg.Wait),
	)

	// Annotate with reconcileAt to trigger reconciliation.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.client.AnnotateResource(ctx, apiVersion, kindTitled, name, ns, map[string]string{
		fluxReconcileAtKey: now,
	}); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("flux annotate: %v", err)}, err
	}

	revision := now

	if !cfg.Wait {
		return NodeResult{
			Status:  StatusGreen,
			Outputs: NodeOutputs{"ready": true, "revision": revision},
		}, nil
	}

	// Poll for Ready=True.
	timeout := parseTimeout(cfg.Timeout, 5*time.Minute)
	ready, rev, err := pollReady(ctx, e.client, apiVersion, kindTitled, name, ns, timeout)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return NodeResult{Status: StatusFailed, Feedback: fmt.Sprintf("flux wait: %v", err)}, err
	}
	if !ready {
		return NodeResult{
			Status:   StatusFailed,
			Feedback: fmt.Sprintf("flux %s %s/%s not Ready after %s", kindTitled, ns, name, timeout),
			Outputs:  NodeOutputs{"ready": false, "revision": rev},
		}, nil
	}

	return NodeResult{
		Status:  StatusGreen,
		Outputs: NodeOutputs{"ready": true, "revision": rev},
	}, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// pollReady polls a resource's Ready condition until True or timeout.
func pollReady(ctx context.Context, kube KubeClient, apiVersion, kind, name, namespace string, timeout time.Duration) (bool, string, error) {
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		obj, err := kube.GetResource(ctx, apiVersion, kind, name, namespace)
		if err != nil {
			return false, "", fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
		}

		ready, _ := extractReadyCondition(obj)
		revision := extractRevision(obj, apiVersion, kind)

		if ready {
			return true, revision, nil
		}

		if time.Now().After(deadline) {
			return false, revision, nil
		}

		select {
		case <-ctx.Done():
			return false, revision, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// extractReadyCondition reads the Ready condition from an unstructured object's
// status.conditions. Returns (true, nil) if Ready=True; (false, nil) if not
// Ready or no conditions; (false, err) if conditions are malformed.
func extractReadyCondition(obj map[string]any) (bool, error) {
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return false, nil
	}
	conditions, ok := status["conditions"].([]any)
	if !ok {
		return false, nil
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Ready" {
			return cond["status"] == string(metav1.ConditionTrue), nil
		}
	}
	return false, nil
}

// extractRevision reads the revision from common Flux/KubeVela status fields.
func extractRevision(obj map[string]any, apiVersion, kind string) string {
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return ""
	}
	// Flux resources store revision in status.lastAttemptedRevision or .source.
	for _, key := range []string{"lastAttemptedRevision", "lastHandledRevert", "observedGeneration"} {
		if v, ok := status[key]; ok {
			return fmt.Sprintf("%v", v)
		}
	}
	// KubeVela Application stores revision in status.latestRevision.
	if rev, ok := status["latestRevision"].(map[string]any); ok {
		if name, ok := rev["name"].(string); ok {
			return name
		}
	}
	return ""
}

// parseTimeout parses a duration string (e.g. "600s", "10m"). Returns the
// default if empty or invalid.
func parseTimeout(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// titleFluxKind capitalizes each segment of a Flux resource kind:
// "helmrelease" → "HelmRelease", "gitrepository" → "GitRepository".
func titleFluxKind(kind string) string {
	// Known multi-word Flux kinds.
	titled := map[string]string{
		"helmrelease":    "HelmRelease",
		"helmrepository": "HelmRepository",
		"helmchart":      "HelmChart",
		"gitrepository":  "GitRepository",
		"ocirepository":  "OCIRepository",
		"kustomization":  "Kustomization",
		"bucket":         "Bucket",
		"alert":          "Alert",
		"provider":       "Provider",
		"receiver":       "Receiver",
	}
	if t, ok := titled[strings.ToLower(kind)]; ok {
		return t
	}
	// Fallback: capitalize first letter.
	if len(kind) > 0 {
		return strings.ToUpper(kind[:1]) + kind[1:]
	}
	return kind
}

// fluxKinds returns a comma-separated list of supported Flux resource kinds.
func fluxKinds() string {
	kinds := make([]string, 0, len(fluxKindToAPIVersion))
	for k := range fluxKindToAPIVersion {
		kinds = append(kinds, k)
	}
	return strings.Join(kinds, ", ")
}

// errNoKubeClient returns a standard error for deployment nodes executed
// without a KubeClient wired.
func errNoKubeClient(span trace.Span) (NodeResult, error) {
	err := fmt.Errorf("deployment node executed without a KubeClient — wire KubeClient in Dependencies")
	span.SetStatus(codes.Error, err.Error())
	return NodeResult{Status: StatusFailed, Feedback: err.Error()}, err
}

// gvrFromAPIVersionKind derives a GroupVersionResource from apiVersion + kind.
// Used by the production KubeClient's RESTMapper.
func gvrFromAPIVersionKind(apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	return gv.WithResource(strings.ToLower(kind) + "s"), nil
}
