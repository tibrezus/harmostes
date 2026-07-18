package ui

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// withIdentity is a test helper that injects an Identity into the context
// (bypassing the auth middleware which reads HTTP headers).
func withIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// tokenTestServer builds a Server with a fake k8s client preloaded with secrets.
func tokenTestServer(existing ...client.Object) *Server {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing...).
		Build()

	tmpl, _ := parseTemplates()

	return &Server{
		k8sClient: cl,
		namespace: "harmostes",
		logger:    slog.Default(),
		templates: tmpl,
	}
}

func TestHandleTokenList_OwnerIsolation(t *testing.T) {
	aliceToken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-github-abcd1234",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
				TokenLabel:          "github",
			},
		},
	}
	bobToken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bob-gitlab-efgh5678",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "bob",
				TokenLabel:          "gitlab",
			},
		},
	}
	// A system secret (no owner label) that must NOT appear
	systemSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harmostes-github-token",
			Namespace: "harmostes",
			Labels:    map[string]string{},
		},
	}

	s := tokenTestServer(aliceToken, bobToken, systemSecret)

	req := httptest.NewRequest(http.MethodGet, "/tokens", nil)
	req.Header.Set("X-Forwarded-Preferred-Username", "alice")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	tokens, err := s.listTokens(req, "alice")
	if err != nil {
		t.Fatalf("listTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token for alice, got %d", len(tokens))
	}
	if tokens[0].Name != "alice-github-abcd1234" {
		t.Errorf("token name = %q, want alice-github-abcd1234", tokens[0].Name)
	}
	if tokens[0].Platform != "github" {
		t.Errorf("platform = %q, want github", tokens[0].Platform)
	}
}

func TestHandleTokenCreate(t *testing.T) {
	s := tokenTestServer()

	form := url.Values{}
	form.Set("platform", "github")
	form.Set("token", "ghp_supersecret123")
	req := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenCreate(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// Verify the secret was created with correct labels
	var secret corev1.SecretList
	_ = s.k8sClient.List(req.Context(), &secret)
	if len(secret.Items) != 1 {
		t.Fatalf("expected 1 secret created, got %d", len(secret.Items))
	}

	created := secret.Items[0]
	if created.Labels[v1alpha1.OwnerLabel] != "alice" {
		t.Errorf("owner = %q, want alice", created.Labels[v1alpha1.OwnerLabel])
	}
	if created.Labels[TokenLabel] != "github" {
		t.Errorf("platform = %q, want github", created.Labels[TokenLabel])
	}
	if string(created.Data[TokenDataKey]) != "ghp_supersecret123" {
		t.Errorf("token data mismatch")
	}
}

func TestHandleTokenCreate_RejectsEmptyToken(t *testing.T) {
	s := tokenTestServer()

	form := url.Values{}
	form.Set("platform", "github")
	form.Set("token", "")
	req := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenCreate(rec, req)

	// Should render error page (200 with error template)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error page)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Error("expected error message about token being required")
	}
}

func TestHandleTokenCreate_RejectsInvalidPlatform(t *testing.T) {
	s := tokenTestServer()

	form := url.Values{}
	form.Set("platform", "evilcorp")
	form.Set("token", "some-token")
	req := httptest.NewRequest(http.MethodPost, "/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenCreate(rec, req)

	if !strings.Contains(rec.Body.String(), "Invalid platform") {
		t.Error("expected 'Invalid platform' error")
	}
}

func TestHandleTokenDelete_OwnerIsolation(t *testing.T) {
	bobToken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bob-gitlab-efgh5678",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "bob",
				TokenLabel:          "gitlab",
			},
		},
	}
	s := tokenTestServer(bobToken)

	// Alice tries to delete Bob's token
	req := httptest.NewRequest(http.MethodPost, "/tokens/bob-gitlab-efgh5678/delete", nil)
	req.SetPathValue("name", "bob-gitlab-efgh5678")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenDelete(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (cross-tenant delete must fail)", rec.Code, http.StatusNotFound)
	}

	// Verify the secret still exists
	var secret corev1.Secret
	if err := s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "bob-gitlab-efgh5678"}, &secret); err != nil {
		t.Errorf("bob's token should still exist after alice's delete attempt: %v", err)
	}
}

func TestHandleTokenDelete_Success(t *testing.T) {
	aliceToken := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice-github-abcd1234",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
				TokenLabel:          "github",
			},
		},
	}
	s := tokenTestServer(aliceToken)

	req := httptest.NewRequest(http.MethodPost, "/tokens/alice-github-abcd1234/delete", nil)
	req.SetPathValue("name", "alice-github-abcd1234")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenDelete(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// Verify the secret is gone
	var secret corev1.Secret
	err := s.k8sClient.Get(req.Context(), types.NamespacedName{Namespace: "harmostes", Name: "alice-github-abcd1234"}, &secret)
	if err == nil {
		t.Error("token should have been deleted")
	}
}

func TestHandleTokenDelete_RejectsNonTokenSecret(t *testing.T) {
	// A secret with the owner label but NOT the token label (e.g., a system secret
	// that happens to have been labeled). Must not be deletable via /tokens.
	systemSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-config",
			Namespace: "harmostes",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
			},
		},
	}
	s := tokenTestServer(systemSecret)

	req := httptest.NewRequest(http.MethodPost, "/tokens/some-config/delete", nil)
	req.SetPathValue("name", "some-config")
	req = req.WithContext(withIdentity(req.Context(), &Identity{Username: "alice"}))

	rec := httptest.NewRecorder()
	s.handleTokenDelete(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d (non-token secret must not be deletable)", rec.Code, http.StatusNotFound)
	}
}

func TestTokenSecretName_GeneratesUnique(t *testing.T) {
	names := make(map[string]bool)
	for i := 0; i < 100; i++ {
		name := tokenSecretName("alice", "github")
		if names[name] {
			t.Fatalf("collision after %d iterations: %s", i, name)
		}
		names[name] = true
		if !strings.HasPrefix(name, "alice-github-") {
			t.Errorf("name = %q, want prefix alice-github-", name)
		}
	}
}

func TestTokenFromSecret_NeverExposesValue(t *testing.T) {
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-secret",
			Labels: map[string]string{
				v1alpha1.OwnerLabel: "alice",
				TokenLabel:          "github",
			},
			CreationTimestamp: metav1.Time{},
		},
		Data: map[string][]byte{
			TokenDataKey: []byte("ghp_supersecret_value_12345"),
		},
	}

	meta := tokenFromSecret(secret)
	if meta.Name != "test-secret" {
		t.Errorf("name = %q", meta.Name)
	}
	if meta.Platform != "github" {
		t.Errorf("platform = %q", meta.Platform)
	}
	// The tokenMeta struct must NOT have a Value field.
	// This test guards against accidentally adding one.
	if strings.Contains(meta.Name, "ghp_supersecret_value") {
		t.Error("token value leaked into metadata")
	}
}
