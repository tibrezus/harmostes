package ui

// The owner label is a single point of failure for UI visibility: it has
// churned historically (tibrezus → tibrez) and automation-written CRs carry
// no owner at all. Admin groups see across owner labels (#324); everyone
// else keeps the strictly-scoped view.

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
	return &Identity{Username: name, Groups: groups}
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

	req := httptest.NewRequest(http.MethodGet, "/runs", nil).WithContext(
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
	owned := attemptOwned("attempt-alice", "alice")
	if !s.mayViewAttempt(owned, idWith("alice")) {
		t.Error("owner must view own attempt")
	}
	if s.mayViewAttempt(nil, admin) {
		t.Error("nil attempt must not be viewable")
	}
}

func TestAuthMiddleware_401ShapeFollowsAccept(t *testing.T) {
	s := newAttemptTestServer(t)
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run without identity")
	}))

	t.Run("browser navigation gets HTML with re-auth link", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/runs", nil)
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
		for _, want := range []string{"<html", "Sign in again", "/outpost.goauthentik.io/start?rd=%2Fruns"} {
			if !strings.Contains(body, want) {
				t.Errorf("401 page missing %q in body:\n%s", want, body)
			}
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
