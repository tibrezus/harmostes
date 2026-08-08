package ui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/ui/templ/pages"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// Index redirects to /workflows
	if w.Code != http.StatusSeeOther {
		t.Errorf("expected status %d, got %d", http.StatusSeeOther, w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "/workflows" {
		t.Errorf("expected redirect to /workflows, got %q", loc)
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

	// Handler returns 404 for workflows not found (mock returns error)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
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
	return nil, fmt.Errorf("not found")
}
func (m *mockK8sClient) ListWorkflows(ctx context.Context, owner string) ([]v1alpha1.Workflow, error) {
	return nil, nil
}
func (m *mockK8sClient) GetAttempt(ctx context.Context, name string) (*v1alpha1.Attempt, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockK8sClient) ListAttempts(ctx context.Context, owner string) ([]v1alpha1.Attempt, error) {
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
func (m *mockUIDaprClient) GetStateFromStore(ctx context.Context, store, key string, value any) (bool, error) {
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

func TestHTMXServer_groupWorkflowsByGate(t *testing.T) {
	s := NewHTMXServer(&mockK8sClient{}, &mockUIDaprClient{}, testLogger())

	workflows := []v1alpha1.Workflow{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wiki-harmostes"},
			Spec: v1alpha1.WorkflowSpec{
				Agent: v1alpha1.AgentSpec{
					Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
				},
				Disabled: false,
			},
			Status: v1alpha1.WorkflowStatus{
				LastRunAt: metav1.Time{Time: time.Now().Add(-2 * time.Hour)},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "wiki-llm-wiki"},
			Spec: v1alpha1.WorkflowSpec{
				Agent: v1alpha1.AgentSpec{
					Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "wiki-lint"}},
				},
				Disabled: true,
			},
			Status: v1alpha1.WorkflowStatus{
				LastRunAt: metav1.Time{},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pr-review-harmostes"},
			Spec: v1alpha1.WorkflowSpec{
				Agent: v1alpha1.AgentSpec{
					Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "review-validate"}},
				},
				Disabled: false,
			},
			Status: v1alpha1.WorkflowStatus{
				LastRunAt: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			},
		},
	}

	groups := s.groupWorkflowsByGate(workflows)

	if len(groups) != 2 {
		t.Errorf("expected 2 gate groups, got %d", len(groups))
	}

	// Find wiki-lint group
	var wikiGroup *pages.GateGroup
	for i := range groups {
		if groups[i].Gate == "wiki-lint" {
			wikiGroup = &groups[i]
			break
		}
	}
	if wikiGroup == nil {
		t.Fatal("wiki-lint group not found")
	}
	if wikiGroup.Count != 2 {
		t.Errorf("expected 2 workflows in wiki-lint group, got %d", wikiGroup.Count)
	}
}

func TestFormatLastRun(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		t        metav1.Time
		expected string
	}{
		{"never", metav1.Time{}, "Never"},
		{"just now", metav1.Time{Time: now.Add(-30 * time.Second)}, "Just now"},
		{"minutes", metav1.Time{Time: now.Add(-45 * time.Minute)}, "45m ago"},
		{"hours", metav1.Time{Time: now.Add(-3 * time.Hour)}, "3h ago"},
		{"days", metav1.Time{Time: now.Add(-5 * 24 * time.Hour)}, "5d ago"},
		{"old", metav1.Time{Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}, "2024-01-01"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLastRun(tt.t)
			if got != tt.expected {
				t.Errorf("formatLastRun() = %q, want %q", got, tt.expected)
			}
		})
	}
}
