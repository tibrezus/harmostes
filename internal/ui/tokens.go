package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// TokenLabel marks a Secret as a per-user git token managed by harmostes-ui.
// Used to distinguish token secrets from other secrets in the namespace.
const TokenLabel = "harmostes.dev/token"

// TokenDataKey is the Secret data key that holds the actual token value.
const TokenDataKey = "token"

// validTokenName restricts secret names to safe DNS-compatible identifiers
// with an owner prefix to prevent collisions and path traversal.
var validTokenName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// tokenSecretName generates a collision-resistant name: <owner>-<platform>-<rand>.
func tokenSecretName(owner, platform string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", owner, platform, hex.EncodeToString(b))
}

// tokenFromSecret extracts the display metadata from a k8s Secret.
// The actual token VALUE is never returned to the browser — only the name,
// platform, and creation timestamp.
type tokenMeta struct {
	Name      string
	Platform  string
	CreatedAt string
}

func tokenFromSecret(s corev1.Secret) tokenMeta {
	return tokenMeta{
		Name:      s.Name,
		Platform:  s.Labels[TokenLabel],
		CreatedAt: s.CreationTimestamp.Format("2006-01-02 15:04"),
	}
}

// handleTokenList renders the user's git tokens page.
func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	tokens, err := s.listTokens(r, owner)
	if err != nil {
		s.logger.Error("list tokens", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load tokens: "+err.Error())
		return
	}
	s.render(w, r, "pages/tokens.html", map[string]any{
		"Tokens": tokens,
	})
}

// handleTokenCreate creates a new per-user git token Secret.
// Form fields: platform (github|gitlab|forgejo), token (the actual value).
// The secret is labeled with the authenticated user's owner label — the user
// cannot set this themselves (anti-spoof via SanitizeLabels).
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username

	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form data")
		return
	}

	platform := strings.ToLower(strings.TrimSpace(r.FormValue("platform")))
	token := strings.TrimSpace(r.FormValue("token"))

	if token == "" {
		s.renderError(w, r, "Token value is required")
		return
	}
	if !isValidPlatform(platform) {
		s.renderError(w, r, "Invalid platform: "+platform)
		return
	}

	name := tokenSecretName(owner, platform)
	labels := SanitizeLabels(map[string]string{}, owner)
	labels[TokenLabel] = platform

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			TokenDataKey: []byte(token),
		},
	}

	if err := s.k8sClient.Create(r.Context(), secret); err != nil {
		s.logger.Error("create token secret", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to create token: "+err.Error())
		return
	}

	s.logger.Info("token created", "owner", owner, "name", name, "platform", platform)
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

// handleTokenDelete removes a per-user git token Secret. It verifies the
// owner label matches the authenticated user before deleting — a user cannot
// delete another user's token even if they know the secret name.
func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !validTokenName.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	// Fetch the secret first to verify ownership.
	var secret corev1.Secret
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &secret); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.renderError(w, r, "Failed to verify token: "+err.Error())
		return
	}

	// Owner check: the secret MUST carry the authenticated user's owner label.
	if secret.Labels[v1alpha1.OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	// Also verify it's a token secret (not some other secret that happens to
	// have the owner label).
	if secret.Labels[TokenLabel] == "" {
		http.NotFound(w, r)
		return
	}

	if err := s.k8sClient.Delete(r.Context(), &secret); err != nil {
		s.logger.Error("delete token secret", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to delete token: "+err.Error())
		return
	}

	s.logger.Info("token deleted", "owner", owner, "name", name)
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

// listTokens returns all token Secrets owned by the given user.
// Token VALUES are never returned — only metadata.
func (s *Server) listTokens(r *http.Request, owner string) ([]tokenMeta, error) {
	var secrets corev1.SecretList
	// Require both the owner label and the token label. HasLabels ensures only
	// token secrets (not system secrets) are returned, even though the RBAC
	// grants broader access.
	opts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.OwnerLabel: owner},
		client.HasLabels{TokenLabel},
	}

	if err := s.k8sClient.List(r.Context(), &secrets, opts...); err != nil {
		return nil, fmt.Errorf("list token secrets: %w", err)
	}

	result := make([]tokenMeta, 0, len(secrets.Items))
	for _, sec := range secrets.Items {
		result = append(result, tokenFromSecret(sec))
	}
	return result, nil
}

// isValidPlatform checks whether a platform identifier is in the allowed set.
func isValidPlatform(p string) bool {
	switch p {
	case "github", "gitlab", "forgejo", "codeberg":
		return true
	default:
		return false
	}
}
