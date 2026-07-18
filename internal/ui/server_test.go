package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractIdentity_AuthentikHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		wantUser string
		wantNil  bool
	}{
		{
			name:     "preferred username header",
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
			name: "with email and groups",
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
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Preferred-Username", "dave")
	req.Header.Set("X-Forwarded-Groups", "admins, developers, ")

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
	req.Header.Set("X-Forwarded-Preferred-Username", "alice")
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
