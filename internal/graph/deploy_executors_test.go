package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Fake KubeClient
// ---------------------------------------------------------------------------

// fakeKubeClient is an in-memory KubeClient for testing deployment nodes.
type fakeKubeClient struct {
	resources   map[string]map[string]any // key: apiVersion/kind/namespace/name
	annotations map[string]map[string]string
	getErr      error
	applyErr    error
	deleteErr   error
	annotateErr error
}

type kubeResourceKey struct {
	apiVersion, kind, namespace, name string
}

func newFakeKubeClient() *fakeKubeClient {
	return &fakeKubeClient{
		resources:   make(map[string]map[string]any),
		annotations: make(map[string]map[string]string),
	}
}

func (f *fakeKubeClient) keyOf(apiVersion, kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name)
}

func (f *fakeKubeClient) GetResource(ctx context.Context, apiVersion, kind, name, namespace string) (map[string]any, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	key := f.keyOf(apiVersion, kind, namespace, name)
	obj, ok := f.resources[key]
	if !ok {
		return nil, fmt.Errorf("%s %s/%s not found", kind, namespace, name)
	}
	// Return a copy with annotations merged.
	cp := copyMap(obj)
	if anns, ok := f.annotations[key]; ok {
		if existing, ok := cp["metadata"].(map[string]any); ok {
			existing["annotations"] = anns
		}
	}
	return cp, nil
}

func (f *fakeKubeClient) ApplyResource(ctx context.Context, apiVersion, kind, name, namespace string, spec map[string]any) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	key := f.keyOf(apiVersion, kind, namespace, name)
	f.resources[key] = map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": spec,
	}
	return nil
}

func (f *fakeKubeClient) DeleteResource(ctx context.Context, apiVersion, kind, name, namespace string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	key := f.keyOf(apiVersion, kind, namespace, name)
	delete(f.resources, key)
	return nil
}

func (f *fakeKubeClient) AnnotateResource(ctx context.Context, apiVersion, kind, name, namespace string, annotations map[string]string) error {
	if f.annotateErr != nil {
		return f.annotateErr
	}
	key := f.keyOf(apiVersion, kind, namespace, name)
	if _, ok := f.resources[key]; !ok {
		return fmt.Errorf("%s %s/%s not found", kind, namespace, name)
	}
	if f.annotations[key] == nil {
		f.annotations[key] = make(map[string]string)
	}
	for k, v := range annotations {
		f.annotations[key][k] = v
	}
	return nil
}

// setReady sets the Ready condition on a resource in the fake client.
func (f *fakeKubeClient) setReady(apiVersion, kind, namespace, name string, ready bool) {
	key := f.keyOf(apiVersion, kind, namespace, name)
	obj, ok := f.resources[key]
	if !ok {
		return
	}
	statusVal, _ := obj["status"].(map[string]any)
	if statusVal == nil {
		statusVal = map[string]any{}
	}
	condStatus := "False"
	if ready {
		condStatus = "True"
	}
	statusVal["conditions"] = []any{
		map[string]any{"type": "Ready", "status": condStatus},
	}
	obj["status"] = statusVal
}

func copyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// ---------------------------------------------------------------------------
// VelaAppExecutor tests
// ---------------------------------------------------------------------------

func TestVelaAppApply(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewVelaAppExecutor(kube)

	cfg := VelaAppConfig{
		Action:    "apply",
		Name:      "my-app",
		Namespace: "production",
		Application: map[string]any{
			"components": []any{
				map[string]any{
					"type": "webservice",
					"name": "api",
					"properties": map[string]any{
						"image": "nginx:latest",
					},
				},
			},
		},
	}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID:     "deploy",
		Type:   "vela-app",
		Config: cfgJSON,
	}, NodeEnv{Namespace: "default"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["applied"] != true {
		t.Errorf("applied = %v, want true", result.Outputs["applied"])
	}

	// Verify the resource was created.
	obj, err := kube.GetResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "production")
	if err != nil {
		t.Fatalf("GetResource: %v", err)
	}
	spec := obj["spec"].(map[string]any)
	comps := spec["components"].([]any)
	if len(comps) != 1 {
		t.Errorf("components = %d, want 1", len(comps))
	}
}

func TestVelaAppDelete(t *testing.T) {
	kube := newFakeKubeClient()
	// Pre-create a resource.
	kube.ApplyResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "production", map[string]any{
		"components": []any{},
	})

	exec := NewVelaAppExecutor(kube)
	cfg := VelaAppConfig{Action: "delete", Name: "my-app", Namespace: "production"}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "deploy", Type: "vela-app", Config: cfgJSON,
	}, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["applied"] != false {
		t.Errorf("applied = %v, want false", result.Outputs["applied"])
	}

	// Verify deleted.
	_, err = kube.GetResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "production")
	if err == nil {
		t.Error("resource should be deleted")
	}
}

func TestVelaAppWaitReady(t *testing.T) {
	kube := newFakeKubeClient()
	kube.ApplyResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "production", map[string]any{})
	// Set Ready=True.
	kube.setReady(velaAppAPIVersion, "Application", "production", "my-app", true)

	exec := NewVelaAppExecutor(kube)
	cfg := VelaAppConfig{Action: "wait", Name: "my-app", Namespace: "production", Timeout: "5s"}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "wait", Type: "vela-app", Config: cfgJSON,
	}, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["ready"] != true {
		t.Errorf("ready = %v, want true", result.Outputs["ready"])
	}
}

func TestVelaAppWaitNotReady(t *testing.T) {
	kube := newFakeKubeClient()
	kube.ApplyResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "production", map[string]any{})
	// Leave Ready=False (default).

	exec := NewVelaAppExecutor(kube)
	cfg := VelaAppConfig{Action: "wait", Name: "my-app", Namespace: "production", Timeout: "2s"}
	cfgJSON, _ := json.Marshal(cfg)

	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "wait", Type: "vela-app", Config: cfgJSON,
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (not ready after timeout)", result.Status)
	}
	if result.Outputs["ready"] != false {
		t.Errorf("ready = %v, want false", result.Outputs["ready"])
	}
}

func TestVelaAppNilClient(t *testing.T) {
	exec := NewVelaAppExecutor(nil)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "deploy", Type: "vela-app",
		Config: json.RawMessage(`{"action":"apply","name":"test"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (no client)", result.Status)
	}
}

func TestVelaAppMissingName(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewVelaAppExecutor(kube)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "deploy", Type: "vela-app",
		Config: json.RawMessage(`{"action":"apply"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (missing name)", result.Status)
	}
}

func TestVelaAppUnknownAction(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewVelaAppExecutor(kube)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "deploy", Type: "vela-app",
		Config: json.RawMessage(`{"action":"explode","name":"test"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (unknown action)", result.Status)
	}
}

func TestVelaAppNamespaceFromEnv(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewVelaAppExecutor(kube)
	cfg := VelaAppConfig{Action: "apply", Name: "my-app", Application: map[string]any{}}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "deploy", Type: "vela-app", Config: cfgJSON,
	}, NodeEnv{Namespace: "harmostes"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q", result.Status)
	}

	// Verify resource was created in env namespace.
	obj, err := kube.GetResource(context.Background(), velaAppAPIVersion, "Application", "my-app", "harmostes")
	if err != nil {
		t.Errorf("resource not in env namespace: %v", err)
	}
	_ = obj
}

// ---------------------------------------------------------------------------
// FluxReconcileExecutor tests
// ---------------------------------------------------------------------------

func TestFluxReconcileAnnotate(t *testing.T) {
	kube := newFakeKubeClient()
	// Pre-create a HelmRelease.
	kube.ApplyResource(context.Background(), "helm.toolkit.fluxcd.io/v2", "HelmRelease", "my-release", "production", map[string]any{})

	exec := NewFluxReconcileExecutor(kube)
	cfg := FluxReconcileConfig{
		Resource:  "helmrelease/my-release",
		Namespace: "production",
		Wait:      false,
	}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile", Config: cfgJSON,
	}, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["ready"] != true {
		t.Errorf("ready = %v, want true (no wait)", result.Outputs["ready"])
	}

	// Verify annotation was set.
	key := kube.keyOf("helm.toolkit.fluxcd.io/v2", "HelmRelease", "production", "my-release")
	anns := kube.annotations[key]
	if anns == nil {
		t.Fatal("no annotations set")
	}
	if _, ok := anns[fluxReconcileAtKey]; !ok {
		t.Errorf("annotation %q not set", fluxReconcileAtKey)
	}
}

func TestFluxReconcileWaitReady(t *testing.T) {
	kube := newFakeKubeClient()
	kube.ApplyResource(context.Background(), "helm.toolkit.fluxcd.io/v2", "HelmRelease", "my-release", "production", map[string]any{})
	kube.setReady("helm.toolkit.fluxcd.io/v2", "HelmRelease", "production", "my-release", true)

	exec := NewFluxReconcileExecutor(kube)
	cfg := FluxReconcileConfig{
		Resource:  "helmrelease/my-release",
		Namespace: "production",
		Wait:      true,
		Timeout:   "5s",
	}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile", Config: cfgJSON,
	}, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
	if result.Outputs["ready"] != true {
		t.Errorf("ready = %v, want true", result.Outputs["ready"])
	}
}

func TestFluxReconcileWaitNotReady(t *testing.T) {
	kube := newFakeKubeClient()
	kube.ApplyResource(context.Background(), "kustomize.toolkit.fluxcd.io/v1", "Kustomization", "my-kust", "default", map[string]any{})
	// Leave Ready=False.

	exec := NewFluxReconcileExecutor(kube)
	cfg := FluxReconcileConfig{
		Resource: "kustomization/my-kust",
		Wait:     true,
		Timeout:  "2s",
	}
	cfgJSON, _ := json.Marshal(cfg)

	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile", Config: cfgJSON,
	}, NodeEnv{Namespace: "default"})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (not ready after timeout)", result.Status)
	}
}

func TestFluxReconcileGitRepository(t *testing.T) {
	kube := newFakeKubeClient()
	kube.ApplyResource(context.Background(), "source.toolkit.fluxcd.io/v1", "GitRepository", "my-source", "flux-system", map[string]any{})
	kube.setReady("source.toolkit.fluxcd.io/v1", "GitRepository", "flux-system", "my-source", true)

	exec := NewFluxReconcileExecutor(kube)
	cfg := FluxReconcileConfig{
		Resource:  "gitrepository/my-source",
		Namespace: "flux-system",
		Wait:      true,
		Timeout:   "5s",
	}
	cfgJSON, _ := json.Marshal(cfg)

	result, err := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile", Config: cfgJSON,
	}, NodeEnv{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Errorf("status = %q, want green", result.Status)
	}
}

func TestFluxReconcileNilClient(t *testing.T) {
	exec := NewFluxReconcileExecutor(nil)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile",
		Config: json.RawMessage(`{"resource":"helmrelease/test"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (no client)", result.Status)
	}
}

func TestFluxReconcileBadResource(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewFluxReconcileExecutor(kube)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile",
		Config: json.RawMessage(`{"resource":"no-slash"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (bad resource)", result.Status)
	}
}

func TestFluxReconcileUnknownKind(t *testing.T) {
	kube := newFakeKubeClient()
	exec := NewFluxReconcileExecutor(kube)
	result, _ := exec.Execute(context.Background(), v1alpha1.NodeSpec{
		ID: "flux", Type: "flux-reconcile",
		Config: json.RawMessage(`{"resource":"unknownkind/test"}`),
	}, NodeEnv{})
	if result.Status != StatusFailed {
		t.Errorf("status = %q, want failed (unknown kind)", result.Status)
	}
}

// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestTitleFluxKind(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"helmrelease", "HelmRelease"},
		{"gitrepository", "GitRepository"},
		{"kustomization", "Kustomization"},
		{"ocirepository", "OCIRepository"},
		{"bucket", "Bucket"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := titleFluxKind(tt.input); got != tt.want {
				t.Errorf("titleFluxKind(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseTimeout(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"600s", 600 * time.Second},
		{"10m", 10 * time.Minute},
		{"", 5 * time.Minute},
		{"invalid", 5 * time.Minute},
	}
	def := 5 * time.Minute
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := parseTimeout(tt.input, def); got != tt.want {
				t.Errorf("parseTimeout(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractReadyCondition(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{"ready_true", map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "True"},
				},
			},
		}, true},
		{"ready_false", map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "False"},
				},
			},
		}, false},
		{"no_status", map[string]any{}, false},
		{"no_conditions", map[string]any{"status": map[string]any{}}, false},
		{"other_condition", map[string]any{
			"status": map[string]any{
				"conditions": []any{
					map[string]any{"type": "Reconciling", "status": "True"},
				},
			},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := extractReadyCondition(tt.obj)
			if got != tt.want {
				t.Errorf("extractReadyCondition() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Registry integration
// ---------------------------------------------------------------------------

func TestRegistryHasDeploymentNodes(t *testing.T) {
	r := NewDefaultRegistry(Dependencies{KubeClient: newFakeKubeClient()})
	if !r.Has("vela-app") {
		t.Error("registry should have vela-app")
	}
	if !r.Has("flux-reconcile") {
		t.Error("registry should have flux-reconcile")
	}
	types := r.Types()
	if len(types) != 9 {
		t.Errorf("registry has %d types, want 9: %v", len(types), types)
	}
}

// ---------------------------------------------------------------------------
// Integration: deploy pipeline (vela-app → flux-reconcile)
// ---------------------------------------------------------------------------

func TestIntegrationDeployPipeline(t *testing.T) {
	kube := newFakeKubeClient()
	registry := NewRegistry()

	velaExec := NewVelaAppExecutor(kube)
	fluxExec := NewFluxReconcileExecutor(kube)
	registry.Register(velaExec)
	registry.Register(fluxExec)

	// Pre-create the HelmRelease (simulating Flux being installed).
	kube.ApplyResource(context.Background(), "helm.toolkit.fluxcd.io/v2", "HelmRelease", "my-service", "production", map[string]any{})
	kube.setReady("helm.toolkit.fluxcd.io/v2", "HelmRelease", "production", "my-service", true)

	velaCfg, _ := json.Marshal(VelaAppConfig{
		Action:    "apply",
		Name:      "my-app",
		Namespace: "production",
		Application: map[string]any{
			"components": []any{
				map[string]any{"type": "webservice", "name": "api", "properties": map[string]any{"image": "nginx:latest"}},
			},
		},
	})

	fluxCfg, _ := json.Marshal(FluxReconcileConfig{
		Resource:  "helmrelease/my-service",
		Namespace: "production",
		Wait:      true,
		Timeout:   "5s",
	})

	graph := v1alpha1.GraphSpec{
		Nodes: []v1alpha1.NodeSpec{
			{ID: "deploy", Type: "vela-app", Config: velaCfg},
			{ID: "flux", Type: "flux-reconcile", Config: fluxCfg},
		},
		Edges: []v1alpha1.EdgeSpec{
			{From: "deploy", To: "flux"},
		},
	}

	exec := NewGraphExecutor(registry, nil)
	result, err := exec.Execute(context.Background(), graph, "deploy-test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != StatusGreen {
		t.Fatalf("status = %q, want green; message: %s", result.Status, result.Message)
	}

	// Verify vela-app applied.
	deployResult := result.NodeResults["deploy"]
	if deployResult.Outputs["applied"] != true {
		t.Errorf("deploy applied = %v, want true", deployResult.Outputs["applied"])
	}

	// Verify flux-reconcile ready.
	fluxResult := result.NodeResults["flux"]
	if fluxResult.Outputs["ready"] != true {
		t.Errorf("flux ready = %v, want true", fluxResult.Outputs["ready"])
	}

	// Verify annotation was set.
	key := kube.keyOf("helm.toolkit.fluxcd.io/v2", "HelmRelease", "production", "my-service")
	anns := kube.annotations[key]
	if anns == nil {
		t.Fatal("no flux annotations set")
	}
	if _, ok := anns[fluxReconcileAtKey]; !ok {
		t.Errorf("flux reconcileAt annotation not set")
	}
}

// ---------------------------------------------------------------------------
// Flux kind catalog completeness
// ---------------------------------------------------------------------------

func TestFluxKindsComplete(t *testing.T) {
	kinds := fluxKinds()
	expected := []string{"helmrelease", "kustomization", "gitrepository"}
	for _, e := range expected {
		if !strings.Contains(kinds, e) {
			t.Errorf("fluxKinds() missing %q: %s", e, kinds)
		}
	}
}
