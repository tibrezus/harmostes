package worker

import (
	"fmt"

	"github.com/tibrezus/harmostes/api/v1alpha1"
)

// Extension composition (ADR-0011 amendment, three-tier 1b / #786): the
// per-attempt pi extension set is composED, never accumulated by accident —
// the AgentProfile carries the UNIVERSAL set (the environment's decision),
// the resolved workflow carries the template's DELTA, and the composed
// manifest is stamped into the Attempt Job for the materialize init
// container to fetch, verify, and install. Empty on both sides = no
// materialization step at all (inert by default).
//
// Delta semantics are REPLACE-BY-NAME: a template entry shadows a universal
// one with the same name (a template owns its agent surface; the profile
// must not leak under it). Order is deterministic: universal entries in
// declaration order (each replaced in place when shadowed), then
// template-only entries in declaration order — two dispatches of the same
// profile+template produce byte-identical manifests, so the Job env (and
// thus the materialization) is reproducible.
//
// Pin validation (exact npm specs, SRI integrity shape) is the types
// package's contract (v1alpha1.ValidateExtensionPins / ValidateHarnessPin) —
// the dispatcher runs it at the compose seam, before any pod exists.
func ComposeExtensions(universal, delta []v1alpha1.ExtensionPin) []v1alpha1.ExtensionPin {
	if len(universal) == 0 && len(delta) == 0 {
		return nil
	}
	universalNames := make(map[string]bool, len(universal))
	for _, u := range universal {
		universalNames[u.Name] = true
	}
	deltaByName := make(map[string]v1alpha1.ExtensionPin, len(delta))
	for _, d := range delta {
		deltaByName[d.Name] = d
	}
	out := make([]v1alpha1.ExtensionPin, 0, len(universal)+len(delta))
	// Universal entries in declaration order, each replaced in place when
	// the template shadows it.
	for _, u := range universal {
		if d, ok := deltaByName[u.Name]; ok {
			out = append(out, d)
			continue
		}
		out = append(out, u)
	}
	// Template-only entries in declaration order (deduped — later duplicates
	// of one name would be dead weight; first declaration wins).
	seen := make(map[string]bool, len(delta))
	for _, d := range delta {
		if universalNames[d.Name] || seen[d.Name] {
			continue
		}
		out = append(out, d)
		seen[d.Name] = true
	}
	return out
}

// ComposeRuntime is the dispatcher's per-attempt materialization fact: the
// composed pin set plus the profile's harness pin, both validated (#504 —
// malformed pins refuse HERE, with the offending artifact named, before any
// pod is burned). Empty pins and no harness = the inert default.
func ComposeRuntime(universal, delta []v1alpha1.ExtensionPin, harness v1alpha1.HarnessRef) ([]v1alpha1.ExtensionPin, *v1alpha1.HarnessRef, error) {
	if err := v1alpha1.ValidateExtensionPins(universal); err != nil {
		return nil, nil, fmt.Errorf("universal extensions: %w", err)
	}
	if err := v1alpha1.ValidateExtensionPins(delta); err != nil {
		return nil, nil, fmt.Errorf("template extensions: %w", err)
	}
	if err := v1alpha1.ValidateHarnessPin(harness); err != nil {
		return nil, nil, err
	}
	composed := ComposeExtensions(universal, delta)
	if len(composed) == 0 && harness.Runtime == "" {
		return nil, nil, nil
	}
	if harness.Runtime != "" {
		h := harness
		return composed, &h, nil
	}
	return composed, nil, nil
}
