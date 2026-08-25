package ui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func boolPtr(b bool) *bool { return &b }

// newAttemptTestServer creates a Server with a fake k8s client pre-loaded
// with the given objects.
func newAttemptTestServer(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	return &Server{
		namespace: "test-ns",
		logger:    nil,
		templates: tmpl,
		hub:       NewEventHub(),
		k8sClient: fakeClient,
		platforms: newPlatformRegistry(nil),
	}
}

func TestAttemptDetail_HidesSessionLinkForDeterministicWorkflow(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fork-maintenance-forgejo", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(false)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-fork-maintenance-forgejo-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "fork-maintenance-forgejo",
			Owner:       "alice",
		},
		Status: v1alpha1.AttemptStatus{
			Phase: "reconciling",
			Runs: []v1alpha1.RunRecord{
				{Name: "worker-pool-pod-abc"},
			},
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-fork-maintenance-forgejo-abc123", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-fork-maintenance-forgejo-abc123")

	rec := httptest.NewRecorder()
	srv.handleAttemptDetail(rec, req)

	body := rec.Body.String()
	sessionURL := "/attempts/attempt-fork-maintenance-forgejo-abc123/runs/worker-pool-pod-abc/session"
	if strings.Contains(body, sessionURL) {
		t.Error("Session link should NOT be shown for deterministic workflows (agent disabled)")
	}
}

func TestAttemptDetail_ShowsSessionLinkForAgentWorkflow(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wiki-lint-harmostes", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(true)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-wiki-lint-harmostes-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "wiki-lint-harmostes",
			Owner:       "alice",
		},
		Status: v1alpha1.AttemptStatus{
			Phase: "succeeded",
			Runs: []v1alpha1.RunRecord{
				{Name: "worker-pool-pod-xyz"},
			},
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-wiki-lint-harmostes-abc123", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-wiki-lint-harmostes-abc123")

	rec := httptest.NewRecorder()
	srv.handleAttemptDetail(rec, req)

	body := rec.Body.String()
	sessionURL := "/attempts/attempt-wiki-lint-harmostes-abc123/runs/worker-pool-pod-xyz/session"
	if !strings.Contains(body, sessionURL) {
		t.Error("Session link should be shown for agent-enabled workflows")
	}
}

func TestAttemptSession_DeterministicEmptyState(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fork-maintenance-forgejo", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(false)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-fork-maintenance-forgejo-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "fork-maintenance-forgejo",
			Owner:       "alice",
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-fork-maintenance-forgejo-abc123/runs/worker-pod-abc/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-fork-maintenance-forgejo-abc123")
	req.SetPathValue("job", "worker-pod-abc")

	rec := httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "deterministic workflow") {
		t.Errorf("expected deterministic workflow message, got: %s", body)
	}
	// Should NOT blame Dapr config for deterministic workflows
	if strings.Contains(body, "Dapr state store is not configured") {
		t.Error("should not mention Dapr config for deterministic workflows")
	}
}

func TestAttemptSession_AgentWorkflowNotFoundState(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wiki-lint-harmostes", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			Agent: v1alpha1.AgentSpec{Enabled: boolPtr(true)},
		},
	}
	att := &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: "attempt-wiki-lint-harmostes-abc123", Namespace: "test-ns",
			Labels: map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.AttemptSpec{
			WorkflowRef: "wiki-lint-harmostes",
			Owner:       "alice",
		},
	}

	srv := newAttemptTestServer(t, wf, att)
	req := httptest.NewRequest("GET", "/attempts/attempt-wiki-lint-harmostes-abc123/runs/worker-pod-xyz/session", nil)
	req = req.WithContext(withTestIdentity(context.Background()))
	req.SetPathValue("name", "attempt-wiki-lint-harmostes-abc123")
	req.SetPathValue("job", "worker-pod-xyz")

	rec := httptest.NewRecorder()
	srv.handleAttemptSession(rec, req)

	body := rec.Body.String()
	// Agent workflow with no session: should show "not available" with Dapr hint
	if !strings.Contains(body, "not available") {
		t.Errorf("expected 'not available' message for agent workflow with no session, got: %s", body)
	}
}

func TestWorkflowCRNameStripsPlatform(t *testing.T) {
	cases := []struct{ in, want string }{
		{"harmostes/pr-review-rhesadox", "pr-review-rhesadox"},
		{"pr-review-rhesadox", "pr-review-rhesadox"},
		{"", ""},
	}
	for _, c := range cases {
		if got := workflowCRName(c.in); got != c.want {
			t.Errorf("workflowCRName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
