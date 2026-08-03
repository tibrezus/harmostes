package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// This file implements platform credential management using the cluster's
// native patterns:
//
//   - ExternalSecrets Operator (ESO): credentials synced from Bitwarden Secrets
//     Manager via ExternalSecret CRs. These are the "managed" credentials —
//     centrally rotated, audited via BSM, synced by ESO.
//   - Direct k8s Secrets: credentials created directly via the UI (personal
//     tokens not in BSM). These are the "direct" credentials.
//   - Dapr secret store: the read abstraction layer. Both ESO-synced and direct
//     Secrets are readable via GET /v1.0/secrets/{store}/{key}.
//
// The UI lists ALL credentials (both sources) and lets users create either type.

const (
	TokenLabel    = "harmostes.dev/token"
	TokenDataKey  = "token"
	SourceLabel   = "harmostes.dev/source" // "bitwarden" or "direct"
	CredKeyEDelim = ":"
)

var (
	validTokenName    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	externalSecretGVK = schema.GroupVersionKind{
		Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret",
	}
)

// credentialMeta is the unified display metadata for a credential, regardless
// of whether it's ESO-managed or a direct k8s Secret.
type credentialMeta struct {
	Name       string   `json:"name"`
	Platform   string   `json:"platform"`
	Source     string   `json:"source"`     // "bitwarden" (ESO) or "direct" (k8s Secret)
	SyncStatus string   `json:"syncStatus"` // for ESO: "Synced" / "Error" / "Pending"; for direct: "—"
	MaskedVal  string   `json:"maskedValue"`
	CreatedAt  string   `json:"createdAt"`
	Workflows  []string `json:"workflows,omitempty"`
}

// platformStatus groups credentials by platform for display.
type platformStatus struct {
	ID       string           `json:"id"`
	Label    string           `json:"label"`
	Color    string           `json:"color"`
	Icon     string           `json:"icon,omitempty"`
	HasCreds bool             `json:"hasCreds"`
	Creds    []credentialMeta `json:"creds,omitempty"`
}

func tokenSecretName(owner, platform string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", owner, platform, hex.EncodeToString(b))
}

func maskToken(value string) string {
	if len(value) <= 12 {
		return strings.Repeat("•", len(value))
	}
	return value[:4] + "…" + value[len(value)-4:]
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	platforms, err := s.buildCredentialStatus(r, owner)
	if err != nil {
		s.logger.Error("list credentials", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load credentials: "+err.Error())
		return
	}
	s.render(w, r, "pages/tokens.html", map[string]any{
		"Platforms": platforms,
	})
}

func (s *Server) handleTokenAPIList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	platforms, err := s.buildCredentialStatus(r, owner)
	if err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "list credentials: %v", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"platforms": platforms})
}

// handleTokenCreate handles two creation paths:
//   - source=bitwarden: creates an ExternalSecret CR referencing a BSM secret ID.
//   - source=direct (default): creates a direct k8s Secret with the token value.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username

	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form data")
		return
	}

	platform := strings.ToLower(strings.TrimSpace(r.FormValue("platform")))
	source := strings.ToLower(strings.TrimSpace(r.FormValue("source")))
	bsmKey := strings.TrimSpace(r.FormValue("bsmKey"))
	token := strings.TrimSpace(r.FormValue("token"))

	if !isValidPlatformID(platform) {
		s.renderError(w, r, "Invalid platform name: must be lowercase alphanumeric with hyphens, max 63 characters")
		return
	}

	switch source {
	case "bitwarden":
		if bsmKey == "" {
			s.renderError(w, r, "Bitwarden secret ID is required for Bitwarden-backed credentials")
			return
		}
		if err := s.createExternalSecret(r.Context(), owner, platform, bsmKey); err != nil {
			s.renderError(w, r, "Failed to create ExternalSecret: "+err.Error())
			return
		}
	default:
		if token == "" {
			s.renderError(w, r, "Token value is required for direct credentials")
			return
		}
		if err := s.createDirectSecret(r.Context(), owner, platform, token); err != nil {
			s.renderError(w, r, "Failed to create token: "+err.Error())
			return
		}
	}

	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !validTokenName.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	// Try deleting as a direct Secret first.
	var secret corev1.Secret
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &secret); err == nil {
		if secret.Labels[v1alpha1.OwnerLabel] != owner || secret.Labels[TokenLabel] == "" {
			http.NotFound(w, r)
			return
		}
		s.k8sClient.Delete(r.Context(), &secret)
		http.Redirect(w, r, "/tokens", http.StatusSeeOther)
		return
	}

	// Try deleting as an ExternalSecret.
	es := s.newExternalSecretObject(name)
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, es); err == nil {
		labels := es.GetLabels()
		if labels[v1alpha1.OwnerLabel] != owner {
			http.NotFound(w, r)
			return
		}
		s.k8sClient.Delete(r.Context(), es)
		http.Redirect(w, r, "/tokens", http.StatusSeeOther)
		return
	}

	http.NotFound(w, r)
}

// ---------------------------------------------------------------------------
// Core logic
// ---------------------------------------------------------------------------

// buildCredentialStatus builds the platform-grouped credential catalog from
// BOTH ExternalSecrets (BSM-managed) and direct k8s Secrets.
func (s *Server) buildCredentialStatus(r *http.Request, owner string) ([]platformStatus, error) {
	creds, err := s.listAllCredentials(r.Context(), owner)
	if err != nil {
		return nil, err
	}

	byPlatform := map[string][]credentialMeta{}
	for _, c := range creds {
		byPlatform[c.Platform] = append(byPlatform[c.Platform], c)
	}

	discovered := make([]string, 0, len(byPlatform))
	for id := range byPlatform {
		discovered = append(discovered, id)
	}
	catalog := s.platforms.mergeDiscovered(discovered)

	result := make([]platformStatus, 0, len(catalog))
	for _, pc := range catalog {
		platformCreds := byPlatform[pc.ID]
		sort.Slice(platformCreds, func(i, j int) bool { return platformCreds[i].CreatedAt > platformCreds[j].CreatedAt })
		result = append(result, platformStatus{
			ID:       pc.ID,
			Label:    pc.Label,
			Color:    pc.Color,
			Icon:     pc.Icon,
			HasCreds: len(platformCreds) > 0,
			Creds:    platformCreds,
		})
	}
	return result, nil
}

// listAllCredentials merges ExternalSecrets and direct Secrets into one list.
func (s *Server) listAllCredentials(ctx context.Context, owner string) ([]credentialMeta, error) {
	direct, err := s.listDirectSecrets(ctx, owner)
	if err != nil {
		return nil, err
	}
	managed, err := s.listExternalSecrets(ctx, owner)
	if err != nil {
		// ESO may not be installed — degrade gracefully.
		s.logger.Warn("list ExternalSecrets failed, showing direct only", "err", err)
		managed = nil
	}

	all := append(direct, managed...)

	// Enrich with workflow references.
	tokenRefs, _ := s.tokenWorkflowRefs(ctx, owner)
	for i := range all {
		all[i].Workflows = tokenRefs[all[i].Name]
	}
	return all, nil
}

// listDirectSecrets lists k8s Secrets created directly by the UI.
func (s *Server) listDirectSecrets(ctx context.Context, owner string) ([]credentialMeta, error) {
	var secrets corev1.SecretList
	opts := []client.ListOption{
		client.InNamespace(s.namespace),
		client.MatchingLabels{v1alpha1.OwnerLabel: owner},
		client.HasLabels{TokenLabel},
	}
	if err := s.k8sClient.List(ctx, &secrets, opts...); err != nil {
		return nil, fmt.Errorf("list direct secrets: %w", err)
	}

	result := make([]credentialMeta, 0, len(secrets.Items))
	for _, sec := range secrets.Items {
		masked := ""
		if val, ok := sec.Data[TokenDataKey]; ok && len(val) > 0 {
			masked = maskToken(string(val))
		}
		result = append(result, credentialMeta{
			Name:       sec.Name,
			Platform:   sec.Labels[TokenLabel],
			Source:     "direct",
			SyncStatus: "—",
			MaskedVal:  masked,
			CreatedAt:  sec.CreationTimestamp.Format("2006-01-02 15:04"),
		})
	}
	return result, nil
}

// listExternalSecrets lists ESO-managed credentials synced from Bitwarden.
func (s *Server) listExternalSecrets(ctx context.Context, owner string) ([]credentialMeta, error) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecretList",
	})
	// List ALL ExternalSecrets in the namespace — shared (cluster-managed) ones
	// don't carry the owner label but should still be visible.
	if err := s.k8sClient.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, err
	}

	result := make([]credentialMeta, 0, len(list.Items))
	for _, es := range list.Items {
		labels := es.GetLabels()
		platform := labels[TokenLabel]
		if platform == "" {
			// No platform label — try to infer from the secret name pattern:
			// harmostes-{platform}-token → platform
			name := es.GetName()
			if strings.Contains(name, "-") {
				parts := strings.Split(name, "-")
				if len(parts) >= 3 && parts[len(parts)-1] == "token" {
					platform = strings.Join(parts[1:len(parts)-1], "-")
				}
			}
			if platform == "" {
				continue
			}
		}
		syncStatus := "Pending"
		if conditions, found, _ := unstructured.NestedSlice(es.Object, "status", "conditions"); found {
			for _, c := range conditions {
				cond, _ := c.(map[string]any)
				if cond["type"] == "Ready" {
					if cond["status"] == "True" {
						syncStatus = "Synced"
					} else {
						syncStatus = "Error"
					}
				}
			}
		}
		created := es.GetCreationTimestamp()
		result = append(result, credentialMeta{
			Name:       es.GetName(),
			Platform:   platform,
			Source:     "bitwarden",
			SyncStatus: syncStatus,
			MaskedVal:  "•••• (BSM)", // value lives in Bitwarden, not readable from ES
			CreatedAt:  created.Format("2006-01-02 15:04"),
		})
	}
	return result, nil
}

// createDirectSecret creates a direct k8s Secret (personal token not in BSM).
func (s *Server) createDirectSecret(ctx context.Context, owner, platform, token string) error {
	name := tokenSecretName(owner, platform)
	labels := SanitizeLabels(map[string]string{}, owner)
	labels[TokenLabel] = platform
	labels[SourceLabel] = "direct"

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: s.namespace, Labels: labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{TokenDataKey: []byte(token)},
	}
	if err := s.k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("create secret: %w", err)
	}
	s.logger.Info("direct credential created", "owner", owner, "name", name, "platform", platform)
	return nil
}

// createExternalSecret creates an ExternalSecret CR that syncs a Bitwarden
// secret into the namespace via the ESO. The user provides the BSM secret ID.
func (s *Server) createExternalSecret(ctx context.Context, owner, platform, bsmKey string) error {
	name := tokenSecretName(owner, platform)
	labels := SanitizeLabels(map[string]string{}, owner)
	labels[TokenLabel] = platform
	labels[SourceLabel] = "bitwarden"

	es := s.newExternalSecretObject(name)
	es.SetLabels(labels)
	es.SetNamespace(s.namespace)

	spec := map[string]any{
		"secretStoreRef": map[string]any{
			"kind": "ClusterSecretStore",
			"name": "bitwarden",
		},
		"refreshInterval": "1h",
		"target": map[string]any{
			"name": name,
		},
		"data": []map[string]any{
			{
				"secretKey": TokenDataKey,
				"remoteRef": map[string]any{
					"key": bsmKey,
				},
			},
		},
	}
	unstructured.SetNestedMap(es.Object, spec, "spec")

	if err := s.k8sClient.Create(ctx, es); err != nil {
		return fmt.Errorf("create ExternalSecret: %w", err)
	}
	s.logger.Info("ExternalSecret credential created", "owner", owner, "name", name, "platform", platform, "bsmKey", bsmKey)
	return nil
}

func (s *Server) newExternalSecretObject(name string) *unstructured.Unstructured {
	es := &unstructured.Unstructured{}
	es.SetGroupVersionKind(externalSecretGVK)
	es.SetName(name)
	return es
}

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

// Suppress unused import for json (used by ExternalSecret serialization if extended).
var _ = json.Marshal

// listTokens returns all credentials (ESO + direct) as a flat list. Used by
// the workflow creation form dropdown.
func (s *Server) listTokens(r *http.Request, owner string) ([]credentialMeta, error) {
	return s.listAllCredentials(r.Context(), owner)
}
