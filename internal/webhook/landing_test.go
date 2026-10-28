// landing_test.go — the template landing surface (three-tier 1c): a signed
// push to a template's OWN repo stashes pending revision identity; every
// refusal path is fact-specific; the reconciler consumes it exactly once.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func landingTemplate() *v1alpha1.WorkflowTemplate {
	return &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: "test-ns"},
		Spec: v1alpha1.WorkflowTemplateSpec{
			Source: &v1alpha1.TemplateSourceSpec{
				Forge:  "github",
				Repo:   "tibrezus/harmostes-template-pr-review",
				Path:   "template.yaml",
				Branch: "main",
				SecretRef: &v1alpha1.SecretRef{
					Name: "tpl-webhook-secret",
					Key:  "secret",
				},
			},
		},
	}
}

func landingPush(t *testing.T, repo, ref, sha, pusher string) []byte {
	t.Helper()
	return []byte(`{"ref":"` + ref + `","after":"` + sha + `","pusher":{"name":"` + pusher + `"},"repository":{"full_name":"` + repo + `"}}`)
}

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestTemplateLanding_RecordsPendingAnnotation(t *testing.T) {
	const secretVal = "tpl-s3cr3t"
	h := newTestHandler(
		landingTemplate(),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tpl-webhook-secret", Namespace: "test-ns"}, Data: map[string][]byte{"secret": []byte(secretVal)}},
	)
	body := landingPush(t, "tibrezus/harmostes-template-pr-review", "refs/heads/main", validSHA, "tibrez")
	req := httptest.NewRequest(http.MethodPost, "/webhook/templates/pr-review", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body, secretVal))
	rec := httptest.NewRecorder()

	h.ServeTemplateLanding(rec, req, "pr-review")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got v1alpha1.WorkflowTemplate
	if err := h.Get(context.Background(), types.NamespacedName{Name: "pr-review", Namespace: "test-ns"}, &got); err != nil {
		t.Fatalf("get template: %v", err)
	}
	raw := got.Annotations[v1alpha1.TemplatePendingLandingAnnotation]
	if raw == "" {
		t.Fatal("no pending landing annotation recorded")
	}
	var landing v1alpha1.TemplateRevisionLanding
	if err := json.Unmarshal([]byte(raw), &landing); err != nil {
		t.Fatalf("pending landing not valid JSON: %v", err)
	}
	if landing.Sha != validSHA || landing.Pusher != "tibrez" || landing.At == "" {
		t.Fatalf("landing identity incomplete: %+v", landing)
	}
}

// Fail closed, path by path:
//   - no source declared → 404 (nothing to correlate; not an error condition)
//   - source without secretRef → 401 unsigned push (an unprovable landing
//     is untrusted — the annotation feeds the REVISION RECORD, so it must
//     be as proven as the CR delivery itself)
//   - bad signature → 401
//   - repo mismatch → 400 (a repo-wide webhook fires for every template
//     and branch; only the declared pair correlates)
//   - branch mismatch → 400
func TestTemplateLanding_Refusals(t *testing.T) {
	const secretVal = "tpl-s3cr3t"

	noSource := &v1alpha1.WorkflowTemplate{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "test-ns"}}
	noSecret := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "nosecret", Namespace: "test-ns"},
		Spec:       v1alpha1.WorkflowTemplateSpec{Source: &v1alpha1.TemplateSourceSpec{Forge: "github", Repo: "tibrezus/harmostes-template-pr-review", Path: "t.yaml"}},
	}
	secretObj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tpl-webhook-secret", Namespace: "test-ns"}, Data: map[string][]byte{"secret": []byte(secretVal)}}

	t.Run("no source declared", func(t *testing.T) {
		h := newTestHandler(noSource)
		rec := httptest.NewRecorder()
		h.ServeTemplateLanding(rec, httptest.NewRequest(http.MethodPost, "/webhook/templates/plain", bytes.NewReader(landingPush(t, "x/y", "refs/heads/main", validSHA, "p"))), "plain")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404", rec.Code)
		}
	})

	t.Run("no secretRef fails closed", func(t *testing.T) {
		h := newTestHandler(noSecret)
		rec := httptest.NewRecorder()
		body := landingPush(t, "tibrezus/harmostes-template-pr-review", "refs/heads/main", validSHA, "p")
		req := httptest.NewRequest(http.MethodPost, "/webhook/templates/nosecret", bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", sign(body, secretVal)) // signed, but the template declares no proof surface
		h.ServeTemplateLanding(rec, req, "nosecret")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
	})

	t.Run("bad signature", func(t *testing.T) {
		h := newTestHandler(landingTemplate(), secretObj)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/webhook/templates/pr-review", bytes.NewReader(landingPush(t, "tibrezus/harmostes-template-pr-review", "refs/heads/main", validSHA, "p")))
		req.Header.Set("X-Hub-Signature-256", sign([]byte("other body"), secretVal))
		h.ServeTemplateLanding(rec, req, "pr-review")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
	})

	for _, tc := range []struct {
		name      string
		repo, ref string
	}{
		{"repo mismatch", "someone/else", "refs/heads/main"},
		{"branch mismatch", "tibrezus/harmostes-template-pr-review", "refs/heads/dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(landingTemplate(), secretObj)
			rec := httptest.NewRecorder()
			body := landingPush(t, tc.repo, tc.ref, validSHA, "p")
			req := httptest.NewRequest(http.MethodPost, "/webhook/templates/pr-review", bytes.NewReader(body))
			req.Header.Set("X-Hub-Signature-256", sign(body, secretVal))
			h.ServeTemplateLanding(rec, req, "pr-review")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400", rec.Code)
			}
		})
	}
}
