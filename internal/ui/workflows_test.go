package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	_ = apiextensionsv1.AddToScheme(scheme)
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
	// Quick actions preset the workflow context (timeline/sessions views are
	// removed destinations — #290).
	for _, href := range []string{
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

// TestLegacyWriteSurfacesStillRemoved pins the #291 pruning that ADR-0012
// deliberately does NOT undo: lifecycle mutations (trigger/toggle/delete)
// and graph write handlers (PUT/convert/create-by-canvas — the canvas is
// dismantled, ADR-0012 §3) exist in no route table and must never re-arm
// except through their own ADR-0012 issues (#418/#419). Creation is the one
// restored surface (tested below) — everything else stays unreachable:
// never 2xx, never a redirect.
func TestLegacyWriteSurfacesStillRemoved(t *testing.T) {
	s := workflowTestServer()

	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/workflows/pr-review-harmostes/trigger"},
		{http.MethodPost, "/workflows/pr-review-harmostes/toggle"},
		{http.MethodPost, "/workflows/pr-review-harmostes/delete"},
		// Graph write handlers exist in no route table and must never re-arm:
		// PUT (spec.graph overwrite), PATCH-style convert, and create-by-canvas
		// are all mutations the ADR-0012 architecture explicitly rejects
		// (read-only auto-layouted projection, #417).
		{http.MethodPut, "/api/workflows/pr-review-harmostes/graph"},
		{http.MethodPost, "/api/workflows/pr-review-harmostes/graph"},
		{http.MethodPost, "/api/workflows/pr-review-harmostes/convert"},
		// The JSON-style create endpoint never existed; the create surface is
		// the form post (POST /workflows).
		{http.MethodPost, "/api/workflows"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: removed write surface reachable (status %d)", tc.method, tc.path, rec.Code)
		}
	}

	// The index IS the live wall now (milestone 3): real page, no redirect.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/ status = %d, want 200 (live wall)", rec.Code)
	}
	for _, marker := range []string{"wall-grid", "EventSource('/api/wall/events')"} {
		if !strings.Contains(rec.Body.String(), marker) {
			t.Errorf("/ missing wall marker %q", marker)
		}
	}
}

// TestWorkflowCreationForm pins the restored creation surface (ADR-0012 §5):
// GET /workflows/new renders the form from the template catalog, with the
// per-template scope fields the selected template declares.
func TestWorkflowCreationForm(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	req := httptest.NewRequest(http.MethodGet, "/workflows/new", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workflows/new status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{
		"wf-new-form", `action="/workflows"`, "New Workflow",
		`value="pr-review"`, // the template radio
		// Scope params render template-prefixed: hidden fieldsets submit too,
		// so unprefixed names could collide across templates.
		`name="scope-pr-review-label"`,
		`name="scope-pr-review-repos"`,
		`name="scope-pr-review-wiki"`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("GET /workflows/new: missing marker %q", marker)
		}
	}
	// The schedule field must stay gone: the controller's trigger decision is
	// poll-driven and never parses a cron string — advertising one would be a
	// dead knob with a plausible label (PR #427 review, Pillar 2).
	if strings.Contains(body, `name="schedule"`) {
		t.Error("GET /workflows/new: schedule field re-armed — it is not honoured by the controller")
	}
}

// TestWorkflowCreate_Instance drives the full POST: the stored CR must be
// thin (templateRef + source + config), carry the SERVER-stamped owner label
// (never a client value), and store only scope keys the template declares —
// an undeclared form field must not survive into spec.config.
func TestWorkflowCreate_Instance(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(
		"name=pr-review-demo&templateRef=pr-review"+
			"&scope-pr-review-label=needs-review&scope-pr-review-repos=a%2Cb&scope-pr-review-wiki=docs"+
			// The actual spoof vector: client-supplied owner-ish fields must
			// never reach the stored object (the stamp comes from the session).
			"&owner=ghost&metadata.labels.harmostes.dev~1owner=ghost"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /workflows status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/workflows/pr-review-demo" {
		t.Errorf("redirect = %q, want /workflows/pr-review-demo", loc)
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "pr-review-demo"}, &wf); err != nil {
		t.Fatalf("created CR not found: %v", err)
	}
	if got := wf.Labels[v1alpha1.OwnerLabel]; got != "alice" {
		t.Errorf("owner label = %q, want alice (stamped from session, never client)", got)
	}
	if wf.Spec.TemplateRef != "pr-review" {
		t.Errorf("templateRef = %q, want pr-review", wf.Spec.TemplateRef)
	}
	if !wf.Spec.Disabled {
		t.Error("created workflow is not disabled — creation must not arm an unattended agent loop (PR #427 review R1)")
	}
	if wf.Spec.Source.Kind != "schedule" {
		t.Errorf("source.kind = %q, want schedule (non-wake for the claim sweep)", wf.Spec.Source.Kind)
	}
	if wf.Spec.Source.Schedule != "" {
		t.Errorf("source.schedule = %q, want empty — the form must not advertise a cron the controller never parses", wf.Spec.Source.Schedule)
	}
	var cfg map[string]any
	if err := json.Unmarshal(wf.Spec.Config, &cfg); err != nil {
		t.Fatalf("spec.config not JSON: %v", err)
	}
	// Exact key set: every declared scope param, nothing else — no client
	// field (owner, injected, or otherwise) survives into spec.config.
	wantKeys := map[string]bool{}
	for _, p := range prReviewTemplate().Spec.Scope {
		wantKeys[p.Name] = true
	}
	if len(cfg) != len(wantKeys) {
		t.Errorf("config keys = %v, want exactly the declared scope %v", cfg, wantKeys)
	}
	for key := range cfg {
		if !wantKeys[key] {
			t.Errorf("undeclared key %q stored — config smuggling possible", key)
		}
	}
	if got, ok := cfg["label"].(string); !ok || got != "needs-review" {
		t.Errorf("label = %v, want \"needs-review\" (string scope)", cfg["label"])
	}
	if repos, ok := cfg["repos"].([]any); !ok || len(repos) != 2 {
		t.Errorf("repos = %v, want a 2-element list (comma-separated form value)", cfg["repos"])
	}
}

// TestWorkflowCreate_AntiSpoof pins the write gate: creation requires an
// Authentik-authoritative identity — or the explicit dev identity on a
// server that was STARTED with dev writes enabled. Legacy X-Forwarded-*
// headers are client-suppliable and never qualify, with or without the flag:
// a forged forwarded username can browse, but can never create under someone
// else's owner label.
func TestWorkflowCreate_AntiSpoof(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{{
		name:    "authoritative identity writes",
		headers: map[string]string{"X-Authentik-Username": "alice"},
		want:    http.StatusSeeOther,
	}, {
		name:    "dev identity without server opt-in is forbidden",
		headers: map[string]string{"X-Harmostes-Dev-User": "devuser"},
		want:    http.StatusForbidden,
	}, {
		name: "forwarded-only identity is forbidden",
		headers: map[string]string{
			"X-Forwarded-User":   "alice",
			"X-Forwarded-Groups": "admins",
		},
		want: http.StatusForbidden,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/workflows",
				strings.NewReader("name=w-"+strings.ReplaceAll(tc.name, " ", "-")+"&templateRef=pr-review"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	// With the explicit server-side opt-in (fixture mode, or a dev-values
	// chart render), the dev identity writes.
	s.SetDevWriteEnabled(true)
	req := httptest.NewRequest(http.MethodPost, "/workflows",
		strings.NewReader("name=w-dev-optin&templateRef=pr-review"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Harmostes-Dev-User", "devuser")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("dev identity with server opt-in: status = %d, want 303", rec.Code)
	}

	// The rejections happened BEFORE any object was created: no workflow may
	// exist under a forged identity.
	wfs, err := s.listWorkflows(httptest.NewRequest(http.MethodGet, "/", nil), "alice")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	if len(wfs) != 1 { // only the authoritative case
		t.Errorf("workflows visible to alice = %d, want 1 (forgeries must not create)", len(wfs))
	}
}

// TestWorkflowCreate_ErrorCases pins the validation contract WITH status
// codes (PR #427 review round 4): 400 for client mistakes (unknown template,
// invalid name, missing templateRef), 409 for the duplicate — never a
// blanket 200.
func TestWorkflowCreate_ErrorCases(t *testing.T) {
	existing := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name: "taken", Namespace: "harmostes",
			Labels: map[string]string{v1alpha1.OwnerLabel: "bob"},
		},
	}
	s := workflowTestServer(prReviewTemplate(), existing)

	cases := []struct {
		name, form, wantMsg string
		wantCode            int
	}{
		{"unknown template", "name=w1&templateRef=nope", "Unknown template", http.StatusBadRequest},
		{"invalid name", "name=UPPER&templateRef=pr-review", "Invalid workflow name", http.StatusBadRequest},
		{"missing templateRef", "name=w2", "A template must be selected", http.StatusBadRequest},
		{"duplicate name", "name=taken&templateRef=pr-review", "already exists", http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(tc.form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Authentik-Username", "alice")
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("error page missing %q", tc.wantMsg)
			}
		})
	}
}

// TestWorkflowCreate_CrossOriginRejected pins the CSRF guard: browsers send
// Origin on cross-site POSTs — a mismatched origin never reaches creation,
// so the (disabled-instance) forgery blast radius stays at zero.
func TestWorkflowCreate_CrossOriginRejected(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())

	cases := []struct {
		name, origin string
		want         int
	}{
		{"same origin passes", "http://" + "example.com", http.StatusSeeOther},
		{"cross origin rejected", "https://evil.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/workflows",
				strings.NewReader("name=cors-probe&templateRef=pr-review"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-Authentik-Username", "alice")
			rec := httptest.NewRecorder()
			s.Routes().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("Origin %q: status = %d, want %d", tc.origin, rec.Code, tc.want)
			}
		})
	}
}

// TestWorkflowCreate_VisibilityParity extends the ownership coupling
// invariant (ADR-0012 §5) to creation: what a user creates is visible to
// them and to admins, invisible to other users.
func TestWorkflowCreate_VisibilityParity(t *testing.T) {
	s := workflowTestServer(prReviewTemplate())
	s.SetAdminGroups([]string{"harmostes-admins"})

	req := httptest.NewRequest(http.MethodPost, "/workflows",
		strings.NewReader("name=alice-only&templateRef=pr-review"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d, want 303", rec.Code)
	}

	get := func(user string, groups ...string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/workflows", nil)
		req.Header.Set("X-Authentik-Username", user)
		if len(groups) > 0 {
			req.Header.Set("X-Authentik-Groups", strings.Join(groups, "|"))
		}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /workflows as %s: status %d", user, rec.Code)
		}
		return rec.Body.String()
	}
	if body := get("alice"); !strings.Contains(body, "alice-only") {
		t.Error("alice cannot see the workflow she created")
	}
	if body := get("admin-user", "harmostes-admins"); !strings.Contains(body, "alice-only") {
		t.Error("admin cannot see alice's workflow — bypass broken")
	}
	if body := get("mallory"); strings.Contains(body, "alice-only") {
		t.Error("mallory sees alice's workflow — visibility parity broken")
	}
}
