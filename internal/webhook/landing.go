// landing.go — template landing events (three-tier 1c, ADR-0011 amendment):
// a signed push to a template's OWN repo correlates the git delivery with
// the on-cluster revision history. The webhook handler proves the push
// (HMAC via the template's source.secretRef — absent secretRef = the
// template accepts NO landings, fail closed), matches repo + branch against
// spec.source, and stashes the commit identity as a PENDING annotation on
// the template CR; the history recorder consumes it when it appends the
// revision the push delivered (the webhook races Flux and usually wins —
// the annotation bridges the gap durably, replica-safe).
package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// ServeTemplateLanding handles POST /webhook/templates/{name}: a push to
// the template's own repo, recorded as pending landing identity.
func (h *Handler) ServeTemplateLanding(w http.ResponseWriter, req *http.Request, templateName string) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer func() { _ = req.Body.Close() }()

	namespace := req.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = h.namespace
	}
	var tmpl v1alpha1.WorkflowTemplate
	if err := h.Get(req.Context(), types.NamespacedName{Namespace: namespace, Name: templateName}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, "template not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to read template", http.StatusInternalServerError)
		return
	}
	src := tmpl.Spec.Source
	if src == nil {
		// No declared repo: nothing to correlate. Not an error — the
		// environment may fire one repo-wide webhook at every template.
		http.Error(w, "template declares no source", http.StatusNotFound)
		return
	}
	// Fail closed: without a secretRef there is no way to prove the push.
	if src.SecretRef == nil {
		h.log.Info("template landing refused: no secretRef on the declared source", "template", templateName)
		http.Error(w, "template source carries no webhook secret", http.StatusUnauthorized)
		return
	}
	var secret corev1.Secret
	if err := h.Get(req.Context(), types.NamespacedName{Namespace: namespace, Name: src.SecretRef.Name}, &secret); err != nil {
		h.log.Error(err, "failed to read template landing secret", "template", templateName, "secret", src.SecretRef.Name)
		http.Error(w, "failed to read webhook secret", http.StatusBadGateway)
		return
	}
	secretValue := string(secret.Data[src.SecretRef.Key])
	if secretValue == "" {
		http.Error(w, "webhook secret key is empty", http.StatusInternalServerError)
		return
	}
	// verifySignature keys its header conventions off the host URL string;
	// the forge field selects the same dialects.
	if !h.verifySignature(req, body, src.Forge, secretValue) {
		h.log.Info("template landing signature verification failed", "template", templateName)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	var push PushEvent
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "invalid event payload", http.StatusBadRequest)
		return
	}
	// The landing must be THIS repo and branch — a repo-wide webhook fires
	// for every branch and every other template living in the same repo.
	branch := src.Branch
	if branch == "" {
		branch = "main"
	}
	if !strings.EqualFold(push.Repository.FullName, src.Repo) || push.Ref != "refs/heads/"+branch {
		h.log.V(1).Info("template landing out of scope", "template", templateName, "repo", push.Repository.FullName, "ref", push.Ref)
		http.Error(w, "push does not match the declared source", http.StatusBadRequest)
		return
	}
	if push.After == "" || push.After == "0000000000000000000000000000000000000000" {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}

	landing := v1alpha1.TemplateRevisionLanding{
		Sha:    push.After,
		Pusher: push.Pusher.Name,
		At:     time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(landing)
	if err != nil { // strings only; unreachable
		http.Error(w, "encode landing", http.StatusInternalServerError)
		return
	}
	// Patch ONLY the annotation: MergeFrom the fetched object (old pending
	// value included in the base) — the patch replaces the one annotation
	// and touches nothing else, so a concurrent spec delivery from Flux
	// cannot be clobbered.
	base := tmpl.DeepCopy()
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[v1alpha1.TemplatePendingLandingAnnotation] = string(raw)
	if err := h.Patch(req.Context(), &tmpl, client.MergeFrom(base)); err != nil {
		h.log.Error(err, "failed to stash pending landing", "template", templateName)
		http.Error(w, "failed to record landing", http.StatusInternalServerError)
		return
	}
	h.log.Info("template landing recorded", "template", templateName, "sha", landing.Sha, "pusher", landing.Pusher)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"landing recorded"}`))
}
