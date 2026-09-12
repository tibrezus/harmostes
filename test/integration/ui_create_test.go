//go:build integration

package integration

// The UI-create acceptance test (#436): the fake clientset the component
// tier drives is schema-blind, so a created Workflow was never proven
// acceptable to a REAL apiserver. This test drives the UI's own create
// route against envtest (chart CRDs installed) and pins the full
// round-trip: the apiserver accepts the object, the owner label carries
// the dev-identity prefix, and the instance lands disabled — the e2e tier
// then verifies the owner-driven visibility on top.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/ui"
)

func TestUIWorkflowCreateAcceptedByRealAPIserver(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()

	// The template the form instantiates — created through the real
	// apiserver, so the CRD validates it here already.
	tmpl := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: "default"},
		Spec: v1alpha1.WorkflowTemplateSpec{
			Description: "PR review",
			Scope: []v1alpha1.ScopeParam{{
				Name: "repos", Kind: "list", Label: "Repos",
				Description: "the repos the prepare plugin operates on",
			}},
			Agent: v1alpha1.AgentSpec{Model: "mistral-small-latest"},
		},
	}
	if err := c.Create(ctx, tmpl); err != nil {
		t.Fatalf("seed template: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := ui.New(c, "default", logger, nil, nil)
	if err != nil {
		t.Fatalf("ui server: %v", err)
	}
	// The fixture/dev posture: dev writes on, identity via the dev header.
	s.SetDevWriteEnabled(true)
	ts := httptest.NewServer(s.Routes())
	defer ts.Close()

	form := url.Values{
		"name":        {"ui-created-acceptance"},
		"templateRef": {"pr-review"},
		"sourceKind":  {"webhook"},
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/workflows", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Harmostes-Dev-User", "writer")
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // the 303 IS the assertion target
	}}
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST /workflows: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /workflows = %d, want 303 — body: %s", res.StatusCode, readAll(res))
	}
	if loc := res.Header.Get("Location"); loc != "/workflows/ui-created-acceptance" {
		t.Errorf("Location = %q", loc)
	}

	// THE point of this tier: the REAL apiserver accepted and holds the
	// object — the fake client's schema-blindness cannot hide a rejected
	// create here.
	wf := &v1alpha1.Workflow{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "ui-created-acceptance"}, wf); err != nil {
		t.Fatalf("created object not readable from the apiserver: %v", err)
	}
	if got := wf.Labels[v1alpha1.OwnerLabel]; got != "dev-writer" {
		t.Errorf("owner label = %q, want dev-writer (dev identities must never occupy real owner labels)", got)
	}
	if wf.Spec.TemplateRef != "pr-review" {
		t.Errorf("templateRef = %q, want pr-review", wf.Spec.TemplateRef)
	}
	if !wf.Spec.Disabled {
		t.Error("created instances must land disabled (the trigger arms them)")
	}
}

func readAll(res *http.Response) string {
	b := make([]byte, 512)
	n, _ := res.Body.Read(b)
	return string(b[:n])
}
