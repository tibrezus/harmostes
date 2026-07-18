package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/k8s"
)

// newTestHandler builds a webhook Handler backed by a fake client pre-loaded
// with the given objects.
func newTestHandler(objs ...client.Object) *Handler {
	cl := fake.NewClientBuilder().
		WithScheme(k8s.Scheme()).
		WithObjects(objs...).
		Build()
	return NewHandler(cl, ctrl.Log.WithName("test-webhook"))
}

// githubPushBody returns a minimal GitHub-style push payload.
func githubPushBody(t *testing.T, after string) []byte {
	t.Helper()
	return []byte(`{"ref":"refs/heads/main","after":"` + after + `","repository":{"html_url":"https://github.com/rezuscloud/platform-website","full_name":"rezuscloud/platform-website"}}`)
}

// validSHA is a 40-char hex commit SHA accepted by the handler.
const validSHA = "abc123def4567890abc123def4567890abc123de"

// githubSignature computes the X-Hub-Signature-256 header value for a body+secret.
func githubSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// --- secretRef resolution (production mode) ---

// TestServeHTTP_SecretRefResolvesFromSecret verifies that a webhook whose
// WebhookSpec uses secretRef resolves the HMAC secret from a Kubernetes Secret.
func TestServeHTTP_SecretRefResolvesFromSecret(t *testing.T) {
	const secretVal = "s3cr3t"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wh-secret", Namespace: "harmostes"},
		Data:       map[string][]byte{"secret": []byte(secretVal)},
	}
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-website", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Branch: "main",
				Webhook: &v1alpha1.WebhookSpec{
					URL: "https://github.com/rezuscloud/platform-website",
					SecretRef: &v1alpha1.SecretRef{
						Name: "wh-secret",
						Key:  "secret",
					},
				},
			},
		},
	}
	h := newTestHandler(secret, wf)

	body := githubPushBody(t, validSHA)
	req := httptest.NewRequest(http.MethodPost, "/webhook/platform-website?namespace=harmostes", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSignature(body, secretVal))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The workflow should now carry the trigger-revision annotation.
	var got v1alpha1.Workflow
	if err := h.Get(context.Background(), types.NamespacedName{Name: "platform-website", Namespace: "harmostes"}, &got); err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if got.Annotations[TriggerRevisionAnnotation] != validSHA {
		t.Fatalf("expected trigger annotation %q, got %q", validSHA, got.Annotations[TriggerRevisionAnnotation])
	}
}

// TestServeHTTP_SecretRefBadSignatureReturns401 verifies that a wrong HMAC
// (secret resolved from Secret) is rejected with 401.
func TestServeHTTP_SecretRefBadSignatureReturns401(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wh-secret", Namespace: "harmostes"},
		Data:       map[string][]byte{"secret": []byte("s3cr3t")},
	}
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-website", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Branch: "main",
				Webhook: &v1alpha1.WebhookSpec{
					URL: "https://github.com/rezuscloud/platform-website",
					SecretRef: &v1alpha1.SecretRef{
						Name: "wh-secret",
						Key:  "secret",
					},
				},
			},
		},
	}
	h := newTestHandler(secret, wf)

	body := githubPushBody(t, validSHA)
	req := httptest.NewRequest(http.MethodPost, "/webhook/platform-website?namespace=harmostes", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad signature, got %d", rec.Code)
	}
}

// TestServeHTTP_SecretRefMissingSecretReturns502 verifies that a secretRef
// pointing at a non-existent Secret is reported as a gateway error.
func TestServeHTTP_SecretRefMissingSecretReturns502(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-website", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Branch: "main",
				Webhook: &v1alpha1.WebhookSpec{
					URL: "https://github.com/rezuscloud/platform-website",
					SecretRef: &v1alpha1.SecretRef{
						Name: "does-not-exist",
						Key:  "secret",
					},
				},
			},
		},
	}
	h := newTestHandler(wf)

	body := githubPushBody(t, validSHA)
	req := httptest.NewRequest(http.MethodPost, "/webhook/platform-website?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for missing secret, got %d", rec.Code)
	}
}

// --- direct secret (testing/legacy mode) ---

// TestServeHTTP_DirectSecretVerifies verifies that the direct Secret field
// (testing mode) still works for HMAC verification.
func TestServeHTTP_DirectSecretVerifies(t *testing.T) {
	const secretVal = "s3cr3t"
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-website", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Branch: "main",
				Webhook: &v1alpha1.WebhookSpec{
					URL:    "https://github.com/rezuscloud/platform-website",
					Secret: secretVal,
				},
			},
		},
	}
	h := newTestHandler(wf)

	body := githubPushBody(t, validSHA)
	req := httptest.NewRequest(http.MethodPost, "/webhook/platform-website?namespace=harmostes", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", githubSignature(body, secretVal))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- misc ---

// TestServeHTTP_BranchMismatchReturns202 verifies that a push to a branch that
// doesn't match the workflow spec is acknowledged but does not trigger a run.
func TestServeHTTP_BranchMismatchReturns202(t *testing.T) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-website", Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Branch: "main",
				Webhook: &v1alpha1.WebhookSpec{
					URL: "https://github.com/rezuscloud/platform-website",
				},
			},
		},
	}
	h := newTestHandler(wf)

	body := []byte(`{"ref":"refs/heads/feature-branch","after":"` + validSHA + `","repository":{"html_url":""}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/platform-website?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 for branch mismatch, got %d", rec.Code)
	}
}

// TestServeHTTP_MethodNotAllowed verifies that only POST is accepted.
func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	h := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/webhook/platform-website?namespace=harmostes", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req, "platform-website")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
