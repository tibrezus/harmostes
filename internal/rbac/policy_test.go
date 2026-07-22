package rbac

import (
	"errors"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

func TestAuthorizeNoPolicy(t *testing.T) {
	// Empty policy = unrestricted: everything allowed.
	p := NodePolicy{}
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
		{ID: "reconcile", Type: "flux-reconcile"},
	}
	if err := p.Authorize([]string{}, nodes); err != nil {
		t.Fatalf("empty policy should allow all: %v", err)
	}
}

func TestAuthorizeUnrestrictedType(t *testing.T) {
	// Node type not in policy = unrestricted.
	p := NodePolicy{"vela-app": {"ops"}}
	nodes := []v1alpha1.NodeSpec{
		{ID: "run", Type: "plugin"},
	}
	if err := p.Authorize([]string{}, nodes); err != nil {
		t.Fatalf("unrestricted type should pass: %v", err)
	}
}

func TestAuthorizeAllowed(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
	}
	// User in "ops" group.
	if err := p.Authorize([]string{"ops"}, nodes); err != nil {
		t.Fatalf("ops user should be authorized for vela-app: %v", err)
	}
}

func TestAuthorizeAdminBypass(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
		{ID: "reconcile", Type: "flux-reconcile"},
	}
	// Admin can use both restricted types.
	if err := p.Authorize([]string{"admin", "users"}, nodes); err != nil {
		t.Fatalf("admin should be authorized: %v", err)
	}
}

func TestAuthorizeDenied(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
	}
	// Regular user (no ops/admin group).
	err := p.Authorize([]string{"devs"}, nodes)
	if err == nil {
		t.Fatal("regular user should be denied for vela-app")
	}

	var rbacErr *Error
	if !errors.As(err, &rbacErr) {
		t.Fatalf("expected *rbac.Error, got %T: %v", err, err)
	}
	if len(rbacErr.Violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(rbacErr.Violations))
	}
	v := rbacErr.Violations[0]
	if v.NodeType != "vela-app" {
		t.Errorf("violation NodeType = %q, want %q", v.NodeType, "vela-app")
	}
	if v.NodeID != "deploy" {
		t.Errorf("violation NodeID = %q, want %q", v.NodeID, "deploy")
	}
}

func TestAuthorizeMultipleViolations(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
		{ID: "reconcile", Type: "flux-reconcile"},
		{ID: "build", Type: "plugin"}, // unrestricted
	}
	err := p.Authorize([]string{"users"}, nodes)
	if err == nil {
		t.Fatal("should be denied")
	}
	var rbacErr *Error
	if !errors.As(err, &rbacErr) {
		t.Fatalf("expected *rbac.Error, got %T", err)
	}
	if len(rbacErr.Violations) != 2 {
		t.Fatalf("expected 2 violations (plugin is unrestricted), got %d", len(rbacErr.Violations))
	}
}

func TestAuthorizeWildcard(t *testing.T) {
	p := NodePolicy{"vela-app": {"*"}}
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
	}
	// Any authenticated user passes the wildcard.
	if err := p.Authorize([]string{"anyone"}, nodes); err != nil {
		t.Fatalf("wildcard should allow any user: %v", err)
	}
}

func TestAuthorizeEmptyGroupsDenied(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "deploy", Type: "vela-app"},
	}
	// User with no groups at all.
	err := p.Authorize([]string{}, nodes)
	if err == nil {
		t.Fatal("user with no groups should be denied")
	}
}

func TestAuthorizeMixedNodes(t *testing.T) {
	p := DefaultNodePolicy()
	nodes := []v1alpha1.NodeSpec{
		{ID: "build", Type: "plugin"},        // unrestricted
		{ID: "test", Type: "agent"},          // unrestricted
		{ID: "deploy", Type: "vela-app"},     // restricted
		{ID: "sync", Type: "flux-reconcile"}, // restricted
	}
	// Ops user: all pass.
	if err := p.Authorize([]string{"ops"}, nodes); err != nil {
		t.Fatalf("ops user should pass all: %v", err)
	}
	// Regular user: only unrestricted pass.
	err := p.Authorize([]string{"devs"}, nodes)
	if err == nil {
		t.Fatal("regular user should fail on restricted nodes")
	}
}

func TestParsePolicy(t *testing.T) {
	raw := map[string][]string{
		"vela-app": {"ops", "admin"},
		"agent":    {"*"},
	}
	p := ParsePolicy(raw)
	if len(p) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(p))
	}
}

func TestViolationErrorString(t *testing.T) {
	e := &Error{Violations: []Violation{
		{NodeType: "vela-app", NodeID: "deploy", Requires: []string{"admin", "ops"}},
	}}
	s := e.Error()
	if s == "" {
		t.Fatal("error string should not be empty")
	}
}

func TestSortedCopy(t *testing.T) {
	out := sortedCopy([]string{"c", "a", "b"})
	if len(out) != 3 || out[0] != "a" || out[1] != "b" || out[2] != "c" {
		t.Fatalf("sortedCopy = %v", out)
	}
	// Nil-safe.
	out2 := sortedCopy(nil)
	if len(out2) != 0 {
		t.Fatalf("sortedCopy(nil) = %v", out2)
	}
}
