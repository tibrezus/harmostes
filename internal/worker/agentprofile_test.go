package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Phase 1a (ADR-0011 amendment 2026-10-26): the worker's AgentProfile read —
// fetch with a named miss, and the profile-first resolution chain.

func TestFetchAgentProfileNamedMiss(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := FetchAgentProfile(context.Background(), cl, "harmostes", "ghost")
	if err == nil || !strings.Contains(err.Error(), "get agentprofile harmostes/ghost") {
		t.Fatalf("missing profile must name the object in the error (#504), got %v", err)
	}
	present := &v1alpha1.AgentProfile{ObjectMeta: metav1.ObjectMeta{Name: "fleet", Namespace: "harmostes"}}
	cl = fake.NewClientBuilder().WithScheme(scheme).WithObjects(present).Build()
	got, err := FetchAgentProfile(context.Background(), cl, "harmostes", "fleet")
	if err != nil || got.Name != "fleet" {
		t.Fatalf("FetchAgentProfile = (%v, %v)", got, err)
	}
}

func TestResolveModelWithProfileChain(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 30, 0, 0, time.UTC) // 18:30 Europe/Paris
	wf := &v1alpha1.Workflow{}
	wf.Spec.Agent.Model = "base-model"
	wf.Spec.Agent.Models = []v1alpha1.ModelWindow{{Model: "spec-window", Start: "00:00", End: "23:59", TZ: "UTC"}}

	// nil profile → the spec decides alone (inert default).
	if got := ResolveModelWithProfile(nil, wf, now); got != "spec-window" {
		t.Fatalf("nil profile: got %q, want spec-window", got)
	}
	// Profile window matched → wins over the spec entirely.
	hit := &v1alpha1.AgentProfile{Spec: v1alpha1.AgentProfileSpec{
		Models: []v1alpha1.ModelWindow{{Model: "profile-model", Start: "16:00", End: "02:00", TZ: "Europe/Paris"}}}}
	if got := ResolveModelWithProfile(hit, wf, now); got != "profile-model" {
		t.Fatalf("profile hit: got %q, want profile-model", got)
	}
	// Profile missed → template fallback (spec windows, then base).
	miss := &v1alpha1.AgentProfile{Spec: v1alpha1.AgentProfileSpec{
		Models: []v1alpha1.ModelWindow{{Model: "profile-model", Start: "03:00", End: "04:00", TZ: "UTC"}}}}
	if got := ResolveModelWithProfile(miss, wf, now); got != "spec-window" {
		t.Fatalf("profile miss: got %q, want spec-window (template fallback)", got)
	}
	// Profile missed AND spec windowless → the spec's base model.
	bare := &v1alpha1.Workflow{}
	bare.Spec.Agent.Model = "bare-base"
	if got := ResolveModelWithProfile(miss, bare, now); got != "bare-base" {
		t.Fatalf("profile miss, windowless spec: got %q, want bare-base", got)
	}
}

// The env knob is the deployment's ONLY wiring: unset = inert.
func TestAgentProfileRefFromEnv(t *testing.T) {
	if got := v1alpha1.AgentProfileRefFromEnv(func(string) string { return "" }); got != "" {
		t.Fatalf("unset env: got %q, want empty", got)
	}
	if got := v1alpha1.AgentProfileRefFromEnv(func(k string) string { return "fleet" }); got != "fleet" {
		t.Fatalf("wired env: got %q, want fleet", got)
	}
}
