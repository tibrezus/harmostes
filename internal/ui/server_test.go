package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestExtractIdentity_AuthentikHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		wantUser string
		wantNil  bool
	}{
		{
			name:     "authentik username header (2026.x)",
			headers:  map[string]string{"X-Authentik-Username": "alice"},
			wantUser: "alice",
		},
		{
			name:     "preferred username header (legacy)",
			headers:  map[string]string{"X-Forwarded-Preferred-Username": "alice"},
			wantUser: "alice",
		},
		{
			name:     "forwarded user fallback",
			headers:  map[string]string{"X-Forwarded-User": "bob"},
			wantUser: "bob",
		},
		{
			name:     "dev user override",
			headers:  map[string]string{"X-Harmostes-Dev-User": "devuser"},
			wantUser: "devuser",
		},
		{
			name:    "no headers → nil",
			headers: map[string]string{},
			wantNil: true,
		},
		{
			name: "with email and groups (authentik pipe-separated)",
			headers: map[string]string{
				"X-Authentik-Username": "carol",
				"X-Authentik-Email":    "carol@example.com",
				"X-Authentik-Groups":   "admins|users",
			},
			wantUser: "carol",
		},
		{
			name: "with email and groups (legacy comma-separated)",
			headers: map[string]string{
				"X-Forwarded-Preferred-Username": "carol",
				"X-Forwarded-Email":              "carol@example.com",
				"X-Forwarded-Groups":             "admins, users",
			},
			wantUser: "carol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/workflows", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			id := extractIdentity(req)
			if tt.wantNil {
				if id != nil {
					t.Fatalf("expected nil identity, got %+v", id)
				}
				return
			}
			if id == nil {
				t.Fatal("expected non-nil identity, got nil")
			}
			if id.Username != tt.wantUser {
				t.Errorf("username = %q, want %q", id.Username, tt.wantUser)
			}
		})
	}
}

func TestExtractIdentity_Groups(t *testing.T) {
	// Authentik pipe-separated groups
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Authentik-Username", "dave")
	req.Header.Set("X-Authentik-Groups", "admins|developers|")

	id := extractIdentity(req)
	if id == nil {
		t.Fatal("expected identity")
	}
	if len(id.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(id.Groups), id.Groups)
	}
	if id.Groups[0] != "admins" || id.Groups[1] != "developers" {
		t.Errorf("groups = %v, want [admins developers]", id.Groups)
	}

	// Legacy comma-separated groups
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("X-Forwarded-Preferred-Username", "eve")
	req2.Header.Set("X-Forwarded-Groups", "admins, developers, ")

	id2 := extractIdentity(req2)
	if id2 == nil {
		t.Fatal("expected identity")
	}
	if len(id2.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d: %v", len(id2.Groups), id2.Groups)
	}
	if id2.Groups[0] != "admins" || id2.Groups[1] != "developers" {
		t.Errorf("groups = %v, want [admins developers]", id2.Groups)
	}
}

func TestAuthMiddleware_RejectsUnauthenticated(t *testing.T) {
	s := &Server{}
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called for unauthenticated request")
	}))

	req := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddleware_PassesAuthenticated(t *testing.T) {
	s := &Server{}
	called := false
	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		id := identityFromContext(r.Context())
		if id == nil || id.Username != "alice" {
			t.Errorf("identity = %+v, want username alice", id)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/workflows", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("handler was not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"green", "green"},
		{"failed", "red"},
		{"", "muted"},
		{"unknown", "muted"},
	}
	for _, tt := range tests {
		if got := statusClass(tt.input); got != tt.want {
			t.Errorf("statusClass(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseTemplates(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	if tmpl == nil {
		t.Fatal("template is nil")
	}
	// Verify the layout exists
	if tmpl.Lookup("layout.html") == nil {
		t.Error("layout.html template not found")
	}
	// Verify page templates exist
	for _, page := range []string{"pages/workflows.html", "pages/detail.html", "pages/error.html"} {
		if tmpl.Lookup(page) == nil {
			t.Errorf("template %q not found", page)
		}
	}
}

// TestSanitizeLabels_AntiSpoof is the Phase B anti-spoofing acceptance test:
// a client-supplied harmostes.dev/owner label is always stripped and replaced
// with the authenticated user's username. The server never trusts the
// client-supplied owner — a malicious user cannot create a workflow under
// another user's identity.
func TestSanitizeLabels_AntiSpoof(t *testing.T) {
	t.Run("strips spoofed owner and stamps authenticated user", func(t *testing.T) {
		input := map[string]string{
			v1alpha1.OwnerLabel:    "victim", // attacker tries to impersonate
			v1alpha1.WorkflowLabel: "llm-wiki",
			"env":                  "prod",
		}
		got := SanitizeLabels(input, "alice")
		if got[v1alpha1.OwnerLabel] != "alice" {
			t.Errorf("owner = %q, want alice (spoofed value must be replaced)", got[v1alpha1.OwnerLabel])
		}
		if _, ok := got[v1alpha1.WorkflowLabel]; ok {
			t.Error("workflow label should be stripped (controller-managed)")
		}
		if got["env"] != "prod" {
			t.Error("non-harmostes labels should be preserved")
		}
	})

	t.Run("nil map returns new map with owner", func(t *testing.T) {
		got := SanitizeLabels(nil, "alice")
		if got[v1alpha1.OwnerLabel] != "alice" {
			t.Errorf("owner = %q, want alice", got[v1alpha1.OwnerLabel])
		}
	})

	t.Run("empty owner omits label", func(t *testing.T) {
		input := map[string]string{v1alpha1.OwnerLabel: "someone"}
		got := SanitizeLabels(input, "")
		if _, ok := got[v1alpha1.OwnerLabel]; ok {
			t.Error("owner label should be absent when owner is empty")
		}
	})
}

// TestStampOwnerLabel verifies that StampOwnerLabel sets the owner on a
// Workflow CR, stripping any pre-existing value first (anti-spoof).
func TestStampOwnerLabel(t *testing.T) {
	t.Run("stamps owner on fresh CR", func(t *testing.T) {
		wf := &v1alpha1.Workflow{}
		StampOwnerLabel(wf, "alice")
		if wf.Labels[v1alpha1.OwnerLabel] != "alice" {
			t.Errorf("owner = %q, want alice", wf.Labels[v1alpha1.OwnerLabel])
		}
	})

	t.Run("replaces spoofed owner", func(t *testing.T) {
		wf := &v1alpha1.Workflow{}
		wf.Labels = map[string]string{v1alpha1.OwnerLabel: "victim"}
		StampOwnerLabel(wf, "alice")
		if wf.Labels[v1alpha1.OwnerLabel] != "alice" {
			t.Errorf("owner = %q, want alice (must replace spoofed value)", wf.Labels[v1alpha1.OwnerLabel])
		}
	})
}
