package controller

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func historyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return scheme
}

func templateWithSpec(name string, model string, ann map[string]string) *v1alpha1.WorkflowTemplate {
	tmpl := &v1alpha1.WorkflowTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns", Annotations: ann},
	}
	tmpl.Spec.Agent.Model = model
	tmpl.Spec.Deploy.Plugin.Name = "noop"
	return tmpl
}

func reconcileTemplate(t *testing.T, cl client.Client, name string) error {
	t.Helper()
	r := &TemplateHistoryReconciler{Client: cl}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test-ns", Name: name}})
	return err
}

func storedHistory(t *testing.T, cl client.Client, name string) []v1alpha1.TemplateRevision {
	t.Helper()
	tmpl := &v1alpha1.WorkflowTemplate{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "test-ns", Name: name}, tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	raw := tmpl.Annotations[v1alpha1.TemplateRevisionsAnnotation]
	if raw == "" {
		return nil
	}
	var revs []v1alpha1.TemplateRevision
	if err := json.Unmarshal([]byte(raw), &revs); err != nil {
		t.Fatalf("stored history is not valid annotation JSON: %v", err)
	}
	return revs
}

// TestTemplateHistory_RecordsSpecChange: a spec that differs from the head
// revision is appended (the first change creates rev 1, the next rev 2 —
// the live spec stays the derived head, never duplicated into the list).
func TestTemplateHistory_RecordsSpecChange(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(historyScheme(t)).WithObjects(templateWithSpec("t", "old-model", nil)).Build()
	if err := reconcileTemplate(t, cl, "t"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	revs := storedHistory(t, cl, "t")
	if len(revs) != 1 || revs[0].Rev != 1 || revs[0].Spec.Agent.Model != "old-model" {
		t.Fatalf("first change: history = %+v, want [rev1 spec old-model]", revs)
	}

	// Flux delivers a changed spec: the CR now carries the NEW spec (the
	// recorder never rolls specs back — it records what landed), with rev 1
	// already in the annotation from the first change.
	cl2 := fake.NewClientBuilder().WithScheme(historyScheme(t)).WithObjects(
		templateWithSpec("t", "new-model", map[string]string{
			v1alpha1.TemplateRevisionsAnnotation: mustJSON(t, []v1alpha1.TemplateRevision{{Rev: 1, Spec: templateWithSpec("t", "old-model", nil).Spec}}),
		})).Build()
	if err := reconcileTemplate(t, cl2, "t"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	revs = storedHistory(t, cl2, "t")
	if len(revs) != 2 || revs[1].Rev != 2 || revs[1].Spec.Agent.Model != "new-model" {
		t.Fatalf("second change: history = %+v, want [rev1 old, rev2 new]", revs)
	}
	if revs[0].Spec.Agent.Model != "old-model" {
		t.Errorf("rev 1 must preserve the superseded spec, got model %q", revs[0].Spec.Agent.Model)
	}
}

// TestTemplateHistory_NoOpWhenHeadMatches: an annotation-only update (the
// recorder's own write, or any metadata touch) reconciles and must not
// append — the loop guard compares SPECS, not generations.
func TestTemplateHistory_NoOpWhenHeadMatches(t *testing.T) {
	ann := map[string]string{
		v1alpha1.TemplateRevisionsAnnotation: mustJSON(t, []v1alpha1.TemplateRevision{
			{Rev: 1, Spec: templateWithSpec("t", "same", nil).Spec},
		}),
	}
	tmpl := templateWithSpec("t", "same", ann)
	cl := fake.NewClientBuilder().WithScheme(historyScheme(t)).WithObjects(tmpl).Build()
	if err := reconcileTemplate(t, cl, "t"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	revs := storedHistory(t, cl, "t")
	if len(revs) != 1 {
		t.Fatalf("head-match append: history len = %d, want 1 (no-op)", len(revs))
	}
}

// TestTemplateHistory_BoundedWindow: past MaxTemplateRevisions the oldest
// entries drop and the remaining history renumbers so the ascending
// head = len+1 convention survives truncation.
func TestTemplateHistory_BoundedWindow(t *testing.T) {
	revs := make([]v1alpha1.TemplateRevision, v1alpha1.MaxTemplateRevisions)
	for i := range revs {
		revs[i] = v1alpha1.TemplateRevision{Rev: i + 1, Spec: templateWithSpec("t", string(rune('a'+i)), nil).Spec}
	}
	tmpl := templateWithSpec("t", "newest", map[string]string{
		v1alpha1.TemplateRevisionsAnnotation: mustJSON(t, revs),
	})
	cl := fake.NewClientBuilder().WithScheme(historyScheme(t)).WithObjects(tmpl).Build()
	if err := reconcileTemplate(t, cl, "t"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := storedHistory(t, cl, "t")
	if len(got) != v1alpha1.MaxTemplateRevisions {
		t.Fatalf("history len = %d, want bounded %d", len(got), v1alpha1.MaxTemplateRevisions)
	}
	if got[0].Spec.Agent.Model != "b" {
		t.Errorf("oldest surviving entry = %q, want \"b\" (\"a\" was truncated)", got[0].Spec.Agent.Model)
	}
	if got[len(got)-1].Spec.Agent.Model != "newest" || got[len(got)-1].Rev != v1alpha1.MaxTemplateRevisions {
		t.Errorf("head = %+v, want the delivered spec at Rev %d", got[len(got)-1], v1alpha1.MaxTemplateRevisions)
	}
	for i, r := range got {
		if r.Rev != i+1 {
			t.Fatalf("renumbering broken: revs = %v", revNums(got))
		}
	}
}

// TestTemplateHistory_CorruptAnnotationStartsFresh: a hand-mangled
// annotation must not fail the reconcile — the live spec becomes the next
// recorded head (the full history is git's, the CR only carries a window).
func TestTemplateHistory_CorruptAnnotationStartsFresh(t *testing.T) {
	tmpl := templateWithSpec("t", "m", map[string]string{v1alpha1.TemplateRevisionsAnnotation: "{not json"})
	cl := fake.NewClientBuilder().WithScheme(historyScheme(t)).WithObjects(tmpl).Build()
	if err := reconcileTemplate(t, cl, "t"); err != nil {
		t.Fatalf("reconcile with corrupt annotation: %v", err)
	}
	revs := storedHistory(t, cl, "t")
	if len(revs) != 1 || revs[0].Spec.Agent.Model != "m" {
		t.Fatalf("history = %+v, want fresh [rev1 = live spec]", revs)
	}
}

// TestTemplateHistory_DeletedTemplateIsNoOp: gone is gone — no error, no
// resurrection (the history annotation dies with the CR).
func TestTemplateHistory_DeletedTemplateIsNoOp(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(historyScheme(t)).Build()
	if err := reconcileTemplate(t, cl, "ghost"); err != nil {
		t.Fatalf("reconcile deleted template: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func revNums(revs []v1alpha1.TemplateRevision) []int {
	out := make([]int, len(revs))
	for i, r := range revs {
		out[i] = r.Rev
	}
	return out
}
