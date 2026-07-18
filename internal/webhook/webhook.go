// Package webhook provides HTTP handlers for git push events (GitHub/GitLab/Forgejo)
// that trigger workflow runs immediately instead of waiting for the poll interval.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

const (
	// TriggerRevisionAnnotation is set on a Workflow when a webhook arrives.
	// The Reconcile respects this annotation to trigger immediately.
	TriggerRevisionAnnotation = "harmostes.dev/trigger-revision"
)

// Handler is an HTTP handler for git push events.
type Handler struct {
	client.Client
	log logr.Logger
}

// NewHandler creates a new webhook handler.
func NewHandler(k8sClient client.Client, logger logr.Logger) *Handler {
	return &Handler{
		Client: k8sClient,
		log:    logger,
	}
}

// PushEvent represents a generic git push event (simplified from GitHub/GitLab/Forgejo schemas).
type PushEvent struct {
	Ref        string `json:"ref"`         // e.g., "refs/heads/main"
	Repository struct {
		URL       string `json:"url"`        // clone URL
		HTMLURL   string `json:"html_url"`  // web URL
		FullName string `json:"full_name"` // org/repo
	} `json:"repository"`
	After      string `json:"after"`  // new HEAD commit SHA
	Before     string `json:"before"` // old HEAD commit SHA (0000000000000000000000000000000000000000 = new branch)
}

// ServeHTTP handles webhook POST requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request, workflowName string) {
	// Only POST is supported
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read request body
	body, err := io.ReadAll(req.Body)
	if err != nil {
		h.log.Error(err, "failed to read webhook body")
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	// Fetch the workflow
	var wf v1alpha1.Workflow
	if err := h.Get(req.Context(), types.NamespacedName{Namespace: req.URL.Query().Get("namespace"), Name: workflowName}, &wf); err != nil {
		h.log.Error(err, "workflow not found", "workflow", workflowName)
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	// Verify signature if webhook secret is configured
	if wf.Spec.Source.Webhook != nil && wf.Spec.Source.Webhook.Secret != "" {
		// GitHub: X-Hub-Signature-256: sha256=...
		// GitLab: X-Gitlab-Token (secret)
		// Forgejo: X-Forgejo-Signature: sha256=...
		if !h.verifySignature(req, body, wf.Spec.Source.Webhook.URL, wf.Spec.Source.Webhook.Secret) {
			h.log.Info("webhook signature verification failed", "workflow", workflowName)
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
			return
		}
	}

	// Parse push event
	var pushEvent PushEvent
	if err := json.Unmarshal(body, &pushEvent); err != nil {
		h.log.Error(err, "failed to parse push event")
		http.Error(w, "invalid event payload", http.StatusBadRequest)
		return
	}

	// Extract revision (commit SHA)
	revision := pushEvent.After
	if revision == "" || revision == "0000000000000000000000000000000000000000" {
		h.log.Info("invalid revision in push event", "after", pushEvent.After)
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}

	// Extract branch from ref
	branch := strings.TrimPrefix(pushEvent.Ref, "refs/heads/")
	if branch == "" {
		branch = strings.TrimPrefix(pushEvent.Ref, "refs/tags/")
	}

	// Validate branch matches workflow spec
	if wf.Spec.Source.Branch != "" && branch != wf.Spec.Source.Branch {
		h.log.Info("push event branch does not match workflow spec",
			"push-branch", branch, "workflow-branch", wf.Spec.Source.Branch)
		w.WriteHeader(http.StatusAccepted) // Accept but don't trigger
		fmt.Fprintf(w, "branch %s does not match workflow spec (wants %s)\n", branch, wf.Spec.Source.Branch)
		return
	}

	// Set trigger annotation
	base := wf.DeepCopy()
	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}
	wf.Annotations[TriggerRevisionAnnotation] = revision

	// Patch the workflow
	if err := h.Patch(req.Context(), &wf, client.MergeFrom(base)); err != nil {
		h.log.Error(err, "failed to annotate workflow", "workflow", workflowName, "revision", revision)
		http.Error(w, "failed to trigger workflow", http.StatusInternalServerError)
		return
	}

	h.log.Info("webhook triggered workflow", "workflow", workflowName, "branch", branch, "revision", revision, "revision", revision)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "workflow %s triggered for revision %s\n", workflowName, revision)
}

// verifySignature verifies the HMAC signature from the webhook request.
// Returns false if verification fails.
func (h *Handler) verifySignature(req *http.Request, body []byte, hostURL, secret string) bool {
	var sigHeader, sigPrefix string

	switch {
	case strings.Contains(hostURL, "github.com"):
		sigHeader = req.Header.Get("X-Hub-Signature-256")
		sigPrefix = "sha256="
	case strings.Contains(hostURL, "gitlab.com"):
		sigToken := req.Header.Get("X-Gitlab-Token")
		return sigToken == secret // GitLab uses simple token comparison
	case strings.Contains(hostURL, "forgejo"):
		sigHeader = req.Header.Get("X-Forgejo-Signature")
		sigPrefix = "sha256="
	default:
		h.log.Info("unknown git host for signature verification", "url", hostURL)
		return true // Unknown host, skip verification
	}

	if sigHeader == "" {
		return false
	}

	// Extract signature from header
	sig := strings.TrimPrefix(sigHeader, sigPrefix)
	if len(sig) != 64 { // sha256 hex = 64 chars
		return false
	}

	// Compute HMAC
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	// Constant-time comparison to avoid timing attacks
	return hmac.Equal([]byte(expectedSig), []byte(sig))
}
