package ui

import (
	"net/http"
	"strconv"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// Workflow lifecycle routes (ADR-0012 §7 — run-with-inputs, the rest of the
// invariant: created ⇒ visible ⇒ OPERABLE). #427 restored composition
// (create); #418 restores the remaining verbs:
//
//	POST /workflows/{name}/enable   — arm the instance (Disabled=false)
//	POST /workflows/{name}/disable  — pause it (Disabled=true)
//	POST /workflows/{name}/trigger  — one run now (the wake annotation)
//	POST /workflows/{name}/delete   — remove the instance
//
// Every route walks the same guard chain (operableWorkflow): write-capable
// identity, then owner isolation. Ownership is enforced HERE, not by k8s —
// the UI service account legitimately holds namespace-wide verbs, so RBAC
// cannot distinguish operators; the visibleOwner filter the read paths
// apply is the same filter the write paths apply. Foreign and missing
// objects return identical 404s (existence is not leaked), and every
// outcome lands in harmostes_ui_writes_total (#436) — rejections are no
// longer log-only.

// operableWorkflow loads the named workflow through the lifecycle guard
// chain. On rejection it has already written the HTTP error and recorded
// the writes counter; the caller just stops. ok=false means "stop".
func (s *Server) operableWorkflow(w http.ResponseWriter, r *http.Request, action string) (*v1alpha1.Workflow, bool) {
	id := identityFromContext(r.Context())
	if !s.mayWrite(id) {
		if id == nil {
			s.logger.Warn("write rejected — no identity in context", "action", action)
		} else {
			s.logger.Warn("write rejected — identity provenance", "action", action, "user", id.Username, "dev", id.Dev)
		}
		// Same generic body as the create handler: no signal about WHICH
		// provenance check failed or how the server is configured.
		recordWrite(action, "forbidden")
		http.Error(w, "403 Forbidden — this identity may not take write actions", http.StatusForbidden)
		return nil, false
	}

	name := r.PathValue("name")
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &wf); err != nil {
		recordWrite(action, "error")
		s.renderErrorStatus(w, r, http.StatusNotFound, "Workflow not found: "+name)
		return nil, false
	}

	// Owner isolation — the detail page's rule, applied to writes: a
	// workflow this identity could not see is one it must not operate
	// blind. Admins keep the same deliberate exception (#324): system
	// workflows are exactly what an operator tends on an incident.
	owner := s.visibleOwner(id)
	if wf.Labels[v1alpha1.OwnerLabel] != owner && !s.isAdmin(id) {
		recordWrite(action, "forbidden")
		s.renderErrorStatus(w, r, http.StatusNotFound, "Workflow not found: "+name)
		return nil, false
	}
	return &wf, true
}

// handleWorkflowEnable arms the instance — the deliberate act creation
// deliberately does not perform (PR #427 review R1): arming is composition's
// opposite number, a separate button, never a side effect.
func (s *Server) handleWorkflowEnable(w http.ResponseWriter, r *http.Request) {
	s.toggleWorkflow(w, r, "enable", false)
}

// handleWorkflowDisable pauses the instance — schedule dues and wakes stop
// at the controller's Disabled gate (workflow_controller reconcile returns
// early); the CR and its history stay.
func (s *Server) handleWorkflowDisable(w http.ResponseWriter, r *http.Request) {
	s.toggleWorkflow(w, r, "disable", true)
}

func (s *Server) toggleWorkflow(w http.ResponseWriter, r *http.Request, action string, disabled bool) {
	wf, ok := s.operableWorkflow(w, r, action)
	if !ok {
		return
	}
	if wf.Spec.Disabled == disabled {
		// Idempotent by design: the button and the state agreeing is not an
		// error, just a no-op round-trip.
		http.Redirect(w, r, "/workflows/"+wf.Name, http.StatusSeeOther)
		return
	}
	base := wf.DeepCopy()
	wf.Spec.Disabled = disabled
	if err := s.k8sClient.Patch(r.Context(), wf, client.MergeFrom(base)); err != nil {
		recordWrite(action, "error")
		s.logger.Error("toggle workflow", "action", action, "name", wf.Name, "err", err)
		s.renderErrorStatus(w, r, http.StatusInternalServerError, "Failed to update workflow")
		return
	}
	recordWrite(action, "created")
	s.logger.Info("workflow toggled", "action", action, "name", wf.Name, "owner", wf.Labels[v1alpha1.OwnerLabel])
	http.Redirect(w, r, "/workflows/"+wf.Name, http.StatusSeeOther)
}

// handleWorkflowTrigger requests one run now by setting the wake annotation
// — the same contract the webhook sink uses (TriggerRevisionAnnotation):
// the next controller reconcile sees an unprocessed value, publishes the
// trigger to the worker pool over Dapr, clears the annotation, and records
// lastProcessedRevision. The manual- prefix makes the recorded trigger type
// "manual" instead of "webhook" — the timeline tells the truth about who
// asked. Disabled instances reject the trigger with 409: the controller
// ignores Disabled workflows entirely, so the annotation would sit armed
// until some later enable fired it — a surprising deferred run nobody asked
// for (the button is likewise not rendered while paused).
func (s *Server) handleWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	const action = "trigger"
	wf, ok := s.operableWorkflow(w, r, action)
	if !ok {
		return
	}
	if wf.Spec.Disabled {
		recordWrite(action, "error")
		s.renderErrorStatus(w, r, http.StatusConflict, "Workflow is paused — arm it before triggering")
		return
	}
	base := wf.DeepCopy()
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations[v1alpha1.TriggerRevisionAnnotation] = v1alpha1.ManualTriggerPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := s.k8sClient.Patch(r.Context(), wf, client.MergeFrom(base)); err != nil {
		recordWrite(action, "error")
		s.logger.Error("trigger workflow", "name", wf.Name, "err", err)
		s.renderErrorStatus(w, r, http.StatusInternalServerError, "Failed to trigger workflow")
		return
	}
	recordWrite(action, "created")
	s.logger.Info("workflow triggered (manual wake)", "name", wf.Name, "owner", wf.Labels[v1alpha1.OwnerLabel])
	http.Redirect(w, r, "/workflows/"+wf.Name, http.StatusSeeOther)
}

// handleWorkflowDelete removes the instance CR. The object is composition,
// not state: templates live in chart values / git (ADR-0011), and the runs
// history lives on Attempts and the timeline store, so deletion is a
// catalog operation, not a data destroy.
func (s *Server) handleWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	const action = "delete"
	wf, ok := s.operableWorkflow(w, r, action)
	if !ok {
		return
	}
	if err := s.k8sClient.Delete(r.Context(), wf); err != nil {
		recordWrite(action, "error")
		s.logger.Error("delete workflow", "name", wf.Name, "err", err)
		s.renderErrorStatus(w, r, http.StatusInternalServerError, "Failed to delete workflow")
		return
	}
	recordWrite(action, "created")
	s.logger.Info("workflow deleted", "name", wf.Name, "owner", wf.Labels[v1alpha1.OwnerLabel])
	http.Redirect(w, r, "/workflows", http.StatusSeeOther)
}
