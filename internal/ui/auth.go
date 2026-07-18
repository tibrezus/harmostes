package ui

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const identityKey contextKey = "identity"

// Identity represents the authenticated user extracted from Authentik
// forward-auth headers.
type Identity struct {
	Username string
	Email    string
	Groups   []string
}

// authMiddleware extracts the user identity from Authentik forward-auth headers.
// Authentik's proxy provider injects these headers on every authenticated
// request:
//
//	X-Forwarded-Preferred-Username  — the user's username (primary identity)
//	X-Forwarded-Email               — the user's email
//	X-Forwarded-Groups              — comma-separated group list
//
// If no identity header is present, the request is rejected with 401. In
// development (without Authentik), set HARMOSTES_DEV_USER to bypass.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := extractIdentity(r)
		if id == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("401 Unauthorized — no Authentik identity headers\n"))
			return
		}
		ctx := context.WithValue(r.Context(), identityKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractIdentity reads Authentik forward-auth headers, or falls back to
// HARMOSTES_DEV_USER for local development. Returns nil if no identity found.
func extractIdentity(r *http.Request) *Identity {
	// Authentik forward-auth headers
	username := r.Header.Get("X-Forwarded-Preferred-Username")
	if username == "" {
		username = r.Header.Get("X-Forwarded-User")
	}

	// Development override (no Authentik in local dev)
	if username == "" {
		if devUser := r.Header.Get("X-Harmostes-Dev-User"); devUser != "" {
			return &Identity{Username: devUser}
		}
		return nil
	}

	email := r.Header.Get("X-Forwarded-Email")
	groups := []string{}
	if g := r.Header.Get("X-Forwarded-Groups"); g != "" {
		for _, grp := range strings.Split(g, ",") {
			grp = strings.TrimSpace(grp)
			if grp != "" {
				groups = append(groups, grp)
			}
		}
	}

	return &Identity{
		Username: username,
		Email:    email,
		Groups:   groups,
	}
}

// identityFromContext returns the Identity from the request context, or nil.
func identityFromContext(ctx context.Context) *Identity {
	v := ctx.Value(identityKey)
	if v == nil {
		return nil
	}
	id, _ := v.(*Identity)
	return id
}
