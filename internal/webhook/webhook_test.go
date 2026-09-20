package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return NewHandler(cl, "test-ns", ctrl.Log.WithName("test-webhook"))
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

// ---------------------------------------------------------------------------
// Consolidated pull_request events (ADR-0006)
// ---------------------------------------------------------------------------

// prWorkflow builds a workflow whose webhook secret (if any) resolves from
// the given inline secret value via the direct-secret (legacy) mode.
func prWorkflow(name, secret, hostURL string) (*v1alpha1.Workflow, *corev1.Secret) {
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "harmostes"},
		Spec: v1alpha1.WorkflowSpec{
			Source: v1alpha1.SourceSpec{
				Webhook: &v1alpha1.WebhookSpec{URL: hostURL, Secret: secret},
			},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-sec", Namespace: "harmostes"},
		Data:       map[string][]byte{"secret": []byte(secret)},
	}
	return wf, sec
}

// prEventBody builds a GitHub/Forgejo-consolidated pull_request payload.
func prEventBody(action string, pr int, sha, fullName, htmlURL string) string {
	return fmt.Sprintf(`{"action":%q,"number":%d,"pull_request":{"number":%d,`+
		`"state":"open","head":{"sha":%q}},"repository":{"full_name":%q,"html_url":%q}}`,
		action, pr, pr, sha, fullName, htmlURL)
}

func TestPullRequestEventAnnotates(t *testing.T) {
	wf, sec := prWorkflow("w-pr", "", "")
	h := newTestHandler(sec, wf)

	body := prEventBody("labeled", 42, "abc123def4567890", "tibrez/rhesadox", "https://git.rezus.cloud/tibrez/rhesadox")
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-pr?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-pr")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-pr"}, &got)
	if got.Annotations[TriggerRevisionAnnotation] != "abc123def4567890" {
		t.Errorf("trigger-revision = %q", got.Annotations[TriggerRevisionAnnotation])
	}
	if got.Annotations[TriggerPRAnnotation] != "git.rezus.cloud/tibrez/rhesadox#42" {
		t.Errorf("trigger-pr = %q", got.Annotations[TriggerPRAnnotation])
	}
	if got.Annotations[TriggerActionAnnotation] != "labeled" {
		t.Errorf("trigger-action = %q", got.Annotations[TriggerActionAnnotation])
	}
}

func TestPullRequestEventGitHubNormalizesHost(t *testing.T) {
	wf, _ := prWorkflow("w-gh", "", "")
	h := newTestHandler(wf)

	body := prEventBody("synchronize", 10, "def456abcdef456abcdef456abcdef456abcd", "tibrezus/harmostes", "https://github.com/tibrezus/harmostes")
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-gh?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-gh")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-gh"}, &got)
	if g := got.Annotations[TriggerPRAnnotation]; g != "github.com/tibrezus/harmostes#10" {
		t.Errorf("trigger-pr = %q, want github.com-normalized", g)
	}
}

func TestPullRequestEventNonWakeActionNoOp(t *testing.T) {
	wf, _ := prWorkflow("w-nw", "", "")
	h := newTestHandler(wf)

	body := prEventBody("assigned", 5, "aaaa", "o/r", "https://git.rezus.cloud/o/r")
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-nw?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-nw")
	if rec.Code != http.StatusOK {
		t.Fatalf("non-wake action must 200, got %d", rec.Code)
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-nw"}, &got)
	if g := got.Annotations[TriggerPRAnnotation]; g != "" {
		t.Errorf("non-wake action must not annotate, got %q", g)
	}
}

func TestPullRequestEventMissingHead400(t *testing.T) {
	wf, _ := prWorkflow("w-mf", "", "")
	h := newTestHandler(wf)

	body := `{"action":"labeled","number":1,"pull_request":{"number":1},"repository":{"full_name":"o/r","html_url":"https://git.rezus.cloud/o/r"}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-mf?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-mf")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing head sha must 400, got %d", rec.Code)
	}
}

func TestForgejoSignatureHeaderVerified(t *testing.T) {
	const secret = "s3cr3t"
	wf, sec := prWorkflow("w-fj", secret, "https://git.rezus.cloud/tibrez/r") // hostname WITHOUT "forgejo"
	h := newTestHandler(sec, wf)

	body := prEventBody("labeled", 7, "fff000fff000fff000fff000fff000fff000fff0", "tibrez/r", "https://git.rezus.cloud/tibrez/r")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook/w-fj?namespace=harmostes", strings.NewReader(body))
	req.Header.Set("X-Forgejo-Signature", "sha256="+sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-fj")
	if rec.Code != http.StatusOK {
		t.Fatalf("valid Forgejo signature rejected: %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/webhook/w-fj?namespace=harmostes", strings.NewReader(body))
	req.Header.Set("X-Forgejo-Signature", "sha256="+strings.Repeat("0", 64))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-fj")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature must 401, got %d", rec.Code)
	}
}

func TestUnsignedRejectedWhenSecretConfigured(t *testing.T) {
	wf, sec := prWorkflow("w-us", "s3cr3t", "https://git.example.com/o/r")
	h := newTestHandler(sec, wf)

	body := prEventBody("labeled", 3, "bbb000bbb000bbb000bbb000bbb000bbb000bbb0", "o/r", "https://git.example.com/o/r")
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-us?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-us")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned + secret configured must fail closed (401), got %d", rec.Code)
	}
}

// TestPullRequestEventCICompletedAnnotates pins the CI-notify wake (#r33):
// a repo's own CI pipeline POSTs a pull_request-shaped payload with
// action=ci_completed when a run finishes (Forgejo Actions emits no
// run-completion webhook). The handler stays dumb — verify → parse →
// annotate; the gate re-verifies label ∧ CI at the head itself, so the
// notification carries no authority.
func TestPullRequestEventCICompletedAnnotates(t *testing.T) {
	wf, sec := prWorkflow("w-ci", "", "")
	h := newTestHandler(sec, wf)

	body := prEventBody("ci_completed", 42, "abc123def4567890", "tibrez/rhesadox", "https://git.rezus.cloud/tibrez/rhesadox")
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci?namespace=harmostes", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-ci"}, &got)
	if got.Annotations[TriggerRevisionAnnotation] != "abc123def4567890" {
		t.Errorf("trigger-revision = %q", got.Annotations[TriggerRevisionAnnotation])
	}
	if got.Annotations[TriggerActionAnnotation] != "ci_completed" {
		t.Errorf("trigger-action = %q", got.Annotations[TriggerActionAnnotation])
	}
}

// #488: the native review request arms the gate — the forge's action passes
// the vocabulary unnormalized (it is already GitHub-shaped), and the
// withdrawal re-evaluates like unlabeled.
func TestPullRequestEventReviewRequestedArms(t *testing.T) {
	for _, tc := range []struct{ action string }{
		{"review_requested"},
		{"review_request_removed"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			wf, sec := prWorkflow("w-rr", "", "")
			h := newTestHandler(sec, wf)

			body := prEventBody(tc.action, 42, "abc123def4567890", "tibrez/rhesadox", "https://git.rezus.cloud/tibrez/rhesadox")
			req := httptest.NewRequest(http.MethodPost, "/webhook/w-rr?namespace=harmostes", strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req, "w-rr")

			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var got v1alpha1.Workflow
			_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-rr"}, &got)
			if got.Annotations[TriggerActionAnnotation] != tc.action {
				t.Errorf("trigger-action = %q, want %q", got.Annotations[TriggerActionAnnotation], tc.action)
			}
			if got.Annotations[TriggerRevisionAnnotation] != "abc123def4567890" {
				t.Errorf("trigger-revision = %q", got.Annotations[TriggerRevisionAnnotation])
			}
		})
	}
}

// --- host-native CI completion wakes (#556) ---

// ciAssertion reads the workflow back and checks the three wake annotations.
func ciAssertion(t *testing.T, h *Handler, wfName, wantRepo string, wantActionSet bool) map[string]string {
	t.Helper()
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: wfName}, &got)
	ann := got.Annotations
	if ann[TriggerActionAnnotation] != "ci_completed" {
		t.Errorf("trigger-action = %q, want ci_completed", ann[TriggerActionAnnotation])
	}
	if wantActionSet && ann[v1alpha1.TriggerRepoAnnotation] != wantRepo {
		t.Errorf("trigger-repo = %q, want %q", ann[v1alpha1.TriggerRepoAnnotation], wantRepo)
	}
	if _, hasPR := ann[TriggerPRAnnotation]; hasPR {
		t.Errorf("CI wakes must NOT set trigger-pr (payloads carry no PR number); got %q", ann[TriggerPRAnnotation])
	}
	return ann
}

func TestCIWakeCheckSuiteAnnotates(t *testing.T) {
	wf, _ := prWorkflow("w-ci", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"action":"completed","check_suite":{"head_sha":"` + validSHA + `","conclusion":"success"},"repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ann := ciAssertion(t, h, "w-ci", "github.com/tibrezus/harmostes", true)
	if ann[TriggerRevisionAnnotation] != validSHA {
		t.Errorf("trigger-revision = %q", ann[TriggerRevisionAnnotation])
	}
}

func TestCIWakeCheckSuiteNonTerminalIgnored(t *testing.T) {
	wf, _ := prWorkflow("w-ci2", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"action":"requested","check_suite":{"head_sha":"` + validSHA + `"},"repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci2?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci2")
	if rec.Code != http.StatusOK {
		t.Fatalf("non-terminal completions are 200-ignored (hosts retry non-2xx), got %d", rec.Code)
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-ci2"}, &got)
	if got.Annotations[TriggerActionAnnotation] == "ci_completed" {
		t.Fatal("check_suite requested must not wake")
	}
}

func TestCIWakeWorkflowRunAnnotates(t *testing.T) {
	wf, _ := prWorkflow("w-ci3", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"action":"completed","workflow_run":{"head_sha":"` + validSHA + `","conclusion":"failure"},"repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci3?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ann := ciAssertion(t, h, "w-ci3", "github.com/tibrezus/harmostes", true)
	if ann[TriggerRevisionAnnotation] != validSHA {
		t.Errorf("trigger-revision = %q", ann[TriggerRevisionAnnotation])
	}
}

func TestCIWakeStatusTerminalWakes(t *testing.T) {
	wf, _ := prWorkflow("w-ci4", "", "")
	h := newTestHandler(wf)
	for _, state := range []string{"success", "failure", "error"} {
		body := []byte(`{"state":"` + state + `","sha":"` + validSHA + `","repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci4?namespace=harmostes", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req, "w-ci4")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %s: code %d", state, rec.Code)
		}
		if ann := ciAssertion(t, h, "w-ci4", "github.com/tibrezus/harmostes", true); ann[TriggerRevisionAnnotation] != validSHA {
			t.Fatalf("state %s: trigger-revision = %q", state, ann[TriggerRevisionAnnotation])
		}
	}
}

func TestCIWakeStatusPendingIgnored(t *testing.T) {
	wf, _ := prWorkflow("w-ci5", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"state":"pending","sha":"` + validSHA + `","repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci5?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci5")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending is 200-ignored, got %d", rec.Code)
	}
	var got v1alpha1.Workflow
	_ = h.Get(context.Background(), types.NamespacedName{Namespace: "harmostes", Name: "w-ci5"}, &got)
	if got.Annotations[TriggerActionAnnotation] == "ci_completed" {
		t.Fatal("status pending must not wake")
	}
}

func TestCIWakeMissingSHA400(t *testing.T) {
	wf, _ := prWorkflow("w-ci6", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"action":"completed","check_suite":{"head_sha":""},"repository":{"full_name":"tibrezus/harmostes","html_url":"https://github.com/tibrezus/harmostes"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci6?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci6")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing head sha must 400, got %d", rec.Code)
	}
}

func TestCIWakeForgejoHostNormalized(t *testing.T) {
	wf, _ := prWorkflow("w-ci7", "", "")
	h := newTestHandler(wf)
	body := []byte(`{"action":"completed","check_suite":{"head_sha":"` + validSHA + `"},"repository":{"full_name":"tibrez/rhesadox","html_url":"https://git.rezus.cloud/tibrez/rhesadox"}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/w-ci7?namespace=harmostes", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req, "w-ci7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	ciAssertion(t, h, "w-ci7", "git.rezus.cloud/tibrez/rhesadox", true)
}
