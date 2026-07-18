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
// Authentik's proxy outpost (2026.x) injects X-Authentik-* headers on every
// authenticated request:
//
//	X-Authentik-Username  — the user's username (primary identity)
//	X-Authentik-Email     — the user's email
//	X-Authentik-Groups    — pipe-separated group list
//
// Older forward-auth setups and oauth2-proxy use X-Forwarded-* variants:
//
//	X-Forwarded-Preferred-Username / X-Forwarded-User  — username
//	X-Forwarded-Email                                 — email
//	X-Forwarded-Groups                                — comma-separated groups
//
// Both sets are accepted. If no identity header is present, the request is
// rejected with 401. In development (without Authentik), use X-Harmostes-Dev-User.
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
	// Authentik proxy outpost headers (2026.x)
	username := r.Header.Get("X-Authentik-Username")
	email := r.Header.Get("X-Authentik-Email")
	groupsRaw := r.Header.Get("X-Authentik-Groups")
	groupSep := "|" // Authentik uses pipe separator

	// Fallback: standard forward-auth headers (oauth2-proxy, older Authentik)
	if username == "" {
		username = r.Header.Get("X-Forwarded-Preferred-Username")
	}
	if username == "" {
		username = r.Header.Get("X-Forwarded-User")
	}
	if email == "" {
		email = r.Header.Get("X-Forwarded-Email")
	}
	if groupsRaw == "" {
		groupsRaw = r.Header.Get("X-Forwarded-Groups")
		groupSep = "," // X-Forwarded-Groups uses comma
	}

	// Development override (no Authentik in local dev)
	if username == "" {
		if devUser := r.Header.Get("X-Harmostes-Dev-User"); devUser != "" {
			return &Identity{Username: devUser}
		}
		return nil
	}

	groups := []string{}
	if groupsRaw != "" {
		for _, grp := range strings.Split(groupsRaw, groupSep) {
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
