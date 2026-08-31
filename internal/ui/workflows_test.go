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
		platforms: newPlatformRegistry(DefaultPlatformConfigs()),
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

// TestWriteSurfaceRemoved pins the observe-only contract (#290): the UI has
// no create, trigger, toggle, or delete surfaces. Each removed endpoint must
// be unreachable — never 2xx, never a redirect (a redirect would mean the
// route still does something).
func TestWriteSurfaceRemoved(t *testing.T) {
	s := workflowTestServer()

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/workflows/new"},
		{http.MethodPost, "/workflows"},
		{http.MethodPost, "/workflows/pr-review-harmostes/trigger"},
		{http.MethodPost, "/workflows/pr-review-harmostes/toggle"},
		{http.MethodPost, "/workflows/pr-review-harmostes/delete"},
	}
	for _, tc := range cases {
		if tc.method == http.MethodGet {
			continue // GET /workflows/new asserted separately (content-level)
		}
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: write surface still reachable (status %d)", tc.method, tc.path, rec.Code)
		}
	}

	// GET /workflows/new falls through to the (missing) workflow "new" and
	// renders the app's error page. The creation form itself must be gone.
	req := httptest.NewRequest(http.MethodGet, "/workflows/new", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	for _, marker := range []string{"wf-new-form", `action="/workflows"`, "New Workflow"} {
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("GET /workflows/new: creation surface marker %q still served", marker)
		}
	}

	// The index must land on the runs history (the interim spine until the
	// live wall exists), not the removed map view.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/attempts" {
		t.Errorf("/ redirect: got status %d Location %q, want 303 -> /attempts", rec.Code, loc)
	}
}
