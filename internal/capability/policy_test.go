package capability

import (
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func binding(name string, granted ...string) v1alpha1.ExternalSystemBinding {
	return v1alpha1.ExternalSystemBinding{Name: name, Granted: granted}
}

func req(b, c string) v1alpha1.CapabilityRequirement {
	return v1alpha1.CapabilityRequirement{Binding: b, Capability: c}
}

// TestAuthorize_EmptyRequires — a node that requests nothing is always
// authorized (backward compatibility with existing nodes without Requires).
func TestAuthorize_EmptyRequires(t *testing.T) {
	if v := Authorize(nil, nil); v != nil {
		t.Errorf("Authorize(nil,nil) = %+v, want nil", v)
	}
	if v := Authorize([]v1alpha1.ExternalSystemBinding{binding("repo")}, nil); v != nil {
		t.Errorf("Authorize with empty requires = %+v, want nil", v)
	}
}

// TestAuthorize_Satisfied — every requirement granted → no violations.
func TestAuthorize_Satisfied(t *testing.T) {
	bindings := []v1alpha1.ExternalSystemBinding{
		binding("sourceRepo", "repository.read", "repository.push"),
		binding("issueTracker", "issue-tracker.comment.write"),
	}
	requires := []v1alpha1.CapabilityRequirement{
		req("sourceRepo", "repository.read"),
		req("issueTracker", "issue-tracker.comment.write"),
	}
	if v := Authorize(bindings, requires); v != nil {
		t.Errorf("Authorize satisfied = %+v, want nil", v)
	}
}

// TestAuthorize_MissingBinding — a requirement against a binding not declared
// on the workflow yields a "binding not declared" violation.
func TestAuthorize_MissingBinding(t *testing.T) {
	bindings := []v1alpha1.ExternalSystemBinding{binding("sourceRepo", "repository.read")}
	requires := []v1alpha1.CapabilityRequirement{req("wiki", "wiki.page.write")}
	v := Authorize(bindings, requires)
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if v[0].Reason != reasonBindingMissing || v[0].Binding != "wiki" {
		t.Errorf("violation = %+v, want binding-missing on wiki", v[0])
	}
}

// TestAuthorize_MissingCapability — declared binding, but the capability is
// not in its Granted scope.
func TestAuthorize_MissingCapability(t *testing.T) {
	bindings := []v1alpha1.ExternalSystemBinding{binding("sourceRepo", "repository.read")}
	requires := []v1alpha1.CapabilityRequirement{req("sourceRepo", "repository.push")}
	v := Authorize(bindings, requires)
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if v[0].Reason != reasonCapabilityMissing || v[0].Capability != "repository.push" {
		t.Errorf("violation = %+v, want capability-missing for repository.push", v[0])
	}
}

// TestAuthorize_WildcardGrant — a binding with Granted=["*"] authorizes any
// capability requested against it.
func TestAuthorize_WildcardGrant(t *testing.T) {
	bindings := []v1alpha1.ExternalSystemBinding{binding("sourceRepo", "*")}
	requires := []v1alpha1.CapabilityRequirement{
		req("sourceRepo", "repository.read"),
		req("sourceRepo", "repository.push"),
		req("sourceRepo", "anything.weird"),
	}
	if v := Authorize(bindings, requires); v != nil {
		t.Errorf("wildcard grant should satisfy all, got %+v", v)
	}
}

// TestAuthorize_CollectsAllViolations — the engine does not short-circuit;
// every unmet requirement is reported (matches internal/rbac convention).
func TestAuthorize_CollectsAllViolations(t *testing.T) {
	bindings := []v1alpha1.ExternalSystemBinding{
		binding("sourceRepo", "repository.read"),
	}
	requires := []v1alpha1.CapabilityRequirement{
		req("sourceRepo", "repository.push"), // capability missing
		req("wiki", "wiki.page.write"),       // binding missing
		req("sourceRepo", "repository.read"), // satisfied
	}
	v := Authorize(bindings, requires)
	if len(v) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(v), v)
	}
}

// TestAuthorizeNode_StampsNodeID — the per-node helper tags every violation
// with the node ID for actionable feedback.
func TestAuthorizeNode_StampsNodeID(t *testing.T) {
	node := v1alpha1.NodeSpec{
		ID:       "comment-on-issue",
		Type:     "plugin",
		Requires: []v1alpha1.CapabilityRequirement{req("missingBinding", "issue-tracker.comment.write")},
	}
	v := AuthorizeNode(nil, node)
	if len(v) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(v), v)
	}
	if v[0].NodeID != "comment-on-issue" {
		t.Errorf("violation NodeID = %q, want comment-on-issue", v[0].NodeID)
	}
}

// TestAuthorizeNode_NoRequires — a node without Requires authorizes trivially.
func TestAuthorizeNode_NoRequires(t *testing.T) {
	if v := AuthorizeNode(nil, v1alpha1.NodeSpec{ID: "n1", Type: "plugin"}); v != nil {
		t.Errorf("node without Requires = %+v, want nil", v)
	}
}

// TestNewError_NilOnEmpty — NewError returns nil for an empty violation slice.
func TestNewError_NilOnEmpty(t *testing.T) {
	if err := NewError(nil); err != nil {
		t.Errorf("NewError(nil) = %v, want nil", err)
	}
	if err := NewError([]Violation{}); err != nil {
		t.Errorf("NewError([]) = %v, want nil", err)
	}
}

// TestError_MessageMentionsAll — the error string lists every violation.
func TestError_MessageMentionsAll(t *testing.T) {
	err := NewError([]Violation{
		{Capability: "repository.push", Binding: "sourceRepo", Reason: reasonCapabilityMissing},
		{Capability: "wiki.page.write", Binding: "wiki", Reason: reasonBindingMissing},
	})
	if err == nil {
		t.Fatal("NewError returned nil for non-empty violations")
	}
	msg := err.Error()
	if !strings.Contains(msg, "repository.push") || !strings.Contains(msg, "wiki.page.write") {
		t.Errorf("error message missing capabilities: %q", msg)
	}
	if !strings.Contains(msg, "2 violation") {
		t.Errorf("error message missing count: %q", msg)
	}
}

// TestFormatViolations — human-readable feedback includes node, capability,
// binding, and reason.
func TestFormatViolations(t *testing.T) {
	s := FormatViolations([]Violation{
		{NodeID: "n1", Capability: "repository.push", Binding: "sourceRepo", Reason: reasonCapabilityMissing},
	})
	if !strings.Contains(s, "n1") || !strings.Contains(s, "repository.push") || !strings.Contains(s, "sourceRepo") {
		t.Errorf("format missing fields: %q", s)
	}
	if !strings.HasPrefix(s, "capability policy:") {
		t.Errorf("format missing prefix: %q", s)
	}
	if FormatViolations(nil) != "" {
		t.Error("FormatViolations(nil) should be empty")
	}
}
