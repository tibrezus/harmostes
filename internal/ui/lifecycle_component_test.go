package ui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"github.com/tibrezus/harmostes/internal/ui/fixture"
)

// Fixture-tier DOM contract for the lifecycle controls (#418, ADR-0012 §7).
// The fixture server runs with dev writes enabled, so the fixture user is
// write-capable: controls must render; the CTA must render.

// postForm posts a urlencoded form to the fixture server with the fixture
// user's dev identity.
func postForm(t *testing.T, ts *httptest.Server, path string, form url.Values) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Harmostes-Dev-User", fixture.DevUser)
	rec := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(rec, req)
	return rec, nil
}

// parseHTML wraps a response body in goquery.
func parseHTML(r io.Reader) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(r)
}

// TestComponent_WorkflowDetail_RunControls: the detail page renders the
// controls matching the instance's state — paused ⇒ Arm (+ Delete);
// armed ⇒ Trigger + Pause (+ Delete). State-appropriate verbs only: a
// paused instance must not show a Trigger button (the server would 409 it).
func TestComponent_WorkflowDetail_RunControls(t *testing.T) {
	ts := newFixtureServer(t)

	// pr-review-demo is armed (no spec.disabled) — webhook-sourced.
	doc := getAsFixtureUser(t, ts, "/workflows/pr-review-demo")
	for _, id := range []string{"wf-controls", "wf-trigger", "wf-disable", "wf-delete"} {
		if n := doc.Find(`[data-testid="` + id + `"]`).Length(); n != 1 {
			t.Errorf("armed detail: data-testid=%s count = %d, want 1", id, n)
		}
	}
	// State-appropriate verbs only: an armed instance must not offer Arm.
	if n := doc.Find(`[data-testid="wf-enable"]`).Length(); n != 0 {
		t.Errorf("armed detail renders Arm %d time(s) — the state verbs are state-appropriate, not all verbs", n)
	}
	if action, _ := doc.Find(`[data-testid="wf-trigger"]`).Closest("form").Attr("action"); action != "/workflows/pr-review-demo/trigger" {
		t.Errorf("trigger form action = %q", action)
	}

	// The paused case comes from creation: POST a fresh instance via the
	// form (the same surface e2e drives), then inspect its detail page.
	form := url.Values{}
	form.Set("name", "created-controls-probe")
	form.Set("templateRef", "pr-review")
	form.Set("sourceKind", "webhook")
	resp, err := postForm(t, ts, "/workflows", form)
	if err != nil || resp.Code != http.StatusSeeOther {
		t.Fatalf("create: status %v err %v", resp, err)
	}
	doc = getAsFixtureUser(t, ts, "/workflows/created-controls-probe")
	if n := doc.Find(`[data-testid="wf-enable"]`).Length(); n != 1 {
		t.Errorf("paused detail: Arm button count = %d, want 1", n)
	}
	if n := doc.Find(`[data-testid="wf-trigger"]`).Length(); n != 0 {
		t.Errorf("paused detail renders Trigger %d time(s) — the server 409s that verb while paused; the button must not offer it", n)
	}
	if body := doc.Text(); !strings.Contains(body, "Paused") {
		t.Error("paused detail lacks the Paused state badge")
	}
}

// TestComponent_Workflows_CTAGatedOnMayWrite (#436): the New Workflow CTA
// renders for a write-capable identity and is absent for a forwarded-only
// (read-only) one — whose catalog view works unchanged.
func TestComponent_Workflows_CTAGatedOnMayWrite(t *testing.T) {
	ts := newFixtureServer(t)

	doc := getAsFixtureUser(t, ts, "/workflows")
	if n := doc.Find(`[data-testid="wf-new-link"]`).Length(); n != 1 {
		t.Errorf("writer's catalog: New Workflow CTA count = %d, want 1", n)
	}

	// Forwarded-only identity: read-only by construction (#427 anti-spoof).
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/workflows", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("X-Forwarded-User", "reader")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /workflows: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read-only catalog: status %d, want 200", resp.StatusCode)
	}
	rdoc, err := parseHTML(resp.Body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := rdoc.Find(`[data-testid="wf-new-link"]`).Length(); n != 0 {
		t.Errorf("read-only identity sees the New Workflow CTA %d time(s) — it 403s on submit; the button must not offer it", n)
	}
}
