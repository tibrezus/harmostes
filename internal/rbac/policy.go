// Package rbac implements node-type-level authorization for pipeline graphs.
//
// Enterprise deployments need to restrict who can use dangerous node types
// (vela-app, flux-reconcile — these deploy infrastructure). The RBAC engine
// maps each node type to a set of Authentik groups that are allowed to use it.
// If a node type has no entry in the policy, it is unrestricted (all users).
//
// The policy is injected into the UI server via chart values and enforced on
// every pipeline create/update (PUT /api/pipelines/{name}).
package rbac

import (
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// NodePolicy maps node types to the Authentik groups allowed to use them.
// A node type absent from the map is unrestricted. An empty slice means
// "no groups allowed" (effectively disabled). A nil/empty policy means
// everything is unrestricted.
type NodePolicy map[string][]string

// Violation describes a single node that the user is not authorized to use.
type Violation struct {
	NodeType   string   `json:"nodeType"`
	NodeID     string   `json:"nodeId"`
	Requires   []string `json:"requires"`
	UserGroups []string `json:"userGroups"`
}

// Error is returned by Authorize when one or more nodes are unauthorized.
// It carries all violations so the UI can display them at once.
type Error struct {
	Violations []Violation
}

func (e *Error) Error() string {
	if len(e.Violations) == 0 {
		return "rbac: unauthorized"
	}
	var parts []string
	for _, v := range e.Violations {
		parts = append(parts, fmt.Sprintf("node %q (type %s) requires groups %s", v.NodeID, v.NodeType, strings.Join(v.Requires, "|")))
	}
	return fmt.Sprintf("rbac: %d violation(s): %s", len(e.Violations), strings.Join(parts, "; "))
}

// Authorize checks whether the given user groups are allowed to use all node
// types in the graph. Returns nil if authorized, or an *Error listing all
// violations (so the UI can show every problem at once).
//
// The wildcard "*" in a node type's required groups means "any authenticated
// user" — useful for marking a type as restricted-by-default but still
// allowing all logged-in users.
func (p NodePolicy) Authorize(groups []string, nodes []v1alpha1.NodeSpec) error {
	if len(p) == 0 {
		return nil // no policy = unrestricted
	}

	groupSet := make(map[string]bool, len(groups))
	for _, g := range groups {
		groupSet[g] = true
	}

	var violations []Violation
	for _, node := range nodes {
		required, ok := p[node.Type]
		if !ok {
			continue // unrestricted
		}

		if isAuthorized(groupSet, required) {
			continue
		}

		violations = append(violations, Violation{
			NodeType:   node.Type,
			NodeID:     node.ID,
			Requires:   sortedCopy(required),
			UserGroups: sortedCopy(groups),
		})
	}

	if len(violations) > 0 {
		return &Error{Violations: violations}
	}
	return nil
}

// isAuthorized returns true if the user has at least one of the required groups,
// or if "*" is in the required list (any authenticated user).
func isAuthorized(userGroups map[string]bool, required []string) bool {
	for _, g := range required {
		if g == "*" {
			return true // wildcard: any authenticated user
		}
		if userGroups[g] {
			return true
		}
	}
	return false
}

// DefaultNodePolicy is the out-of-the-box policy: deployment node types
// (vela-app, flux-reconcile) require the "ops" or "admin" group. All other
// node types are unrestricted. This can be overridden via chart values
// (ui.rbac.nodeTypePolicy).
func DefaultNodePolicy() NodePolicy {
	return NodePolicy{
		"vela-app":       {"ops", "admin"},
		"flux-reconcile": {"ops", "admin"},
	}
}

// ParsePolicy converts a flat map[string][]string (e.g. from YAML config) into
// a NodePolicy. This is the bridge between chart values and the policy engine.
func ParsePolicy(raw map[string][]string) NodePolicy {
	p := make(NodePolicy, len(raw))
	for k, v := range raw {
		p[k] = v
	}
	return p
}

func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return []string{}
	}
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}
