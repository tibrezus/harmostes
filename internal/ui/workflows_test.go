package ui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// workflowTestServer builds a Server with a fake k8s client preloaded with objects.
func workflowTestServer(existing ...client.Object) *Server {
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
		hub:       NewEventHub(),
	}
}

func TestHandleWorkflowCreate_LLMPreset(t *testing.T) {
	s := workflowTestServer()

	form := url.Values{}
	form.Set("name", "my-wiki")
	form.Set("repoUrl", "git@github.com:rezuscloud/llm-wiki.git")
	form.Set("branch", "main")
	form.Set("preset", "llm-wiki")
	form.Set("model", "litellm/zai/glm-5.2")
	form.Set("schedule", "*/30 * * * *")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "my-wiki"}, &wf); err != nil {
		t.Fatalf("workflow not created: %v", err)
	}

	// Verify owner label
	if wf.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("owner = %q, want alice", wf.Labels[v1alpha1.OwnerLabel])
	}

	// Verify preset plugins
	if wf.Spec.Prepare.Plugin.Name != "rig-emit" {
		t.Errorf("prepare plugin = %q, want rig-emit", wf.Spec.Prepare.Plugin.Name)
	}
	if wf.Spec.Agent.Gate.Plugin.Name != "wiki-lint" {
		t.Errorf("gate plugin = %q, want wiki-lint", wf.Spec.Agent.Gate.Plugin.Name)
	}
	if wf.Spec.Deploy.Plugin.Name != "git-push" {
		t.Errorf("deploy plugin = %q, want git-push", wf.Spec.Deploy.Plugin.Name)
	}

	// Verify workspace repo
	if wf.Spec.WorkspaceRepo == nil {
		t.Fatal("workspaceRepo is nil")
	}
	if wf.Spec.WorkspaceRepo.URL != "git@github.com:rezuscloud/llm-wiki.git" {
		t.Errorf("repo URL = %q", wf.Spec.WorkspaceRepo.URL)
	}
}

func TestHandleWorkflowCreate_CustomPreset(t *testing.T) {
	s := workflowTestServer()

	form := url.Values{}
	form.Set("name", "custom-wf")
	form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")
	form.Set("preset", "custom")
	form.Set("preparePlugin", "raw-copy")
	form.Set("gatePlugin", "wiki-lint")
	form.Set("deployPlugin", "git-push")
	form.Set("model", "litellm/zai/glm-4.7")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "bob"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}

	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "custom-wf"}, &wf)

	if wf.Spec.Prepare.Plugin.Name != "raw-copy" {
		t.Errorf("prepare = %q, want raw-copy", wf.Spec.Prepare.Plugin.Name)
	}
	if wf.Spec.Agent.Model != "litellm/zai/glm-4.7" {
		t.Errorf("model = %q, want glm-4.7", wf.Spec.Agent.Model)
	}
}

func TestHandleWorkflowCreate_WithTokenRef(t *testing.T) {
	s := workflowTestServer()

	// Pre-create a token secret for alice
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-github-abcd1234",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
				TokenLabel:          "github",
			},
		},
	}
	_ = s.k8sClient.Create(context.Background(), token)

	form := url.Values{}
	form.Set("name", "tokenized-wf")
	form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")
	form.Set("preset", "llm-wiki")
	form.Set("tokenSecret", "alice-github-abcd1234")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "tokenized-wf"}, &wf)

	if wf.Spec.WorkspaceRepo.TokenRef == nil {
		t.Fatal("tokenRef is nil — should reference the per-user token")
	}
	if wf.Spec.WorkspaceRepo.TokenRef.Name != "alice-github-abcd1234" {
		t.Errorf("tokenRef.Name = %q", wf.Spec.WorkspaceRepo.TokenRef.Name)
	}
}

func TestHandleWorkflowCreate_RejectsEmptyName(t *testing.T) {
	s := workflowTestServer()

	form := url.Values{}
	form.Set("name", "")
	form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (error page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Error("expected error about name being required")
	}
}

func TestHandleWorkflowCreate_RejectsInvalidName(t *testing.T) {
	s := workflowTestServer()

	cases := []string{"My-Workflow", "wf with space", "-leading-dash", ""}
	for _, badName := range cases {
		form := url.Values{}
		form.Set("name", badName)
		form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")
		form.Set("preset", "llm-wiki")

		req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

		rec := httptest.NewRecorder()
		s.handleWorkflowCreate(rec, req)

		// Should render error page (200), not redirect (303)
		if rec.Code == http.StatusSeeOther && badName != "" {
			t.Errorf("invalid name %q should be rejected", badName)
		}
	}
}

func TestHandleWorkflowCreate_DuplicateName(t *testing.T) {
	existing := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	s := workflowTestServer(existing)

	form := url.Values{}
	form.Set("name", "existing-wf")
	form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")
	form.Set("preset", "llm-wiki")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Error("expected 'already exists' error")
	}
}

func TestHandleWorkflowCreate_OwnerNeverSpoofed(t *testing.T) {
	s := workflowTestServer()

	// Even though the form has no owner field, the server should stamp "alice"
	// from the authenticated identity. A malicious client CANNOT inject an
	// owner label via the form (there's no owner form field, and StampOwnerLabel
	// overwrites any existing label).
	form := url.Values{}
	form.Set("name", "spoof-test")
	form.Set("repoUrl", "git@github.com:rezuscloud/repo.git")
	form.Set("preset", "llm-wiki")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "spoof-test"}, &wf)

	if wf.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("owner = %q, want alice (server-set, not client-supplied)", wf.Labels[v1alpha1.OwnerLabel])
	}
}

func TestHandleWorkflowDelete_OwnerIsolation(t *testing.T) {
	bobWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bobs-workflow",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "bob"},
		},
	}
	s := workflowTestServer(bobWf)

	// Alice tries to delete Bob's workflow
	req := httptest.NewRequest(http.MethodPost, "/workflows/bobs-workflow/delete", nil)
	req.SetPathValue("name", "bobs-workflow")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowDelete(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-tenant delete must fail)", rec.Code, http.StatusNotFound)
	}

	// Verify still exists
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "bobs-workflow"}, &wf); err != nil {
		t.Errorf("bob's workflow should still exist: %v", err)
	}
}

func TestHandleWorkflowDelete_Success(t *testing.T) {
	aliceWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	s := workflowTestServer(aliceWf)

	req := httptest.NewRequest(http.MethodPost, "/workflows/alice-wf/delete", nil)
	req.SetPathValue("name", "alice-wf")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowDelete(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// Verify deleted
	var wf v1alpha1.Workflow
	err := s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "alice-wf"}, &wf)
	if err == nil {
		t.Error("workflow should have been deleted")
	}
}

func TestHandleWorkflowDelete_RejectsUnmanagedWorkflow(t *testing.T) {
	// A workflow without an owner label (GitOps-created system workflow) must
	// NOT be deletable from the self-service UI.
	systemWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "system-workflow",
			Namespace: "harmostes",
			Labels:    map[string]string{}, // no owner label
		},
	}
	s := workflowTestServer(systemWf)

	req := httptest.NewRequest(http.MethodPost, "/workflows/system-workflow/delete", nil)
	req.SetPathValue("name", "system-workflow")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowDelete(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (unmanaged workflow not deletable)", rec.Code, http.StatusNotFound)
	}
}

func TestHandleWorkflowTrigger_SetsAnnotation(t *testing.T) {
	aliceWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	s := workflowTestServer(aliceWf)

	req := httptest.NewRequest(http.MethodPost, "/workflows/alice-wf/trigger", nil)
	req.SetPathValue("name", "alice-wf")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowTrigger(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// Verify trigger annotation was set
	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "alice-wf"}, &wf)

	if wf.Annotations == nil {
		t.Fatal("annotations is nil")
	}
	triggerRev := wf.Annotations[triggerAnnotation]
	if triggerRev == "" {
		t.Fatal("trigger-revision annotation not set")
	}
	if !strings.HasPrefix(triggerRev, "manual-") {
		t.Errorf("trigger value = %q, want prefix manual-", triggerRev)
	}
}

func TestHandleWorkflowTrigger_OwnerIsolation(t *testing.T) {
	bobWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bobs-workflow",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "bob"},
		},
	}
	s := workflowTestServer(bobWf)

	req := httptest.NewRequest(http.MethodPost, "/workflows/bobs-workflow/trigger", nil)
	req.SetPathValue("name", "bobs-workflow")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowTrigger(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-tenant trigger must fail)", rec.Code, http.StatusNotFound)
	}

	// Verify annotation was NOT set
	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "bobs-workflow"}, &wf)
	if wf.Annotations != nil && wf.Annotations[triggerAnnotation] != "" {
		t.Error("trigger annotation should NOT have been set by cross-tenant user")
	}
}

func TestHandleWorkflowToggle(t *testing.T) {
	aliceWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-wf",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
	}
	s := workflowTestServer(aliceWf)

	// First toggle: enabled → disabled
	req := httptest.NewRequest(http.MethodPost, "/workflows/alice-wf/toggle", nil)
	req.SetPathValue("name", "alice-wf")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowToggle(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "alice-wf"}, &wf)

	if !wf.Spec.Disabled {
		t.Error("workflow should be disabled after toggle")
	}

	// Second toggle: disabled → enabled
	rec2 := httptest.NewRecorder()
	s.handleWorkflowToggle(rec2, req)

	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "alice-wf"}, &wf)
	if wf.Spec.Disabled {
		t.Error("workflow should be enabled after second toggle")
	}
}

func TestHandleWorkflowToggle_OwnerIsolation(t *testing.T) {
	bobWf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bobs-workflow",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "bob"},
		},
	}
	s := workflowTestServer(bobWf)

	req := httptest.NewRequest(http.MethodPost, "/workflows/bobs-workflow/toggle", nil)
	req.SetPathValue("name", "bobs-workflow")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowToggle(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-tenant toggle must fail)", rec.Code, http.StatusNotFound)
	}

	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "bobs-workflow"}, &wf)
	if wf.Spec.Disabled {
		t.Error("workflow should not have been toggled by cross-tenant user")
	}
}

func TestPresetFor(t *testing.T) {
	// Known presets
	p := presetFor("llm-wiki")
	if p.Prepare != "rig-emit" {
		t.Errorf("llm-wiki prepare = %q", p.Prepare)
	}

	p = presetFor("fork-maintenance")
	if p.Gate != "fork-resolved" {
		t.Errorf("fork-maintenance gate = %q", p.Gate)
	}

	// Unknown → falls back to custom
	p = presetFor("nonexistent")
	if p.ID != "custom" {
		t.Errorf("unknown preset should fall back to custom, got %q", p.ID)
	}
}

func TestWorkflowNameRe_RejectsInvalid(t *testing.T) {
	invalid := []string{"UPPER", "spaces here", "-leading", "trailing-", "under_score"}
	for _, name := range invalid {
		if workflowNameRe.MatchString(name) {
			t.Errorf("name %q should be rejected", name)
		}
	}
	valid := []string{"my-wiki", "wf-123", "a", "abc-def-123"}
	for _, name := range valid {
		if !workflowNameRe.MatchString(name) {
			t.Errorf("name %q should be accepted", name)
		}
	}
}
