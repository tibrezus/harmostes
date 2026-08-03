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

var validTokenName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// tokenSecretName generates a collision-resistant name: <owner>-<platform>-<rand>.
func tokenSecretName(owner, platform string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", owner, platform, hex.EncodeToString(b))
}

// maskToken produces a safe preview: first 4 + … + last 4 chars.
func maskToken(value string) string {
	if len(value) <= 12 {
		return strings.Repeat("•", len(value))
	}
	return value[:4] + "…" + value[len(value)-4:]
}

// tokenMeta is the display metadata for a token. The VALUE is never returned
// in full — only a masked preview.
type tokenMeta struct {
	Name      string   `json:"name"`
	Platform  string   `json:"platform"`
	MaskedVal string   `json:"maskedValue"`
	CreatedAt string   `json:"createdAt"`
	Workflows []string `json:"workflows,omitempty"`
}

// platformStatus represents one platform's token state for the UI.
type platformStatus struct {
	ID       string      `json:"id"`
	Label    string      `json:"label"`
	Color    string      `json:"color"`
	Icon     string      `json:"icon,omitempty"`
	HasToken bool        `json:"hasToken"`
	Tokens   []tokenMeta `json:"tokens,omitempty"`
}

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

// handleTokenAPIList returns token metadata as JSON (no raw values).
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

// buildTokenStatus builds the platform-grouped view: all known platforms
// (from config) + any platforms discovered from existing tokens, each with
// its tokens (masked previews + workflow linkage).
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

	// Discover platform IDs from existing tokens (for platforms not in config).
	discovered := make([]string, 0, len(byPlatform))
	for id := range byPlatform {
		discovered = append(discovered, id)
	}

	// Merge known config + discovered platforms.
	catalog := s.platforms.mergeDiscovered(discovered)

	result := make([]platformStatus, 0, len(catalog))
	for _, pc := range catalog {
		toks := byPlatform[pc.ID]
		sort.Slice(toks, func(i, j int) bool { return toks[i].CreatedAt > toks[j].CreatedAt })
		result = append(result, platformStatus{
			ID:       pc.ID,
			Label:    pc.Label,
			Color:    pc.Color,
			Icon:     pc.Icon,
			HasToken: len(toks) > 0,
			Tokens:   toks,
		})
	}
	return result, nil
}

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
	// Any DNS-safe lowercase string is a valid platform — NOT a fixed enum.
	if !isValidPlatformID(platform) {
		s.renderError(w, r, "Invalid platform name: must be lowercase alphanumeric with hyphens, max 63 characters")
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

// listTokens returns all token Secrets owned by the user, enriched with masked
// previews and workflow linkage. Token VALUES are never returned in full.
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

	tokenRefs, err := s.tokenWorkflowRefs(r.Context(), owner)
	if err != nil {
		tokenRefs = map[string][]string{}
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

// tokenWorkflowRefs scans workflows for tokenRef.name references.
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
