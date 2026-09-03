package ui

// The owner label is a single point of failure for UI visibility: it has
// churned historically (tibrezus → tibrez) and automation-written CRs carry
// no owner at all. Admin groups see across owner labels (#324); everyone
// else keeps the strictly-scoped view. The privilege gate requires an
// Authentik-authoritative identity — the X-Forwarded-* fallback headers are
// client-suppliable and never grant the bypass.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func adminTestServer(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	s := newAttemptTestServer(t, objs...)
	s.SetAdminGroups([]string{"harmostes-admins"})
	return s
}

func attemptOwned(name, owner string) *v1alpha1.Attempt {
	labels := map[string]string{}
	if owner != "" {
		labels[v1alpha1.OwnerLabel] = owner
	}
	return &v1alpha1.Attempt{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "test-ns", Labels: labels,
		},
		Spec: v1alpha1.AttemptSpec{WorkflowRef: "wf"},
	}
}

func idWith(name string, groups ...string) *Identity {
	return &Identity{Username: name, Groups: groups, Authoritative: true}
}

// forwardedIdentity simulates an identity that arrived only via the legacy
// X-Forwarded-* fallback headers — client-suppliable, never privileged.
func forwardedIdentity(name string, groups ...string) *Identity {
	return &Identity{Username: name, Groups: groups}
}

func workflowOwned(name, owner string) *v1alpha1.Workflow {
	labels := map[string]string{v1alpha1.WorkflowLabel: name}
	if owner != "" {
		labels[v1alpha1.OwnerLabel] = owner
	}
	return &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns", Labels: labels},
	}
}

func TestParseAdminGroups(t *testing.T) {
	if got := ParseAdminGroups(""); got != nil {
		t.Errorf("empty env → %v, want nil", got)
	}
	got := ParseAdminGroups(" harmostes-admins , ops ,, ")
	if len(got) != 2 || got[0] != "harmostes-admins" || got[1] != "ops" {
		t.Errorf("ParseAdminGroups = %v, want [harmostes-admins ops]", got)
	}
}

func TestVisibleOwner(t *testing.T) {
	s := adminTestServer(t)

	admin := idWith("tib", "harmostes-admins", "users")
	if got := s.visibleOwner(admin); got != "" {
		t.Errorf("admin visibleOwner = %q, want unrestricted \"\"", got)
	}
	regular := idWith("tibrez", "users")
	if got := s.visibleOwner(regular); got != "tibrez" {
		t.Errorf("regular visibleOwner = %q, want \"tibrez\"", got)
	}
	// Fail-closed: a missing identity must never widen the list.
	if got := s.visibleOwner(nil); got == "" {
		t.Error("nil identity visibleOwner must not be unrestricted")
	}
	// A forwarded-only identity never holds the bypass, even with the group.
	spoof := forwardedIdentity("tib", "harmostes-admins")
	if got := s.visibleOwner(spoof); got != "tib" {
		t.Errorf("forwarded-only identity visibleOwner = %q, want scoped \"tib\"", got)
	}
	// A server without admin groups configured keeps everyone scoped.
	plain := &Server{}
	if got := plain.visibleOwner(admin); got != "tib" {
		t.Errorf("no-admin-config visibleOwner = %q, want \"tib\"", got)
	}
}

func TestAttemptList_AdminSeesAcrossOwnerLabels(t *testing.T) {
	s := adminTestServer(t,
		attemptOwned("attempt-alice", "alice"),
		attemptOwned("attempt-bob", "bob"),
		attemptOwned("attempt-ownerless", ""), // automation-written CRs carry no owner
	)

	req := httptest.NewRequest(http.MethodGet, "/runs?window=all", nil).WithContext(
		withIdentity(context.Background(), idWith("tib", "harmostes-admins")))
	atts, err := s.listAttempts(req, s.visibleOwner(idWith("tib", "harmostes-admins")))
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(atts) != 3 {
		t.Errorf("admin sees %d attempts, want 3 (cross-owner incl. ownerless)", len(atts))
	}

	req2 := httptest.NewRequest(http.MethodGet, "/runs", nil).WithContext(
		withIdentity(context.Background(), idWith("alice", "users")))
	atts, err = s.listAttempts(req2, s.visibleOwner(idWith("alice", "users")))
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(atts) != 1 {
		t.Errorf("regular user sees %d attempts, want 1 (scoped as before)", len(atts))
	}
}

// Handler-layer proof (the shipping path): an admin rendering /runs sees
// attempts across owner labels; a regular identity sees only their own.
func TestHandleAttemptList_AdminHandlerView(t *testing.T) {
	s := adminTestServer(t,
		attemptOwned("attempt-alice", "alice"),
		attemptOwned("attempt-bob", "bob"),
	)

	req := httptest.NewRequest(http.MethodGet, "/runs?window=all", nil).WithContext(
		withIdentity(context.Background(), idWith("tib", "harmostes-admins")))
	rec := httptest.NewRecorder()
	s.handleAttemptList(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"attempt-alice", "attempt-bob"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin /runs missing %q", want)
		}
	}

	req2 := httptest.NewRequest(http.MethodGet, "/runs?window=all", nil).WithContext(
		withIdentity(context.Background(), idWith("bob", "users")))
	rec2 := httptest.NewRecorder()
	s.handleAttemptList(rec2, req2)
	if strings.Contains(rec2.Body.String(), "attempt-alice") {
		t.Error("regular identity must not see another owner's attempts")
	}
	if !strings.Contains(rec2.Body.String(), "attempt-bob") {
		t.Error("regular identity must see their own attempts")
	}
}

func TestMayViewAttempt(t *testing.T) {
	s := adminTestServer(t)
	admin := idWith("tib", "harmostes-admins")
	other := idWith("mallory", "users")
	ownerless := attemptOwned("attempt-ownerless", "")

	if !s.mayViewAttempt(ownerless, admin) {
		t.Error("admin must view ownerless attempts")
	}
	if s.mayViewAttempt(ownerless, other) {
		t.Error("non-admin must NOT view ownerless attempts")
	}
	if s.mayViewAttempt(ownerless, nil) {
		t.Error("nil identity must not view attempts (fail-closed)")
	}
	owned := attemptOwned("attempt-alice", "alice")
	if !s.mayViewAttempt(owned, idWith("alice")) {
		t.Error("owner must view own attempt")
	}
	if s.mayViewAttempt(nil, admin) {
		t.Error("nil attempt must not be viewable")
	}
}

// Unmanaged (ownerless, GitOps-created) system workflows are invisible to
// the self-service UI but deliberately visible to admins (#324): system
// workflows are exactly what an operator needs on an incident.
func TestHandleWorkflowDetail_AdminSeesUnmanaged(t *testing.T) {
	s := adminTestServer(t, workflowOwned("fork-maintenance-forgejo", ""))

	req := httptest.NewRequest(http.MethodGet, "/workflows/fork-maintenance-forgejo", nil).
		WithContext(withIdentity(context.Background(), idWith("tib", "harmostes-admins")))
	req.SetPathValue("name", "fork-maintenance-forgejo")
	rec := httptest.NewRecorder()
	s.handleWorkflowDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("admin workflow-detail status = %d, want 200", rec.Code)
	}

	// Regular identities still get 404 for unmanaged workflows.
	s2 := adminTestServer(t, workflowOwned("fork-maintenance-forgejo", ""))
	req2 := httptest.NewRequest(http.MethodGet, "/workflows/fork-maintenance-forgejo", nil).
		WithContext(withIdentity(context.Background(), idWith("alice", "users")))
	req2.SetPathValue("name", "fork-maintenance-forgejo")
	rec2 := httptest.NewRecorder()
	s2.handleWorkflowDetail(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("regular workflow-detail status = %d, want 404", rec2.Code)
	}
}

func TestAuthMiddleware_401ShapeFollowsAccept(t *testing.T) {
	s := newAttemptTestServer(t)
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run without identity")
	}))

	t.Run("browser navigation gets HTML with re-auth link", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/runs?x=1&y=2", nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("content-type = %q, want text/html", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"<html", "Sign in again", "rd=%2Fruns%3Fx%3D1%26y%3D2"} {
			if !strings.Contains(body, want) {
				t.Errorf("401 page missing %q in body:\n%s", want, body)
			}
		}
		// The rd value must be query-escaped INSIDE the attribute: no raw
		// & that would invent a second attribute.
		if strings.Contains(body, `rd=/runs`) || strings.Contains(body, `&y=`) {
			t.Errorf("rd parameter not escaped: %s", body)
		}
	})

	t.Run("api consumers keep plain text", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/wall/events", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("content-type = %q, want text/plain", ct)
		}
	})
}
