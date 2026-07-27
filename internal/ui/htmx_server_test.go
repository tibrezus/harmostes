package ui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestNewHTMXServer(t *testing.T) {
	k8s := &mockK8sClient{}
	dapr := &mockUIDaprClient{}
	logger := testLogger()

	s := NewHTMXServer(k8s, dapr, logger)

	if s == nil {
		t.Error("NewHTMXServer returned nil")
	}
	if s.k8sClient == nil {
		t.Error("k8sClient not set")
	}
	if s.daprClient == nil {
		t.Error("daprClient not set")
	}
	if s.logger == nil {
		t.Error("logger not set")
	}
}

func TestHTMXServer_handleHealthz(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestHTMXServer_Routes_healthz(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()

	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHTMXServer_Routes_index(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	s.Routes().ServeHTTP(w, req)

	// No auth headers = 401
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
	}
}

func TestHTMXServer_Routes_index_authenticated(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	w := httptest.NewRecorder()

	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Home page") {
		t.Errorf("expected body to contain 'Home page', got %q", body)
	}
}

func TestHTMXServer_Routes_workflows(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/workflows", nil)
	req.Header.Set("X-Authentik-Username", "bob")
	w := httptest.NewRecorder()

	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestHTMXServer_Routes_workflowDetail(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	req := httptest.NewRequest("GET", "/workflows/test-workflow", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	w := httptest.NewRecorder()

	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "test-workflow") {
		t.Errorf("expected body to contain 'test-workflow', got %q", body)
	}
}

func TestExtractIdentityFromHeaders(t *testing.T) {
	tests := []struct {
		name         string
		headers      map[string]string
		wantUsername string
		wantGroups   []string
		wantNil      bool
	}{
		{
			name: "X-Authentik headers",
			headers: map[string]string{
				"X-Authentik-Username": "alice",
				"X-Authentik-Email":    "alice@example.com",
				"X-Authentik-Groups":   "developers|admins",
			},
			wantUsername: "alice",
			wantGroups:   []string{"developers", "admins"},
		},
		{
			name: "X-Forwarded headers (legacy)",
			headers: map[string]string{
				"X-Forwarded-Preferred-Username": "bob",
				"X-Forwarded-Email":              "bob@example.com",
				"X-Forwarded-Groups":             "users",
			},
			wantUsername: "bob",
			wantGroups:   []string{"users"},
		},
		{
			name: "Dev mode override",
			headers: map[string]string{
				"X-Harmostes-Dev-User": "dev-user",
			},
			wantUsername: "dev-user",
		},
		{
			name:    "No auth headers",
			headers: map[string]string{},
			wantNil: true,
		},
		{
			name: "Empty groups header",
			headers: map[string]string{
				"X-Authentik-Username": "alice",
				"X-Authentik-Groups":   "",
			},
			wantUsername: "alice",
			wantGroups:   nil,
		},
		{
			name: "Single group",
			headers: map[string]string{
				"X-Authentik-Username": "alice",
				"X-Authentik-Groups":   "developers",
			},
			wantUsername: "alice",
			wantGroups:   []string{"developers"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			identity := extractIdentity(req)

			if tt.wantNil {
				if identity != nil {
					t.Error("expected nil identity, got", identity)
				}
				return
			}

			if identity == nil {
				t.Error("expected identity, got nil")
				return
			}
			if identity.Username != tt.wantUsername {
				t.Errorf("expected username %q, got %q", tt.wantUsername, identity.Username)
			}
			if tt.wantGroups != nil {
				if len(identity.Groups) != len(tt.wantGroups) {
					t.Errorf("expected %d groups, got %d", len(tt.wantGroups), len(identity.Groups))
				}
				for i, g := range tt.wantGroups {
					if identity.Groups[i] != g {
						t.Errorf("expected group[%d] %q, got %q", i, g, identity.Groups[i])
					}
				}
			}
		})
	}
}

func TestIdentityFromContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		want    string
		wantNil bool
	}{
		{
			name: "identity in context",
			ctx:  context.WithValue(context.Background(), identityKey, &Identity{Username: "alice"}),
			want: "alice",
		},
		{
			name:    "no identity in context",
			ctx:     context.Background(),
			wantNil: true,
		},
		{
			name:    "wrong type in context",
			ctx:     context.WithValue(context.Background(), identityKey, "not-an-identity"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identityFromContext(tt.ctx)
			if tt.wantNil {
				if got != nil {
					t.Error("expected nil, got", got)
				}
				return
			}
			if got == nil {
				t.Error("expected identity, got nil")
				return
			}
			if got.Username != tt.want {
				t.Errorf("expected username %q, got %q", tt.want, got.Username)
			}
		})
	}
}

func TestWithAuth(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		identity := identityFromContext(r.Context())
		if identity == nil {
			http.Error(w, "no identity", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("user: " + identity.Username))
	}

	wrapped := withAuth(handler, testLogger())

	t.Run("authenticated request", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		w := httptest.NewRecorder()

		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "user: alice") {
			t.Errorf("expected body to contain 'user: alice', got %q", body)
		}
	})

	t.Run("unauthenticated request", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()

		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status %d, got %d", http.StatusUnauthorized, w.Code)
		}
	})
}

// Mock K8sClient for testing
type mockK8sClient struct{}

func (m *mockK8sClient) GetWorkflow(ctx context.Context, name string) (*v1alpha1.Workflow, error) {
	return nil, nil
}
func (m *mockK8sClient) ListWorkflows(ctx context.Context, owner string) ([]v1alpha1.Workflow, error) {
	return nil, nil
}
func (m *mockK8sClient) CreateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error {
	return nil
}
func (m *mockK8sClient) UpdateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error {
	return nil
}
func (m *mockK8sClient) DeleteWorkflow(ctx context.Context, name string) error {
	return nil
}
func (m *mockK8sClient) GetPipeline(ctx context.Context, name string) (*v1alpha1.Pipeline, error) {
	return nil, nil
}
func (m *mockK8sClient) ListPipelines(ctx context.Context, owner string) ([]v1alpha1.Pipeline, error) {
	return nil, nil
}
func (m *mockK8sClient) CreatePipeline(ctx context.Context, pl *v1alpha1.Pipeline) error {
	return nil
}
func (m *mockK8sClient) UpdatePipeline(ctx context.Context, pl *v1alpha1.Pipeline) error {
	return nil
}
func (m *mockK8sClient) DeletePipeline(ctx context.Context, name string) error {
	return nil
}
func (m *mockK8sClient) ListJobs(ctx context.Context, workflowName string) ([]batchv1.Job, error) {
	return nil, nil
}
func (m *mockK8sClient) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	return nil
}
func (m *mockK8sClient) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	return nil, nil
}
func (m *mockK8sClient) DeleteSecret(ctx context.Context, name string) error {
	return nil
}

// mockUIK8sClient implements K8sClient for HTMX server testing

// mockUIDaprClient implements DaprClient for HTMX server testing
type mockUIDaprClient struct{}

func (m *mockUIDaprClient) SaveState(ctx context.Context, key string, value any) error {
	return nil
}
func (m *mockUIDaprClient) GetState(ctx context.Context, key string, value any) (bool, error) {
	return false, nil
}
func (m *mockUIDaprClient) DeleteState(ctx context.Context, key string) error {
	return nil
}
func (m *mockUIDaprClient) GetSecret(ctx context.Context, secretName, key string) (string, error) {
	return "", nil
}
func (m *mockUIDaprClient) PublishEvent(ctx context.Context, topic string, data any) error {
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}
