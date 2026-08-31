// Package capability implements Surface Capability Policy enforcement
// (ADR-0003): a node may only execute if every Surface Capability it Requires
// is granted by one of the Workflow's declared External System Bindings.
//
// The engine is pure — no I/O, no Kubernetes client. It is called from two
// places:
//
//   - At runtime, by the deterministic kernel (the graph executor, ADR-0001)
//     before executing a node. A denied node is refused and marked failed.
//   - (Future) at admission/validation, to reject a Workflow whose nodes
//     require un-granted capabilities before it is ever run.
//
// Capability tokens are explicit strings in OAuth-scope style
// ("repository.read", "repository.push", "issue-tracker.comment.write"). A
// binding's Granted slice is the authority scope for that binding. A node's
// Requires slice lists {binding, capability} pairs. Matching is exact, except
// that a Granted entry of "*" is a wildcard granting any capability on that
// binding (wildcard convention: "*" grants any capability on the binding).
package capability

import (
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// Violation describes a single unmet capability requirement.
type Violation struct {
	NodeID     string `json:"nodeId,omitempty"` // empty when validating a raw requirement list
	Binding    string `json:"binding"`          // the binding the node required
	Capability string `json:"capability"`       // the capability requested
	Reason     string `json:"reason"`           // "binding not declared" | "capability not granted"
}

// Error carries all violations found during an Authorize call, so the caller
// (UI / executor) can surface every problem at once instead of just the first.
// Matches the aggregate-error convention of capability validation.
type Error struct {
	Violations []Violation
}

func (e *Error) Error() string {
	if len(e.Violations) == 0 {
		return "capability: denied"
	}
	var parts []string
	for _, v := range e.Violations {
		parts = append(parts, fmt.Sprintf("%s: %s on %q", v.Capability, v.Reason, v.Binding))
	}
	return fmt.Sprintf("capability: %d violation(s): %s", len(e.Violations), strings.Join(parts, "; "))
}

const (
	reasonBindingMissing    = "binding not declared"
	reasonCapabilityMissing = "capability not granted"
)

// Authorize checks that every requirement is satisfiable by the declared
// bindings: the named binding must exist, and it must grant the requested
// capability (exact match, or "*" wildcard). Returns all violations found —
// it does not short-circuit on the first.
//
// Backward compatible: empty requires → no violations (a node that requests
// nothing is always authorized, so existing nodes without Requires are
// unaffected). Non-empty requires against an empty binding set yields one
// violation per requirement (binding not declared).
func Authorize(bindings []v1alpha1.ExternalSystemBinding, requires []v1alpha1.CapabilityRequirement) []Violation {
	if len(requires) == 0 {
		return nil
	}

	// Index granted capabilities per binding name for O(require) lookup.
	granted := make(map[string]map[string]bool, len(bindings))
	for _, b := range bindings {
		caps := make(map[string]bool, len(b.Granted))
		for _, c := range b.Granted {
			caps[c] = true
		}
		granted[b.Name] = caps
	}

	var violations []Violation
	for _, req := range requires {
		caps, ok := granted[req.Binding]
		if !ok {
			violations = append(violations, Violation{
				Binding:    req.Binding,
				Capability: req.Capability,
				Reason:     reasonBindingMissing,
			})
			continue
		}
		// Granted includes the exact capability, or "*" grants everything.
		if !caps[req.Capability] && !caps["*"] {
			violations = append(violations, Violation{
				Binding:    req.Binding,
				Capability: req.Capability,
				Reason:     reasonCapabilityMissing,
			})
		}
	}
	return violations
}

// AuthorizeNode is the per-node helper the kernel calls before execution. It
// returns nil when the node carries no Requires or all are satisfied.
func AuthorizeNode(bindings []v1alpha1.ExternalSystemBinding, node v1alpha1.NodeSpec) []Violation {
	violations := Authorize(bindings, node.Requires)
	for i := range violations {
		violations[i].NodeID = node.ID
	}
	return violations
}

// NewError wraps a non-empty violation slice in an *Error, or returns nil when
// there is nothing to report. Convenience for callers that want a single error
// value.
func NewError(violations []Violation) error {
	if len(violations) == 0 {
		return nil
	}
	return &Error{Violations: violations}
}

// FormatViolations renders a violation slice into a single human-readable
// feedback string for node failure messages / logs.
func FormatViolations(violations []Violation) string {
	if len(violations) == 0 {
		return ""
	}
	// Sort for stable output (violations originate from a map iteration order).
	sorted := make([]Violation, len(violations))
	copy(sorted, violations)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].NodeID != sorted[j].NodeID {
			return sorted[i].NodeID < sorted[j].NodeID
		}
		if sorted[i].Binding != sorted[j].Binding {
			return sorted[i].Binding < sorted[j].Binding
		}
		return sorted[i].Capability < sorted[j].Capability
	})
	var parts []string
	for _, v := range sorted {
		if v.NodeID != "" {
			parts = append(parts, fmt.Sprintf("node %q requires %s on binding %q — %s", v.NodeID, v.Capability, v.Binding, v.Reason))
		} else {
			parts = append(parts, fmt.Sprintf("%s on binding %q — %s", v.Capability, v.Binding, v.Reason))
		}
	}
	return "capability policy: " + strings.Join(parts, "; ")
}
