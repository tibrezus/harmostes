package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// pipelineTestServer builds a Server with a fake k8s client preloaded with objects.
func pipelineTestServer(existing ...client.Object) *Server {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing...).
		Build()

	tmpl, _ := parseTemplates()

	return &Server{
		k8sClient: cl,
		namespace: "harmostes",
		logger:    slog.Default(),
		templates: tmpl,
	}
}

// reqWithAuth creates a request with the identity in the context (simulating
// what the authMiddleware does after extracting X-Authentik-Username).
func reqWithAuth(method, target string, body string) *http.Request {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, bodyReader)
	id := &Identity{Username: "alice"}
	ctx := context.WithValue(req.Context(), identityKey, id)
	return req.WithContext(ctx)
}

func preloadedPipeline(name, owner string) *v1alpha1.Pipeline {
	p := &v1alpha1.Pipeline{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "harmostes",
			Labels:    map[string]string{},
		},
		Spec: v1alpha1.PipelineSpec{
			Trigger: v1alpha1.TriggerSpec{Type: "webhook"},
			Graph: v1alpha1.GraphSpec{
				Nodes: []v1alpha1.NodeSpec{
					{ID: "prepare", Type: "plugin"},
					{ID: "agent", Type: "agent"},
					{ID: "deploy", Type: "plugin"},
				},
				Edges: []v1alpha1.EdgeSpec{
					{From: "prepare", To: "agent"},
					{From: "agent", To: "deploy"},
				},
			},
		},
	}
	if owner != "" {
		p.Labels[v1alpha1.OwnerLabel] = owner
	}
	return p
}

// ---------------------------------------------------------------------------
// GET /api/pipelines — list
// ---------------------------------------------------------------------------

func TestPipelineAPIList_Empty(t *testing.T) {
	s := pipelineTestServer()
	w := httptest.NewRecorder()
	s.handlePipelineAPIList(w, reqWithAuth("GET", "/api/pipelines", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	pipelines, ok := resp["pipelines"].([]any)
	if !ok {
		t.Fatalf("response missing pipelines array: %v", resp)
	}
	if len(pipelines) != 0 {
		t.Errorf("pipelines = %d, want 0", len(pipelines))
	}
}

func TestPipelineAPIList_OwnerFiltered(t *testing.T) {
	s := pipelineTestServer(
		preloadedPipeline("alice-pipe", "alice"),
		preloadedPipeline("bob-pipe", "bob"),
	)
	w := httptest.NewRecorder()
	s.handlePipelineAPIList(w, reqWithAuth("GET", "/api/pipelines", ""))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	pipelines := resp["pipelines"].([]any)
	if len(pipelines) != 1 {
		t.Fatalf("pipelines = %d, want 1 (owner alice only)", len(pipelines))
	}
	pipe := pipelines[0].(map[string]any)
	if pipe["name"] != "alice-pipe" {
		t.Errorf("name = %v, want alice-pipe", pipe["name"])
	}
}

func TestPipelineAPIList_SummaryFields(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("my-pipe", "alice"))
	w := httptest.NewRecorder()
	s.handlePipelineAPIList(w, reqWithAuth("GET", "/api/pipelines", ""))

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	pipelines := resp["pipelines"].([]any)
	pipe := pipelines[0].(map[string]any)
	if pipe["nodes"].(float64) != 3 {
		t.Errorf("nodes = %v, want 3", pipe["nodes"])
	}
	if pipe["trigger"] != "webhook" {
		t.Errorf("trigger = %v, want webhook", pipe["trigger"])
	}
}

// ---------------------------------------------------------------------------
// GET /api/pipelines/{name} — get
// ---------------------------------------------------------------------------

func TestPipelineAPIGet_Success(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("my-pipe", "alice"))
	r := reqWithAuth("GET", "/api/pipelines/my-pipe", "")
	r.SetPathValue("name", "my-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIGet(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var pipe v1alpha1.Pipeline
	json.Unmarshal(w.Body.Bytes(), &pipe)
	if pipe.Name != "my-pipe" {
		t.Errorf("name = %q, want my-pipe", pipe.Name)
	}
	if len(pipe.Spec.Graph.Nodes) != 3 {
		t.Errorf("nodes = %d, want 3", len(pipe.Spec.Graph.Nodes))
	}
}

func TestPipelineAPIGet_NotFound(t *testing.T) {
	s := pipelineTestServer()
	r := reqWithAuth("GET", "/api/pipelines/nonexistent", "")
	r.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	s.handlePipelineAPIGet(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPipelineAPIGet_OwnerMismatch(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("bob-pipe", "bob"))
	r := reqWithAuth("GET", "/api/pipelines/bob-pipe", "")
	r.SetPathValue("name", "bob-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIGet(w, r)

	// Owner mismatch returns 404 (hide existence), not 403.
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (hide existence)", w.Code)
	}
}

// ---------------------------------------------------------------------------
// PUT /api/pipelines/{name} — create + update
// ---------------------------------------------------------------------------

func TestPipelineAPIPut_Create(t *testing.T) {
	s := pipelineTestServer()
	body := `{"spec":{"trigger":{"type":"webhook"},"graph":{"nodes":[{"id":"a","type":"plugin"}],"edges":[]}}}`
	r := reqWithAuth("PUT", "/api/pipelines/new-pipe", body)
	r.SetPathValue("name", "new-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIPut(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	// Verify the pipeline was created with owner label.
	var pipe v1alpha1.Pipeline
	json.Unmarshal(w.Body.Bytes(), &pipe)
	if pipe.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("owner label = %q, want alice", pipe.Labels[v1alpha1.OwnerLabel])
	}
	if len(pipe.Spec.Graph.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(pipe.Spec.Graph.Nodes))
	}
}

func TestPipelineAPIPut_Update(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("existing-pipe", "alice"))
	body := `{"spec":{"trigger":{"type":"schedule"},"graph":{"nodes":[{"id":"x","type":"plugin"},{"id":"y","type":"agent"}],"edges":[{"from":"x","to":"y"}]}}}`
	r := reqWithAuth("PUT", "/api/pipelines/existing-pipe", body)
	r.SetPathValue("name", "existing-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIPut(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var pipe v1alpha1.Pipeline
	json.Unmarshal(w.Body.Bytes(), &pipe)
	if pipe.Spec.Trigger.Type != "schedule" {
		t.Errorf("trigger = %q, want schedule", pipe.Spec.Trigger.Type)
	}
	if len(pipe.Spec.Graph.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2", len(pipe.Spec.Graph.Nodes))
	}
}

func TestPipelineAPIPut_UpdateOwnerMismatch(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("bob-pipe", "bob"))
	body := `{"spec":{"trigger":{"type":"webhook"},"graph":{"nodes":[],"edges":[]}}}`
	r := reqWithAuth("PUT", "/api/pipelines/bob-pipe", body)
	r.SetPathValue("name", "bob-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIPut(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestPipelineAPIPut_InvalidJSON(t *testing.T) {
	s := pipelineTestServer()
	r := reqWithAuth("PUT", "/api/pipelines/bad", "not json")
	r.SetPathValue("name", "bad")
	w := httptest.NewRecorder()
	s.handlePipelineAPIPut(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestPipelineAPIPut_DefaultTrigger(t *testing.T) {
	s := pipelineTestServer()
	body := `{"spec":{"graph":{"nodes":[],"edges":[]}}}`
	r := reqWithAuth("PUT", "/api/pipelines/no-trigger", body)
	r.SetPathValue("name", "no-trigger")
	w := httptest.NewRecorder()
	s.handlePipelineAPIPut(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	var pipe v1alpha1.Pipeline
	json.Unmarshal(w.Body.Bytes(), &pipe)
	if pipe.Spec.Trigger.Type != "manual" {
		t.Errorf("trigger = %q, want manual (default)", pipe.Spec.Trigger.Type)
	}
}

// ---------------------------------------------------------------------------
// DELETE /api/pipelines/{name}
// ---------------------------------------------------------------------------

func TestPipelineAPIDelete_Success(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("del-pipe", "alice"))
	r := reqWithAuth("DELETE", "/api/pipelines/del-pipe", "")
	r.SetPathValue("name", "del-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIDelete(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// Verify it's actually deleted.
	var check v1alpha1.Pipeline
	err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "del-pipe"}, &check)
	if err == nil {
		t.Error("pipeline still exists after delete")
	}
}

func TestPipelineAPIDelete_NotFound(t *testing.T) {
	s := pipelineTestServer()
	r := reqWithAuth("DELETE", "/api/pipelines/nonexistent", "")
	r.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	s.handlePipelineAPIDelete(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPipelineAPIDelete_OwnerMismatch(t *testing.T) {
	s := pipelineTestServer(preloadedPipeline("bob-pipe", "bob"))
	r := reqWithAuth("DELETE", "/api/pipelines/bob-pipe", "")
	r.SetPathValue("name", "bob-pipe")
	w := httptest.NewRecorder()
	s.handlePipelineAPIDelete(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Name validation helpers
// ---------------------------------------------------------------------------

func TestIsValidPipelineName(t *testing.T) {
	valid := []string{"my-pipe", "wiki-update", "a", "test123", "my.pipe"}
	for _, name := range valid {
		if !isValidPipelineName(name) {
			t.Errorf("isValidPipelineName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", "UPPER", "has spaces", "-leading", "trailing-", "_under"}
	for _, name := range invalid {
		if isValidPipelineName(name) {
			t.Errorf("isValidPipelineName(%q) = true, want false", name)
		}
	}
}

func TestSanitizePipelineName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"My Pipeline", "my-pipeline"},
		{"WIKI_Update", "wiki-update"},
		{"  spaces  ", "spaces"},
		{"--dashes--", "dashes"},
		{"CamelCase", "camelcase"},
	}
	for _, tt := range tests {
		if got := sanitizePipelineName(tt.input); got != tt.want {
			t.Errorf("sanitizePipelineName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
