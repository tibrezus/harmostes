package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// workflowNameRe restricts Workflow CR names to DNS-compatible identifiers.
// Prevents path traversal and ensures k8s naming compliance.
var workflowNameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxWorkflowNameLen = 63

// triggerAnnotation is the annotation that the controller checks for manual/webhook runs.
// Setting it to a value that differs from status.lastProcessedRevision triggers a run.
const triggerAnnotation = "harmostes.dev/trigger-revision"

// handleWorkflowNew renders the create form. It loads the user's tokens (for
// the template catalog (WorkflowTemplate CRs — the reusable pipeline
// shapes). This is the only sanctioned creation surface: the owner label is
// stamped from the authenticated identity, so a created workflow is always
// visible to its creator in the UI. (GitOps/YAML creation was removed — it
// could produce workflows no one could see.)
func (s *Server) handleWorkflowNew(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username

	templates, err := s.listTemplates(r)
	if err != nil {
		s.logger.Error("list templates for workflow form", "owner", owner, "err", err)
		s.renderError(w, r, "Failed to load templates: "+err.Error())
		return
	}

	s.render(w, r, "pages/workflow_new.html", map[string]any{
		"Templates":        templates,
		"SelectedTemplate": r.URL.Query().Get("template"),
	})
}

// resolveWorkflow returns wf with its effective spec: when spec.templateRef
// names a WorkflowTemplate, the template defaults are overlaid (instance-set
// fields win; spec.config overlays prepare.config). Read paths (list
// grouping, detail, graph) render the merged shape while the stored CR stays
// thin. A missing template degrades to the thin spec (rendered as-is).
func (s *Server) resolveWorkflow(ctx context.Context, wf *v1alpha1.Workflow) v1alpha1.Workflow {
	if wf.Spec.TemplateRef == "" {
		return *wf
	}
	var tmpl v1alpha1.WorkflowTemplate
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: wf.Spec.TemplateRef}, &tmpl); err != nil {
		return *wf
	}
	merged := wf.DeepCopy()
	v1alpha1.ApplyTemplateDefaults(merged, &tmpl)
	return *merged
}

// scopeConfigJSON builds the instance's spec.config from the form, using
// ONLY the parameters the selected template declares (spec.scope). The
// template owns its configuration dialect end-to-end: the form renders from
// the same declaration, and keys the template does not declare are never
// stored. Defaults apply when a field is left empty.
func scopeConfigJSON(r *http.Request, tmpl *v1alpha1.WorkflowTemplate) ([]byte, error) {
	cfg := map[string]any{}
	for _, p := range tmpl.Spec.Scope {
		switch p.Kind {
		case "list":
			items := []string{}
			for _, v := range strings.Split(r.FormValue(p.Name), ",") {
				if v = strings.TrimSpace(v); v != "" {
					items = append(items, v)
				}
			}
			if len(items) == 0 && p.Default != "" {
				for _, v := range strings.Split(p.Default, ",") {
					if v = strings.TrimSpace(v); v != "" {
						items = append(items, v)
					}
				}
			}
			cfg[p.Name] = items
		default: // "string" (and undeclared kinds degrade to string)
			v := strings.TrimSpace(r.FormValue(p.Name))
			if v == "" {
				v = p.Default
			}
			cfg[p.Name] = v
		}
	}
	return json.Marshal(cfg)
}

// handleWorkflowCreate handles POST /workflows — the ONLY creation form:
// a thin template instance (name + templateRef + schedule + the template's
// declared scope params). The legacy gate-archetype branch is retired —
// WorkflowTemplate CRs are the single archetype registry.
//
// The owner label is stamped via StampOwnerLabel (anti-spoof — the server
// sets it from the authenticated identity, never from client input), so
// every created workflow is visible to its creator.
func (s *Server) handleWorkflowCreate(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username

	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form data")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	schedule := strings.TrimSpace(r.FormValue("schedule"))
	if schedule == "" {
		schedule = "*/30 * * * *"
	}

	// Validation
	if name == "" {
		s.renderError(w, r, "Workflow name is required")
		return
	}
	if !workflowNameRe.MatchString(name) || len(name) > maxWorkflowNameLen {
		s.renderError(w, r, "Invalid workflow name: must be lowercase, alphanumeric with hyphens, max 63 characters")
		return
	}

	templateRef := strings.TrimSpace(r.FormValue("templateRef"))
	if templateRef == "" {
		s.renderError(w, r, "A template must be selected — workflows are template instances (the gate-archetype form was retired)")
		return
	}

	var tmpl v1alpha1.WorkflowTemplate
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: templateRef}, &tmpl); err != nil {
		s.renderError(w, r, "Unknown template: "+templateRef)
		return
	}

	cfg, err := scopeConfigJSON(r, &tmpl)
	if err != nil {
		s.renderError(w, r, "Failed to build config: "+err.Error())
		return
	}

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			TemplateRef: templateRef,
			Source:      v1alpha1.SourceSpec{Kind: "schedule", Schedule: schedule},
			Config:      cfg,
		},
	}
	StampOwnerLabel(wf, owner)
	if err := s.k8sClient.Create(r.Context(), wf); err != nil {
		if errors.IsAlreadyExists(err) {
			s.renderError(w, r, "A workflow with that name already exists")
			return
		}
		s.logger.Error("create workflow", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to create workflow: "+err.Error())
		return
	}
	s.logger.Info("workflow created (template instance)", "owner", owner, "name", name, "template", templateRef)
	http.Redirect(w, r, "/workflows/"+name, http.StatusSeeOther)
}

// handleWorkflowDelete removes a Workflow CR. It verifies the owner label
// matches before deleting — a user cannot delete another user's workflow.
func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !workflowNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	// Fetch + verify ownership
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &wf); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.renderError(w, r, "Failed to load workflow: "+err.Error())
		return
	}

	if wf.Labels[v1alpha1.OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	// Check for active jobs — don't allow deletion while a worker is running.
	active, err := s.hasActiveJob(r, &wf)
	if err != nil {
		s.logger.Error("check active job", "workflow", name, "err", err)
	}
	if active {
		s.renderError(w, r, "Cannot delete: a worker is currently running for this workflow. Wait for it to finish.")
		return
	}

	if err := s.k8sClient.Delete(r.Context(), &wf); err != nil {
		s.logger.Error("delete workflow", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to delete workflow: "+err.Error())
		return
	}

	s.logger.Info("workflow deleted", "owner", owner, "name", name)
	http.Redirect(w, r, "/workflows", http.StatusSeeOther)
}

// handleWorkflowTrigger sets the trigger-revision annotation to force an
// immediate run. The annotation value includes a timestamp so it always
// differs from status.lastProcessedRevision.
func (s *Server) handleWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !workflowNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &wf); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.renderError(w, r, "Failed to load workflow: "+err.Error())
		return
	}

	if wf.Labels[v1alpha1.OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	// Don't trigger if disabled or already running
	if wf.Spec.Disabled {
		s.renderError(w, r, "Cannot trigger: workflow is disabled")
		return
	}
	active, err := s.hasActiveJob(r, &wf)
	if err != nil {
		s.logger.Error("check active job", "workflow", name, "err", err)
	}
	if active {
		s.renderError(w, r, "Cannot trigger: a worker is already running for this workflow")
		return
	}

	// Patch the trigger annotation. The value includes a timestamp so it
	// always differs from status.lastProcessedRevision, forcing a run.
	triggerValue := fmt.Sprintf("manual-%d", time.Now().Unix())

	base := wf.DeepCopy()
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations[triggerAnnotation] = triggerValue

	if err := s.k8sClient.Patch(r.Context(), &wf, client.MergeFrom(base)); err != nil {
		s.logger.Error("patch trigger annotation", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to trigger workflow: "+err.Error())
		return
	}

	s.logger.Info("workflow triggered", "owner", owner, "name", name)
	http.Redirect(w, r, "/workflows/"+name, http.StatusSeeOther)
}

// handleWorkflowToggle enables/disables a workflow by patching spec.disabled.
func (s *Server) handleWorkflowToggle(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context()).Username
	name := r.PathValue("name")
	if name == "" || !workflowNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: s.namespace, Name: name}, &wf); err != nil {
		if errors.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		s.renderError(w, r, "Failed to load workflow: "+err.Error())
		return
	}

	if wf.Labels[v1alpha1.OwnerLabel] != owner {
		http.NotFound(w, r)
		return
	}

	base := wf.DeepCopy()
	wf.Spec.Disabled = !wf.Spec.Disabled

	if err := s.k8sClient.Patch(r.Context(), &wf, client.MergeFrom(base)); err != nil {
		s.logger.Error("patch disabled flag", "owner", owner, "name", name, "err", err)
		s.renderError(w, r, "Failed to toggle workflow: "+err.Error())
		return
	}

	s.logger.Info("workflow toggled", "owner", owner, "name", name, "disabled", wf.Spec.Disabled)
	http.Redirect(w, r, "/workflows/"+name, http.StatusSeeOther)
}

// hasActiveJob reports whether a non-terminal worker Job exists for the workflow.
func (s *Server) hasActiveJob(r *http.Request, wf *v1alpha1.Workflow) (bool, error) {
	jobs, err := s.listJobs(r, wf.Name, identityFromContext(r.Context()).Username)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Status.Succeeded == 0 && j.Status.Failed == 0 {
			return true, nil
		}
	}
	return false, nil
}
