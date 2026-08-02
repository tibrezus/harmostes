package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// TokenLabel marks a Secret as a per-user git token managed by harmostes-ui.
const TokenLabel = "harmostes.dev/token"

// TokenDataKey is the Secret data key that holds the actual token value.
const TokenDataKey = "token"

// validTokenName restricts secret names to safe DNS-compatible identifiers.
var validTokenName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// All supported platforms, in display order.
var allPlatforms = []platformInfo{
	{"github", "GitHub", "#24292e"},
	{"gitlab", "GitLab", "#fc6d26"},
	{"forgejo", "Forgejo", "#ff6600"},
	{"codeberg", "Codeberg", "#2185d0"},
}

type platformInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Color string `json:"color"`
}

// tokenSecretName generates a collision-resistant name: <owner>-<platform>-<rand>.
func tokenSecretName(owner, platform string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", owner, platform, hex.EncodeToString(b))
}

// maskToken produces a safe preview of the token value: first 4 + … + last 4.
// This lets users identify which token is which without revealing the full value.
// Tokens shorter than 12 chars are fully masked.
func maskToken(value string) string {
	if len(value) <= 12 {
		return strings.Repeat("•", len(value))
	}
	return value[:4] + "…" + value[len(value)-4:]
}

// tokenMeta is the display metadata for a token. The actual VALUE is never
// returned to the browser — only a masked preview.
type tokenMeta struct {
	Name      string   `json:"name"`
	Platform  string   `json:"platform"`
	MaskedVal string   `json:"maskedValue"`
	CreatedAt string   `json:"createdAt"`
	Workflows []string `json:"workflows,omitempty"`
}

// platformStatus represents the state of a platform's tokens for the UI.
type platformStatus struct {
	Platform platformInfo `json:"platform"`
	Tokens   []tokenMeta  `json:"tokens"`
	HasToken bool         `json:"hasToken"`
}

// handleTokenList renders the user's git tokens page.
func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	platforms, err := s.buildTokenStatus(r, owner)
	if err != nil {
		s.logger.Error("list tokens", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load tokens: "+err.Error())
		return
	}
	s.render(w, r, "pages/tokens.html", map[string]any{
		"Platforms": platforms,
	})
}

// handleTokenAPIList returns the user's token metadata as JSON (no raw values).
// Route: GET /api/tokens
func (s *Server) handleTokenAPIList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	platforms, err := s.buildTokenStatus(r, owner)
	if err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "list tokens: %v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"platforms": platforms,
	})
}

// buildTokenStatus builds the full platform-grouped view: for each supported
// platform, lists the user's tokens (with masked previews + workflow linkage).
func (s *Server) buildTokenStatus(r *http.Request, owner string) ([]platformStatus, error) {
	tokens, err := s.listTokens(r, owner)
	if err != nil {
		return nil, err
	}

	// Group tokens by platform.
	byPlatform := map[string][]tokenMeta{}
	for _, t := range tokens {
		byPlatform[t.Platform] = append(byPlatform[t.Platform], t)
	}

	// Build the platform list in display order (all platforms, even those
	// without tokens, so the UI can show "Not configured" states).
	result := make([]platformStatus, 0, len(allPlatforms))
	for _, p := range allPlatforms {
		toks := byPlatform[p.ID]
		sort.Slice(toks, func(i, j int) bool { return toks[i].CreatedAt > toks[j].CreatedAt })
		result = append(result, platformStatus{
			Platform: p,
			Tokens:   toks,
			HasToken: len(toks) > 0,
		})
	}
	return result, nil
}

// handleTokenCreate creates a new per-user git token Secret.
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

// handleTokenDelete removes a per-user git token Secret.
func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !validTokenName.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	var secret corev1.Secret
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &secret); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.renderError(w, r, "Failed to verify token: "+err.Error())
		return
	}

	if secret.Labels[v1alpha1.OwnerLabel] != owner || secret.Labels[TokenLabel] == "" {
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

// listTokens returns all token Secrets owned by the given user, enriched with
// masked previews and workflow linkage. Token VALUES are never returned in full.
func (s *Server) listTokens(r *http.Request, owner string) ([]tokenMeta, error) {
	var secrets corev1.SecretList
	opts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.OwnerLabel: owner},
		client.HasLabels{TokenLabel},
	}
	if err := s.k8sClient.List(r.Context(), &secrets, opts...); err != nil {
		return nil, fmt.Errorf("list token secrets: %w", err)
	}

	// Build a map of secret-name → workflows that reference it.
	tokenRefs, err := s.tokenWorkflowRefs(r.Context(), owner)
	if err != nil {
		tokenRefs = map[string][]string{} // best-effort
	}

	result := make([]tokenMeta, 0, len(secrets.Items))
	for _, sec := range secrets.Items {
		platform := sec.Labels[TokenLabel]
		masked := ""
		if val, ok := sec.Data[TokenDataKey]; ok && len(val) > 0 {
			masked = maskToken(string(val))
		}
		result = append(result, tokenMeta{
			Name:      sec.Name,
			Platform:  platform,
			MaskedVal: masked,
			CreatedAt: sec.CreationTimestamp.Format("2006-01-02 15:04"),
			Workflows: tokenRefs[sec.Name],
		})
	}
	return result, nil
}

// tokenWorkflowRefs scans the user's workflows and builds a map of
// secret-name → workflow names that reference that token via
// workspaceRepo.tokenRef.name.
func (s *Server) tokenWorkflowRefs(ctx context.Context, owner string) (map[string][]string, error) {
	var wfs v1alpha1.WorkflowList
	opts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.OwnerLabel: owner},
	}
	if err := s.k8sClient.List(ctx, &wfs, opts...); err != nil {
		return nil, err
	}
	refs := map[string][]string{}
	for _, wf := range wfs.Items {
		if wf.Spec.WorkspaceRepo != nil && wf.Spec.WorkspaceRepo.TokenRef != nil {
			name := wf.Spec.WorkspaceRepo.TokenRef.Name
			refs[name] = append(refs[name], wf.Name)
		}
	}
	return refs, nil
}

// isValidPlatform checks whether a platform identifier is in the allowed set.
func isValidPlatform(p string) bool {
	for _, pl := range allPlatforms {
		if pl.ID == p {
			return true
		}
	}
	return false
}
