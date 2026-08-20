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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

const (
	// TriggerRevisionAnnotation is set on a Workflow when a webhook arrives.
	// The Reconcile respects this annotation to trigger immediately.
	TriggerRevisionAnnotation = "harmostes.dev/trigger-revision"

	// TriggerPRAnnotation carries the pull-request pointer ("host/owner/name#N")
	// from a pull_request event to the Review-Ready Gate.
	TriggerPRAnnotation = "harmostes.dev/trigger-pr"

	// TriggerActionAnnotation carries the consolidated event action
	// (labeled, unlabeled, synchronize, opened, reopened, closed, …).
	TriggerActionAnnotation = "harmostes.dev/trigger-action"
)

// Handler is an HTTP handler for git push events.
type Handler struct {
	client.Client
	log       logr.Logger
	namespace string
}

// NewHandler creates a new webhook handler. The namespace is used as the
// default when the request does not include a ?namespace= query parameter.
func NewHandler(k8sClient client.Client, namespace string, logger logr.Logger) *Handler {
	return &Handler{
		Client:    k8sClient,
		log:       logger,
		namespace: namespace,
	}
}

// PushEvent represents a generic git push event (simplified from GitHub/GitLab/Forgejo schemas).
type PushEvent struct {
	Ref        string `json:"ref"` // e.g., "refs/heads/main"
	Repository struct {
		URL      string `json:"url"`       // clone URL
		HTMLURL  string `json:"html_url"`  // web URL
		FullName string `json:"full_name"` // org/repo
	} `json:"repository"`
	After  string `json:"after"`  // new HEAD commit SHA
	Before string `json:"before"` // old HEAD commit SHA (0000000000000000000000000000000000000000 = new branch)
}

// PullRequestEvent is the GitHub-consolidated pull_request event (GitHub
// and Forgejo share the payload shape; the action field dispatches).
// Only the fields the dumb handler needs are captured — the Review-Ready
// Gate re-verifies everything against the API at evaluation time.
type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Head   struct {
			Sha string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
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
	namespace := req.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = h.namespace
	}
	var wf v1alpha1.Workflow
	if err := h.Get(req.Context(), types.NamespacedName{Namespace: namespace, Name: workflowName}, &wf); err != nil {
		h.log.Error(err, "workflow not found", "workflow", workflowName)
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	// Resolve secret from secretRef or direct secret field
	var secretValue string
	if wf.Spec.Source.Webhook != nil {
		// Production mode: read from Kubernetes Secret
		if wf.Spec.Source.Webhook.SecretRef != nil {
			var secret corev1.Secret
			if err := h.Get(req.Context(), types.NamespacedName{
				Namespace: namespace,
				Name:      wf.Spec.Source.Webhook.SecretRef.Name,
			}, &secret); err != nil {
				h.log.Error(err, "failed to read webhook secret", "secret", wf.Spec.Source.Webhook.SecretRef.Name)
				http.Error(w, "failed to read webhook secret", http.StatusBadGateway)
				return
			}
			secretValue = string(secret.Data[wf.Spec.Source.Webhook.SecretRef.Key])
			if secretValue == "" {
				h.log.Info("webhook secret key is empty", "secret", wf.Spec.Source.Webhook.SecretRef.Name, "key", wf.Spec.Source.Webhook.SecretRef.Key)
				http.Error(w, "webhook secret key is empty", http.StatusInternalServerError)
				return
			}
		} else {
			// Testing/legacy mode: direct secret value (NOT recommended)
			secretValue = wf.Spec.Source.Webhook.Secret
		}

		if secretValue != "" && wf.Spec.Source.Webhook.URL != "" {
			// GitHub: X-Hub-Signature-256: sha256=...
			// GitLab: X-Gitlab-Token (secret)
			// Forgejo: X-Forgejo-Signature: sha256=...
			if !h.verifySignature(req, body, wf.Spec.Source.Webhook.URL, secretValue) {
				h.log.Info("webhook signature verification failed", "workflow", workflowName)
				http.Error(w, "signature verification failed", http.StatusUnauthorized)
				return
			}
		}
	}

	// Parse: git push event (branch/revision trigger) or consolidated
	// pull_request event (Review-Ready Gate arming, ADR-0006).
	var probe struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Action != "" {
		var pre PullRequestEvent
		if err := json.Unmarshal(body, &pre); err != nil {
			h.log.Error(err, "failed to parse pull_request event")
			http.Error(w, "invalid event payload", http.StatusBadRequest)
			return
		}
		h.servePullRequest(w, req, &wf, pre)
		return
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

// pullRequestWakeActions are the consolidated actions that wake the
// Review-Ready Gate. Everything else (assigned, review_requested, …) is a
// no-op: 200, no annotations.
var pullRequestWakeActions = map[string]bool{
	"labeled":          true, // a human or the skill set a label — arm
	"unlabeled":        true, // label removed (also the post-review consume) — re-evaluate
	"synchronize":      true, // new push — head moved, re-arm at new SHA
	"opened":           true,
	"reopened":         true,
	"closed":           true, // disarm path — the gate stands down promptly
	"ready_for_review": true,
}

// servePullRequest handles a consolidated pull_request event. The handler
// stays DUMB: verify → parse → annotate. No business logic lives here — the
// Review-Ready Gate (internal/review, executed by the worker) re-verifies
// label presence and CI greenness at evaluation time, so duplicate or early
// events are free.
func (h *Handler) servePullRequest(w http.ResponseWriter, req *http.Request, wf *v1alpha1.Workflow, pre PullRequestEvent) {
	if !pullRequestWakeActions[pre.Action] {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "action %q ignored\n", pre.Action)
		return
	}
	prNum := pre.Number
	if prNum == 0 {
		prNum = pre.PullRequest.Number
	}
	if prNum == 0 || pre.PullRequest.Head.Sha == "" {
		h.log.Info("pull_request event missing number/head", "action", pre.Action)
		http.Error(w, "missing number or head sha", http.StatusBadRequest)
		return
	}
	if pre.Repository.FullName == "" {
		http.Error(w, "missing repository full_name", http.StatusBadRequest)
		return
	}

	// The repo as the platform configures it: host/owner/name. The host comes
	// from the payload URL (GitHub and Forgejo both fill html_url), not from
	// a hard-coded list.
	repo := normalizeRepo(pre.Repository.HTMLURL, pre.Repository.FullName)

	base := wf.DeepCopy()
	if wf.Annotations == nil {
		wf.Annotations = make(map[string]string)
	}
	wf.Annotations[TriggerRevisionAnnotation] = pre.PullRequest.Head.Sha
	wf.Annotations[TriggerPRAnnotation] = fmt.Sprintf("%s#%d", repo, prNum)
	wf.Annotations[TriggerActionAnnotation] = pre.Action

	if err := h.Patch(req.Context(), wf, client.MergeFrom(base)); err != nil {
		h.log.Error(err, "failed to annotate workflow (pull_request)", "workflow", wf.Name, "pr", prNum)
		http.Error(w, "failed to trigger workflow", http.StatusInternalServerError)
		return
	}
	h.log.Info("webhook armed workflow (pull_request)", "workflow", wf.Name, "pr", prNum, "action", pre.Action, "head", pre.PullRequest.Head.Sha[:12])
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "workflow %s armed for %s#%d (%s)\n", wf.Name, repo, prNum, pre.Action)
}

// normalizeRepo turns the payload's html_url + full_name into the platform
// repo path convention (host/owner/name; GitHub two-segment paths stay
// owner/name-compatible by keeping the github.com host explicitly).
func normalizeRepo(htmlURL, fullName string) string {
	host := ""
	if u := strings.TrimPrefix(htmlURL, "https://"); u != htmlURL {
		host = strings.SplitN(u, "/", 2)[0]
	} else if u := strings.TrimPrefix(htmlURL, "http://"); u != htmlURL {
		host = strings.SplitN(u, "/", 2)[0]
	}
	if host == "" || host == "github.com" {
		// Keep github.com explicit so the gate's host resolution is uniform;
		// a bare owner/name also resolves to GitHub, but the annotation should
		// carry exactly what the config compares against.
		return "github.com/" + fullName
	}
	return host + "/" + fullName
}

// verifySignature verifies the webhook request's authenticity by the HEADER
// the host actually sent (GitHub X-Hub-Signature-256, Forgejo
// X-Forgejo-Signature — both HMAC-SHA256 of the body; GitLab X-Gitlab-Token
// — shared secret), falling back to URL sniffing when no header matches.
// Header-first matters for self-hosted Forgejo whose hostname does not
// contain "forgejo".
func (h *Handler) verifySignature(req *http.Request, body []byte, hostURL, secret string) bool {
	if sig := req.Header.Get("X-Hub-Signature-256"); sig != "" {
		return verifyHMACSHA256(sig, "sha256=", body, secret)
	}
	if sig := req.Header.Get("X-Forgejo-Signature"); sig != "" {
		return verifyHMACSHA256(sig, "sha256=", body, secret)
	}
	if tok := req.Header.Get("X-Gitlab-Token"); tok != "" {
		return tok == secret
	}
	// No signature header and a secret is configured: fail closed. Legacy
	// tolerance for self-hosted unsigned hooks let forged wakes reach the
	// gate (bounded by repoInScope, but pointless risk — the gate re-verifies
	// via the authenticated API anyway).
	h.log.Info("unsigned webhook rejected (secret configured, no signature header)", "url", hostURL)
	return false
}

func verifyHMACSHA256(header, prefix string, body []byte, secret string) bool {
	sig := strings.TrimPrefix(header, prefix)
	if len(sig) != 64 { // sha256 hex = 64 chars
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}
