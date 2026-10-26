package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FetchAgentProfile fetches the namespace's AgentProfile. A configured-but-
// absent profile is a misconfiguration and returns a NAMED error (#504): the
// callers refuse loudly instead of degrading to no-profile silently.
func FetchAgentProfile(ctx context.Context, cl client.Client, namespace, name string) (*v1alpha1.AgentProfile, error) {
	var p v1alpha1.AgentProfile
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &p); err != nil {
		return nil, fmt.Errorf("get agentprofile %s/%s: %w", namespace, name, err)
	}
	return &p, nil
}

// ResolveModelWithProfile is the phase-1a resolution chain (ADR-0011
// amendment, "Materialization" ordering): profile windows first; when no
// window matches, the template's own resolution (its windows, then its base
// model). A nil profile is the inert default — the spec decides alone,
// byte-identical to the pre-profile behavior.
func ResolveModelWithProfile(p *v1alpha1.AgentProfile, wf *v1alpha1.Workflow, now time.Time) string {
	if p != nil {
		if m := p.Spec.ResolveModel(now); m != "" {
			return m
		}
	}
	return wf.Spec.Agent.ResolveModel(now)
}
