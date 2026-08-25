package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
		k8sClient:  cl,
		namespace:  "harmostes",
		logger:     slog.Default(),
		templates:  tmpl,
		hub:        NewEventHub(),
		nodePolicy: nil,
		platforms:  newPlatformRegistry(DefaultPlatformConfigs()),
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
		form.Set("gate", "wiki-lint")

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

func TestHandleWorkflowCreate_OwnerNeverSpoofed(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	// Even though the form has no owner field, the server should stamp "alice"
	// from the authenticated identity. A malicious client CANNOT inject an
	// owner label via the form (there's no owner form field, and StampOwnerLabel
	// overwrites any existing label).
	form := url.Values{}
	form.Set("name", "spoof-test")
	form.Set("templateRef", "pr-review")

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

func TestParseGitURL(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantObj  string
	}{
		{"git@github.com:rezuscloud/llm-wiki.git", "github.com", "rezuscloud/llm-wiki"},
		{"https://github.com/rezuscloud/llm-wiki.git", "github.com", "rezuscloud/llm-wiki"},
		{"https://gitlab.com/tibrez/operations/k8s-config", "gitlab.com", "tibrez/operations/k8s-config"},
		{"", "", ""},
	}
	for _, c := range cases {
		host, obj := parseGitURL(c.url)
		if host != c.wantHost || obj != c.wantObj {
			t.Errorf("parseGitURL(%q) = (%q, %q), want (%q, %q)", c.url, host, obj, c.wantHost, c.wantObj)
		}
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

// prReviewTemplate builds the standard pr-review WorkflowTemplate for tests.
func prReviewTemplate() *v1alpha1.WorkflowTemplate {
	return &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowTemplateSpec{
			Description: "PR review",
			Scope: []v1alpha1.ScopeParam{
				{Name: "label", Kind: "string", Label: "Label trigger", Default: "needs-review", Description: "only act when this label is present"},
				{Name: "repos", Kind: "list", Label: "Repos", Description: "the scope the prepare plugin operates on"},
				{Name: "wiki", Kind: "string", Label: "Wiki repo", Description: "context repo some agents use for design evidence"},
			},
			Prepare: v1alpha1.PrepareSpec{
				Plugin: v1alpha1.PluginRef{Name: "pr-fetch", ConfigMap: "harmostes-pr-review"},
				Detect: "changed",
			},
			Agent: v1alpha1.AgentSpec{
				Model:        "litellm/zai/glm-4.7",
				Skill:        "/skills/pr-review/SKILL.md",
				Tools:        []string{"read", "bash", "grep"},
				TaskTemplate: v1alpha1.TaskTemplate{Name: "pr-review", ConfigMap: "harmostes-tasks", Key: "pr-review.txt"},
				Gate:         v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review", ConfigMap: "harmostes-pr-review"}},
				MaxFixes:     1,
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review", ConfigMap: "harmostes-pr-review"}},
		},
	}
}

func TestHandleWorkflowCreate_TemplateInstance(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	form := url.Values{}
	form.Set("name", "pr-review-harmostes")
	form.Set("templateRef", "pr-review")
	form.Set("schedule", "*/10 * * * *")
	form.Set("label", "needs-review")
	form.Set("repos", "tibrezus/harmostes, github.com/other/repo")
	form.Set("wiki", "")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "pr-review-harmostes"}, &wf); err != nil {
		t.Fatalf("workflow not created: %v", err)
	}
	if wf.Spec.TemplateRef != "pr-review" {
		t.Errorf("templateRef = %q, want pr-review", wf.Spec.TemplateRef)
	}
	if wf.Spec.Source.Kind != "schedule" || wf.Spec.Source.Schedule != "*/10 * * * *" {
		t.Errorf("source = %+v, want schedule */10", wf.Spec.Source)
	}
	var cfg map[string]any
	if err := json.Unmarshal(wf.Spec.Config, &cfg); err != nil {
		t.Fatalf("config not JSON: %v", err)
	}
	if repos, _ := cfg["repos"].([]any); len(repos) != 2 || repos[0] != "tibrezus/harmostes" {
		t.Errorf("config repos = %v, want 2 repos", cfg["repos"])
	}
	// Owner is stamped from the authenticated identity — the creation
	// invariant: every created workflow is visible to its creator.
	if wf.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("owner label = %q, want alice", wf.Labels[v1alpha1.OwnerLabel])
	}
	// The stored CR stays thin: pipeline shape lives in the template.
	if wf.Spec.Agent.Model != "" || wf.Spec.Prepare.Plugin.Name != "" {
		t.Errorf("thin instance must not duplicate template spec, got agent=%+v prepare=%+v", wf.Spec.Agent, wf.Spec.Prepare)
	}
}

func TestHandleWorkflowCreate_TemplateInstanceUnknownTemplate(t *testing.T) {
	s := workflowTestServer()

	form := url.Values{}
	form.Set("name", "orphan")
	form.Set("templateRef", "does-not-exist")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusOK { // renders the error page
		t.Fatalf("status = %d, want 200 (error page)", rec.Code)
	}
	var list v1alpha1.WorkflowList
	_ = s.k8sClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("no workflow should be created for unknown template, got %d", len(list.Items))
	}
}

func TestResolveWorkflow_MergesTemplate(t *testing.T) {
	thin := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-review-harmostes",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			TemplateRef: "pr-review",
			Source:      v1alpha1.SourceSpec{Kind: "schedule", Schedule: "*/10 * * * *"},
			Config:      json.RawMessage(`{"label":"needs-review","repos":["tibrezus/harmostes"]}`),
		},
	}
	s := workflowTestServer(prReviewTemplate(), thin)

	merged := s.resolveWorkflow(context.Background(), thin)
	if merged.Spec.Agent.Model != "litellm/zai/glm-4.7" {
		t.Errorf("merged model = %q, want inherited", merged.Spec.Agent.Model)
	}
	if merged.Spec.Prepare.Plugin.Name != "pr-fetch" {
		t.Errorf("merged prepare = %q, want pr-fetch", merged.Spec.Prepare.Plugin.Name)
	}
	if merged.Spec.Deploy.Plugin.Name != "post-review" {
		t.Errorf("merged deploy = %q, want post-review", merged.Spec.Deploy.Plugin.Name)
	}
	if string(merged.Spec.Prepare.Config) == "" {
		t.Error("instance config must overlay prepare.config")
	}
	// The stored CR is never mutated by resolution.
	if thin.Spec.Agent.Model != "" {
		t.Error("resolveWorkflow must not mutate the stored thin spec")
	}
}

func TestResolveWorkflow_MissingTemplateDegrades(t *testing.T) {
	thin := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "harmostes"},
		Spec:       v1alpha1.WorkflowSpec{TemplateRef: "gone"},
	}
	s := workflowTestServer()
	got := s.resolveWorkflow(context.Background(), thin)
	if got.Spec.TemplateRef != "gone" || got.Spec.Agent.Model != "" {
		t.Error("missing template must degrade to the thin spec unchanged")
	}
}

func TestHandleWorkflowDetail_ThinInstanceRendersMergedPipeline(t *testing.T) {
	thin := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-review-harmostes",
			Namespace: "harmostes",
			Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
		},
		Spec: v1alpha1.WorkflowSpec{
			TemplateRef: "pr-review",
			Source:      v1alpha1.SourceSpec{Kind: "schedule", Schedule: "*/10 * * * *"},
		},
	}
	s := workflowTestServer(prReviewTemplate(), thin)

	req := httptest.NewRequest(http.MethodGet, "/workflows/pr-review-harmostes", nil)
	req.SetPathValue("name", "pr-review-harmostes")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	rec := httptest.NewRecorder()
	s.handleWorkflowDetail(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The pipeline renders node sublabels from the compiled merged spec:
	// prepare=pr-fetch, agent present, deploy=post-review.
	if !strings.Contains(body, "pr-fetch") || !strings.Contains(body, "post-review") {
		t.Error("detail page must render the merged pipeline (template shape) for thin instances")
	}
	if !strings.Contains(body, "AGENT") {
		t.Error("detail page must render the agent node for thin instances")
	}
}

func TestHandleWorkflowCreate_CustomScopeDialect(t *testing.T) {
	// A template may declare an arbitrary configuration dialect: the form
	// and the stored instance config follow the declaration, with no UI
	// code knowing these keys.
	tmpl := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "deploy-thing", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowTemplateSpec{
			Description: "deploy thing",
			Scope: []v1alpha1.ScopeParam{
				{Name: "env", Kind: "string", Label: "Environment", Default: "staging"},
				{Name: "targets", Kind: "list", Label: "Targets"},
			},
			Prepare: v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "noop"}},
			Agent:   v1alpha1.AgentSpec{Enabled: boolPtr(false), Model: "none", Gate: v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "noop"}}},
			Deploy:  v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "noop"}},
		},
	}
	s := workflowTestServer(tmpl)

	form := url.Values{}
	form.Set("name", "deploy-thing-prod")
	form.Set("templateRef", "deploy-thing")
	form.Set("schedule", "0 * * * *")
	form.Set("env", "prod")
	form.Set("targets", "cluster-a, cluster-b")
	// Stray keys the template does NOT declare must be ignored.
	form.Set("label", "needs-review")

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "deploy-thing-prod"}, &wf); err != nil {
		t.Fatalf("not created: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(wf.Spec.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["env"] != "prod" {
		t.Errorf("env = %v, want prod", cfg["env"])
	}
	if tg, _ := cfg["targets"].([]any); len(tg) != 2 || tg[0] != "cluster-a" {
		t.Errorf("targets = %v, want [cluster-a cluster-b]", cfg["targets"])
	}
	if _, exists := cfg["label"]; exists {
		t.Error("undeclared key 'label' must not be stored")
	}
}

func TestHandleWorkflowCreate_ScopeDefaultsApply(t *testing.T) {
	tmpl := prReviewTemplate()
	s := workflowTestServer(tmpl)

	form := url.Values{}
	form.Set("name", "pr-review-defaults")
	form.Set("templateRef", "pr-review")
	form.Set("repos", "tibrezus/harmostes")
	// label + wiki left empty → defaults (needs-review) / empty string

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))
	rec := httptest.NewRecorder()
	s.handleWorkflowCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}
	var wf v1alpha1.Workflow
	_ = s.k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "pr-review-defaults"}, &wf)
	var cfg map[string]any
	_ = json.Unmarshal(wf.Spec.Config, &cfg)
	if cfg["label"] != "needs-review" {
		t.Errorf("label default not applied: %v", cfg["label"])
	}
}

// #245: the workflows page is the navigation hub — one dense row per
// workflow, CR name primary (unique — slugs can collide across hosts),
// quick actions presetting ?workflow=.
func TestWorkflowListHubTable(t *testing.T) {
	mkWf := func(name, tmpl, repo string) *v1alpha1.Workflow {
		return &v1alpha1.Workflow{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "test-ns",
				Labels:    map[string]string{v1alpha1.OwnerLabel: "alice"},
			},
			Spec: v1alpha1.WorkflowSpec{
				TemplateRef: tmpl,
				Source:      v1alpha1.SourceSpec{Repo: repo},
			},
		}
	}
	wfs := []*v1alpha1.Workflow{
		mkWf("pr-review-harmostes", "pr-review", "github.com/tibrezus/harmostes"),
		mkWf("pr-review-rhesadox", "pr-review", "git.rezus.cloud/tibrez/rhesadox"),
		mkWf("signoz", "sync", "github.com/rezuscloud/signoz"),
	}
	s := newAttemptTestServer(t, wfs[0], wfs[1], wfs[2])
	req := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	req = req.WithContext(withTestIdentity(req.Context()))
	rec := httptest.NewRecorder()
	s.handleWorkflowList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The hub table, one row per unique CR name.
	for _, want := range []string{"wf-table", "wf-row", "pr-review-harmostes", "pr-review-rhesadox", "signoz"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Quick actions preset the workflow context.
	for _, href := range []string{
		`href="/timeline?workflow=pr-review-harmostes"`,
		`href="/sessions?workflow=pr-review-harmostes"`,
		`href="/workflows/pr-review-harmostes"`,
	} {
		if !strings.Contains(body, href) {
			t.Errorf("missing quick action %q", href)
		}
	}
	// Template grouping: one pr-review group carrying both instances.
	if got := strings.Count(body, "gate-group-title"); got != 2 {
		t.Errorf("gate-group-title count = %d, want 2 (pr-review, sync)", got)
	}
}
