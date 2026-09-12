package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// lifecycleWorld: one owned workflow per shape the guard chain distinguishes
// — the operator's own, another user's (foreign), a paused one, an armed
// one — plus the template catalog the creation tests use.
func lifecycleWorld() []client.Object {
	owned := &v1alpha1.Workflow{}
	owned.Name = "owned-armed"
	owned.Namespace = "harmostes"
	owned.Labels = map[string]string{v1alpha1.OwnerLabel: "alice"}
	owned.Spec.TemplateRef = "pr-review"
	owned.Spec.Source.Kind = "schedule"

	foreign := owned.DeepCopy()
	foreign.Name = "foreign"
	foreign.Labels = map[string]string{v1alpha1.OwnerLabel: "mallory"}

	paused := owned.DeepCopy()
	paused.Name = "owned-paused"
	paused.Spec.Disabled = true

	return []client.Object{owned, foreign, paused, prReviewTemplate()}
}

// postAs drives a lifecycle route with the given identity headers.
func postAs(s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func aliceHeaders() map[string]string {
	return map[string]string{"X-Authentik-Username": "alice"}
}

// writesCount reads the counter for one action/result pair.
func writesCount(t *testing.T, action, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(writesTotal.WithLabelValues(action, result))
}

// TestWorkflowToggle pins the arm/pause pair: the verb flips spec.disabled
// on the CALLER'S OWN workflow only, is idempotent, and every outcome lands
// in the writes counter.
func TestWorkflowToggle(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	// Pause the armed one.
	rec := postAs(s, "/workflows/owned-armed/disable", aliceHeaders())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disable status = %d, want 303", rec.Code)
	}
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "owned-armed"}, &wf); err != nil {
		t.Fatalf("get after disable: %v", err)
	}
	if !wf.Spec.Disabled {
		t.Error("disable left spec.disabled false")
	}

	// Re-enable.
	rec = postAs(s, "/workflows/owned-armed/enable", aliceHeaders())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("enable status = %d, want 303", rec.Code)
	}
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "owned-armed"}, &wf); err != nil {
		t.Fatalf("get after enable: %v", err)
	}
	if wf.Spec.Disabled {
		t.Error("enable left spec.disabled true")
	}

	// Idempotent re-enable: 303, no error.
	rec = postAs(s, "/workflows/owned-armed/enable", aliceHeaders())
	if rec.Code != http.StatusSeeOther {
		t.Errorf("redundant enable status = %d, want 303", rec.Code)
	}

	// Counters: the two real flips. The idempotent re-enable is a no-op —
	// nothing changed, so it is not a write and stays uncounted.
	if got := writesCount(t, "disable", "created"); got != 1 {
		t.Errorf("disable/created = %v, want 1", got)
	}
	if got := writesCount(t, "enable", "created"); got != 1 {
		t.Errorf("enable/created = %v, want 1", got)
	}
}

// TestWorkflowLifecycle_ForeignOwnerIs404 pins the ownership guard on
// writes: mallory's workflow is invisible to alice — and a write-verb URL
// must not become an oracle for its existence. Foreign and missing objects
// answer identically.
func TestWorkflowLifecycle_ForeignOwnerIs404(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	for _, action := range []string{"enable", "disable", "trigger", "delete"} {
		for _, name := range []string{"foreign", "does-not-exist"} {
			rec := postAs(s, "/workflows/"+name+"/"+action, aliceHeaders())
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404 (existence not leaked)", action, name, rec.Code)
			}
		}
	}

	// The foreign workflow survived every attempt.
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "foreign"}, &wf); err != nil {
		t.Fatalf("foreign workflow was mutated: %v", err)
	}
	// Every rejection counted as forbidden (the decision, not the wire code).
	if got := writesCount(t, "delete", "forbidden"); got != 1 {
		t.Errorf("delete/forbidden = %v, want 1 (foreign)", got)
	}
	if got := writesCount(t, "delete", "error"); got != 1 {
		t.Errorf("delete/error = %v, want 1 (missing)", got)
	}
}

// TestWorkflowLifecycle_ReadOnlyIdentity pins gate order: the write gate
// fires BEFORE object lookup — a read-only identity cannot even probe names.
func TestWorkflowLifecycle_ReadOnlyIdentity(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	// Forwarded-only identity: may browse, never write (#427 anti-spoof).
	for _, action := range []string{"enable", "disable", "trigger", "delete"} {
		rec := postAs(s, "/workflows/owned-armed/"+action, map[string]string{"X-Forwarded-User": "mallory"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s as forwarded-only: status = %d, want 403", action, rec.Code)
		}
	}
	// Dev identity without server opt-in: same wall.
	rec := postAs(s, "/workflows/owned-armed/delete", map[string]string{"X-Harmostes-Dev-User": "devuser"})
	if rec.Code != http.StatusForbidden {
		t.Errorf("dev identity without opt-in: status = %d, want 403", rec.Code)
	}
}

// TestWorkflowTrigger pins the wake contract end to end: an armed workflow
// gets the manual-prefixed trigger-revision annotation (the controller's
// documented wake), a paused one refuses with 409 — an annotation left on a
// Disabled object would fire a deferred run the moment someone later arms
// it — and an unknown trigger is a 404, not a silent annotation write.
func TestWorkflowTrigger(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	rec := postAs(s, "/workflows/owned-armed/trigger", aliceHeaders())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("trigger status = %d, want 303", rec.Code)
	}
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "owned-armed"}, &wf); err != nil {
		t.Fatalf("get after trigger: %v", err)
	}
	rev, ok := wf.Annotations[v1alpha1.TriggerRevisionAnnotation]
	if !ok {
		t.Fatal("trigger left no wake annotation")
	}
	if !strings.HasPrefix(rev, v1alpha1.ManualTriggerPrefix) {
		t.Errorf("wake value = %q, want the %q prefix (controller records triggerType manual)", rev, v1alpha1.ManualTriggerPrefix)
	}

	// Paused refuses.
	rec = postAs(s, "/workflows/owned-paused/trigger", aliceHeaders())
	if rec.Code != http.StatusConflict {
		t.Errorf("trigger on paused status = %d, want 409", rec.Code)
	}
	var paused v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "owned-paused"}, &paused); err != nil {
		t.Fatalf("get paused: %v", err)
	}
	if _, live := paused.Annotations[v1alpha1.TriggerRevisionAnnotation]; live {
		t.Error("refused trigger left a wake annotation — a deferred run nobody asked for")
	}

	// Admins may trigger ownerless system workflows (#324 exception — the
	// same one the detail page grants): stamp an ownerless workflow and let
	// an admin group member at it.
	system := &v1alpha1.Workflow{}
	system.Name = "system-fork-sync"
	system.Namespace = "harmostes"
	if err := s.k8sClient.Create(context.Background(), system); err != nil {
		t.Fatalf("seed system workflow: %v", err)
	}
	admin := func(s *Server, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("X-Authentik-Username", "root")
		req.Header.Set("X-Authentik-Groups", "harmostes-admins")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}
	s.SetAdminGroups([]string{"harmostes-admins"})
	if rec := admin(s, "/workflows/system-fork-sync/trigger"); rec.Code != http.StatusSeeOther {
		t.Errorf("admin trigger on ownerless system workflow: status = %d, want 303", rec.Code)
	}
}

// TestWorkflowDelete pins removal: the owner's instance is gone after the
// POST; the foreign one is not (guard proven above) — and the redirect
// lands on the catalog, whose owner filter simply no longer sees it.
func TestWorkflowDelete(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	rec := postAs(s, "/workflows/owned-armed/delete", aliceHeaders())
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/workflows" {
		t.Errorf("delete redirect = %q, want /workflows", loc)
	}
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "owned-armed"}, &wf); err == nil {
		t.Error("owned-armed survived delete")
	}
}

// TestCreate_SourceKindPinnedToSelfService pins the cadence contract: the
// form offers schedule (default) and webhook; anything else is a 400, and a
// webhook-created instance stores kind webhook — while staying disabled
// (arming is still a separate act).
func TestCreate_SourceKindPinnedToSelfService(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/workflows", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range aliceHeaders() {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec
	}

	// Default (no sourceKind): schedule.
	rec := post("name=wf-sched&templateRef=pr-review")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create default cadence: status = %d, want 303; body %s", rec.Code, rec.Body.String())
	}
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "wf-sched"}, &wf); err != nil {
		t.Fatalf("created CR not found: %v", err)
	}
	if wf.Spec.Source.Kind != "schedule" {
		t.Errorf("default source.kind = %q, want schedule", wf.Spec.Source.Kind)
	}

	// Webhook accepted.
	rec = post("name=wf-hook&templateRef=pr-review&sourceKind=webhook")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create webhook cadence: status = %d", rec.Code)
	}
	if err := s.k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "wf-hook"}, &wf); err != nil {
		t.Fatalf("created CR not found: %v", err)
	}
	if wf.Spec.Source.Kind != "webhook" || !wf.Spec.Disabled {
		t.Errorf("webhook instance = kind %q disabled %v, want webhook + true", wf.Spec.Source.Kind, wf.Spec.Disabled)
	}

	// Everything else rejected — the operator dialects (git/event/cron) are
	// not the form's to pretend.
	rec = post("name=wf-cron&templateRef=pr-review&sourceKind=event")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("create event cadence: status = %d, want 400", rec.Code)
	}
	if got := writesCount(t, "create", "error"); got < 1 {
		t.Errorf("create/error = %v, want ≥1", got)
	}
}

// TestWorkflowCreationForm_ReadOnlyRedirect: a read-only identity that
// deep-links to the form gets the catalog, not a form that can only 403.
func TestWorkflowCreationForm_ReadOnlyRedirect(t *testing.T) {
	s := workflowTestServer(lifecycleWorld()...)

	req := httptest.NewRequest(http.MethodGet, "/workflows/new", nil)
	req.Header.Set("X-Forwarded-User", "mallory")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("GET /workflows/new as read-only: status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/workflows" {
		t.Errorf("redirect = %q, want /workflows", loc)
	}
}

// TestWritesCounterVectors pins the counter's vocabulary exactly (#436):
// every mutating action reports one of created|forbidden|error — a typo'd
// label value would silently fork the series a dashboard reads.
func TestWritesCounterVectors(t *testing.T) {
	for _, action := range []string{"create", "enable", "disable", "trigger", "delete"} {
		for _, result := range []string{"created", "forbidden", "error"} {
			before := writesCount(t, action, result)
			recordWrite(action, result)
			after := writesCount(t, action, result)
			if fmt.Sprintf("%.0f", after-before) != "1" {
				t.Errorf("recordWrite(%s, %s) moved the counter by %v, want 1", action, result, after-before)
			}
		}
	}
}
