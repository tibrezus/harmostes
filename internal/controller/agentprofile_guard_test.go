package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The AgentProfile compatibility guard (ADR-0011 amendment 2026-10-26,
// phase 1a): a wired profile whose statement the running kernel does not
// satisfy (or whose statement is malformed, or which is wired but absent)
// refuses dispatch loudly — named cause on a warning Event, trigger slot NOT
// consumed. Unset env never refuses.

func TestProfileCompat(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	newCl := func(objs ...*v1alpha1.AgentProfile) *fake.ClientBuilder {
		b := fake.NewClientBuilder().WithScheme(scheme)
		for _, o := range objs {
			b = b.WithObjects(o)
		}
		return b
	}
	r := &WorkflowReconciler{}

	tests := []struct {
		name    string
		env     string
		profile *v1alpha1.AgentProfile
		version string
		want    bool
		wantSub string // substring the cause must carry ("" when not blocked)
		// (r.KernelVersion is set from tc.version per case)
	}{
		{
			name: "no env: inert, never refuses",
			env:  "",
			want: false,
		},
		{
			name: "satisfied statement passes",
			env:  "fleet",
			profile: &v1alpha1.AgentProfile{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "harmostes"},
				Spec: v1alpha1.AgentProfileSpec{Compatibility: "kernel >=1.4.0"}},
			version: "1.4.1",
			want:    false,
		},
		{
			name: "unsatisfied statement refuses, naming the statement",
			env:  "fleet",
			profile: &v1alpha1.AgentProfile{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "harmostes"},
				Spec: v1alpha1.AgentProfileSpec{Compatibility: "kernel >=1.5.0"}},
			version: "1.4.1",
			want:    true,
			wantSub: "kernel >=1.5.0",
		},
		{
			name: "malformed statement refuses loudly",
			env:  "fleet",
			profile: &v1alpha1.AgentProfile{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "harmostes"},
				Spec: v1alpha1.AgentProfileSpec{Compatibility: "kernel >latest"}},
			version: "1.4.1",
			want:    true,
			wantSub: "malformed",
		},
		{
			name:    "wired but absent profile refuses",
			env:     "ghost",
			version: "1.4.1",
			want:    true,
			wantSub: "cannot be read",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1alpha1.AgentProfileEnvVar, tc.env)
			var objs []*v1alpha1.AgentProfile
			if tc.profile != nil {
				objs = append(objs, tc.profile)
			}
			r.Client = newCl(objs...).Build()
			r.KernelVersion = tc.version
			blocked, cause := r.profileCompat(context.Background(), "harmostes")
			if blocked != tc.want {
				t.Fatalf("blocked = %v (cause %q), want %v", blocked, cause, tc.want)
			}
			if tc.wantSub != "" && !strings.Contains(cause, tc.wantSub) {
				t.Fatalf("cause %q must contain %q", cause, tc.wantSub)
			}
		})
	}
}

// Reconcile-level: a DUE workflow under an incompatible profile is refused —
// the warning Event fires with the named cause and the trigger slot stays
// unconsumed (LastRunAt untouched), so a later profile repair re-fires the
// run without any watermark surgery.
func TestReconcileRefusedByIncompatibleProfile(t *testing.T) {
	t.Setenv(v1alpha1.AgentProfileEnvVar, "fleet")
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	profile := &v1alpha1.AgentProfile{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "harmostes"},
		Spec: v1alpha1.AgentProfileSpec{Compatibility: "kernel >=99.0.0"}}
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "due-wf", Namespace: "harmostes"},
	}
	wf.Generation = 2
	wf.Status.ObservedGeneration = 1 // spec-change → due
	rec := record.NewFakeRecorder(8)
	r := &WorkflowReconciler{
		Client:        fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, wf).WithStatusSubresource(&v1alpha1.Workflow{}).Build(),
		Scheme:        scheme,
		Recorder:      rec,
		PollInterval:  1,
		KernelVersion: "1.4.1",
		DaprClient:    nil, // publish would no-op anyway — the assertions below carry the weight
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "harmostes", Name: "due-wf"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("refused reconcile must requeue, got %+v", res)
	}
	select {
	case e := <-rec.Events:
		if !strings.Contains(e, "AgentProfileIncompatible") || !strings.Contains(e, "kernel >=99.0.0") {
			t.Fatalf("event %q must name the reason and the statement", e)
		}
	default:
		t.Fatal("expected a warning Event naming the refusal — the refusal must be LOUD")
	}
	var fresh v1alpha1.Workflow
	_ = r.Get(context.Background(), client.ObjectKey{Namespace: "harmostes", Name: "due-wf"}, &fresh)
	if !fresh.Status.LastRunAt.IsZero() {
		t.Fatalf("trigger slot must NOT be consumed by a refused dispatch, LastRunAt = %v", fresh.Status.LastRunAt)
	}
}
