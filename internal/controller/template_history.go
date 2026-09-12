/*
Copyright © 2026 tibrezus
*/
package controller

import (
	"context"
	"encoding/json"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// TemplateHistoryReconciler records what git delivered: whenever a
// WorkflowTemplate's spec changes (Flux reconciled a merged MR/chart
// release — ADR-0012 §5), the previous-to-current append lands in the
// TemplateRevisionsAnnotation, feeding the UI's topology diff and version
// switcher. Git is the authoring source of truth; this annotation is the
// system's bounded on-cluster record of it — the UI reads history, it
// never writes templates.
//
// Loop safety: the reconcile writes ONLY the annotation, and the guard
// compares specs, not generations — an annotation-only update reconciles,
// finds the live spec equal to the head revision, and no-ops.
type TemplateHistoryReconciler struct {
	client.Client
}

// Reconcile appends the template's new spec to the revisions annotation.
func (r *TemplateHistoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("template", req.Name)

	tmpl := &v1alpha1.WorkflowTemplate{}
	if err := r.Get(ctx, req.NamespacedName, tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // deleted: history goes with it
		}
		return ctrl.Result{}, err
	}

	revs := parseTemplateRevisions(tmpl.Annotations[v1alpha1.TemplateRevisionsAnnotation])
	if n := len(revs); n > 0 && reflect.DeepEqual(revs[n-1].Spec, tmpl.Spec) {
		return ctrl.Result{}, nil // head already records this spec — nothing landed
	}

	revs = append(revs, v1alpha1.TemplateRevision{
		Rev:  len(revs) + 1,
		Spec: tmpl.Spec,
	})
	if n := len(revs); n > v1alpha1.MaxTemplateRevisions {
		// Bounded window: drop the oldest and renumber so the ascending
		// head convention (Rev = len+1) survives truncation. The full
		// history is the template's git log, not a CR.
		revs = revs[n-v1alpha1.MaxTemplateRevisions:]
		for i := range revs {
			revs[i].Rev = i + 1
		}
	}

	encoded, err := json.Marshal(revs)
	if err != nil {
		return ctrl.Result{}, err // specs are JSON-tagged structs; unreachable in practice
	}
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[v1alpha1.TemplateRevisionsAnnotation] = string(encoded)
	if err := r.Update(ctx, tmpl); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("recorded template revision", "rev", revs[len(revs)-1].Rev)
	return ctrl.Result{}, nil
}

// parseTemplateRevisions decodes the annotation's history; a missing or
// corrupt value means "no known history" — the recorder starts fresh rather
// than failing the reconcile (the live spec always becomes the next head).
func parseTemplateRevisions(raw string) []v1alpha1.TemplateRevision {
	if raw == "" {
		return nil
	}
	var revs []v1alpha1.TemplateRevision
	if err := json.Unmarshal([]byte(raw), &revs); err != nil {
		return nil
	}
	return revs
}

// SetupWithManager registers the reconciler on template events.
func (r *TemplateHistoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkflowTemplate{}).
		Complete(r)
}
