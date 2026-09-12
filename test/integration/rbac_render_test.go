//go:build integration

package integration

// The rendered-RBAC gate (PR #427 review, R2a): the UI's cluster-facing
// claims — ClusterRole get-only and resourceName-narrowed, workflows without
// lifecycle verbs, no secrets/externalsecrets rules, bindings pointing at
// the SA the pod actually runs as — are pinned here against the committed
// golden render. A typo'd resourceNames or a drifted roleRef now fails CI
// instead of surfacing in prod as an indistinguishable 500 on /api/schema.
//
// The golden is byte-pinned by the chart job, so parsing it here is testing
// exactly what a cluster would receive from `helm template`.
//
// Assertions are written to be ABLE to fail (round-4 review): verbs and
// resources match by membership (never index [0]), write verbs are banned
// across EVERY harmostes.dev rule (a future rule cannot silently carry
// them), and roleRef/subject identity including namespaces is asserted.

import (
	"os"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

const goldenPath = "../../chart/ci/golden/full.yaml"

// writeVerbs are the verbs that must never appear outside the one workflows
// create rule the write path justifies.
var writeVerbs = map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true}

type goldenResource struct {
	Kind     string           `json:"kind"`
	Metadata map[string]any   `json:"metadata"`
	Rules    []map[string]any `json:"rules,omitempty"`
	RoleRef  map[string]any   `json:"roleRef,omitempty"`
	Subjects []map[string]any `json:"subjects,omitempty"`
}

func loadGoldenResources(t *testing.T) []goldenResource {
	t.Helper()
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden render: %v", err)
	}
	var out []goldenResource
	for _, doc := range strings.Split(string(raw), "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var r goldenResource
		if err := sigsyaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatalf("parse golden doc: %v", err)
		}
		if r.Kind != "" {
			out = append(out, r)
		}
	}
	return out
}

func findGolden(t *testing.T, kind, name string) goldenResource {
	t.Helper()
	for _, r := range loadGoldenResources(t) {
		if r.Kind == kind && r.Metadata["name"] == name {
			return r
		}
	}
	t.Fatalf("golden render has no %s/%s", kind, name)
	return goldenResource{}
}

func stringsOf(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestGoldenUIRBAC(t *testing.T) {
	// ClusterRole: exactly one rule — get on the two harmostes CRDs, narrowed
	// by resourceNames. Nothing cluster-scoped beyond that.
	cr := findGolden(t, "ClusterRole", "harmostes-ui-crd-reader")
	if len(cr.Rules) != 1 {
		t.Fatalf("ClusterRole rules = %d, want exactly 1", len(cr.Rules))
	}
	rule := cr.Rules[0]
	if got := stringsOf(rule["apiGroups"]); !contains(got, "apiextensions.k8s.io") {
		t.Errorf("ClusterRole apiGroups = %v, want apiextensions.k8s.io", got)
	}
	if got := stringsOf(rule["verbs"]); len(got) != 1 || got[0] != "get" {
		t.Errorf("ClusterRole verbs = %v, want [get] only", got)
	}
	if got := stringsOf(rule["resourceNames"]); !contains(got, v1alpha1.WorkflowCRDName) || !contains(got, v1alpha1.WorkflowTemplateCRDName) {
		t.Errorf("ClusterRole resourceNames = %v, want %s + %s", got, v1alpha1.WorkflowCRDName, v1alpha1.WorkflowTemplateCRDName)
	}

	// Role: membership-based. Every harmostes.dev rule must be read-only
	// EXCEPT the workflows rule, whose write verbs are exactly the lifecycle
	// surface (#418: create + update/patch for the arm/pause toggle and the
	// manual wake, delete for instance removal); secrets and
	// externalsecrets must not appear anywhere. Verbs are granted only when a
	// route needs them — deletecollection stays out (no bulk route exists),
	// and any NEW write verb must arrive in the PR that ships its route.
	role := findGolden(t, "Role", "harmostes-ui")
	if ns, ok := role.Metadata["namespace"]; !ok || ns != "harmostes-ci" {
		t.Errorf("Role namespace = %v, want harmostes-ci (the golden values' namespace)", ns)
	}
	workflowsRuleFound := false
	for i, r := range role.Rules {
		groups := stringsOf(r["apiGroups"])
		resources := stringsOf(r["resources"])
		verbs := stringsOf(r["verbs"])
		if !contains(groups, "harmostes.dev") {
			continue
		}
		for _, res := range resources {
			if res == "secrets" {
				t.Errorf("rule %d grants secrets — the token manager is gone (#291)", i)
			}
			if res == "externalsecrets" {
				t.Errorf("rule %d grants externalsecrets — dead privilege since ADR-0011", i)
			}
			if res == "pipelines" {
				t.Errorf("rule %d grants pipelines — the canvas was dismantled AND the resource with it (no CRD, no type)", i)
			}
		}
		if contains(verbs, "watch") {
			t.Errorf("rule %d (resources %v) grants watch — the UI uses a direct non-cached client and never calls Watch", i, resources)
		}
		for _, v := range verbs {
			if !writeVerbs[v] {
				continue
			}
			isWorkflowLifecycle := contains(resources, "workflows") &&
				(v == "create" || v == "update" || v == "patch" || v == "delete")
			if !isWorkflowLifecycle {
				t.Errorf("rule %d (resources %v) carries write verb %q — write verbs are granted only to the workflows lifecycle set (create/update/patch/delete)", i, resources, v)
			}
		}
		if contains(resources, "workflows") {
			workflowsRuleFound = true
			for _, forbidden := range []string{"deletecollection"} {
				if contains(verbs, forbidden) {
					t.Errorf("workflows verb %q granted — no bulk route exists; verbs are granted only when a route needs them", forbidden)
				}
			}
			for _, want := range []string{"get", "list", "create", "update", "patch", "delete"} {
				if !contains(verbs, want) {
					t.Errorf("workflows verbs %v missing %q", verbs, want)
				}
			}
		}
	}
	if !workflowsRuleFound {
		t.Fatal("golden Role has no workflows rule")
	}

	// Template read-only shape (#420): the UI proposes template changes via
	// MRs against the template's GIT source — it must carry NO write verb on
	// workflowtemplates. If a write verb appears here, an in-cluster
	// mutation path for templates exists, and the bridge's contract is
	// broken. Verbs are exactly get/list (+watch is rejected above).
	for i, r := range role.Rules {
		if !contains(stringsOf(r["apiGroups"]), "harmostes.dev") || !contains(stringsOf(r["resources"]), "workflowtemplates") {
			continue
		}
		for _, v := range stringsOf(r["verbs"]) {
			if writeVerbs[v] {
				t.Errorf("UI Role rule %d grants %q on workflowtemplates — templates are never mutated in-cluster; changes go through the git MR bridge (#420)", i, v)
			}
		}
	}

	// The controller's history recorder (#420) is the ONE justified writer
	// on workflowtemplates: when Flux delivers a spec change it appends the
	// revision window to the CR's annotation. Its verbs must be exactly
	// get/list/watch (the worker's read set) + update/patch (the recorder) —
	// anything wider (delete!) is drift. The controller Role is rendered in
	// the same namespace; find it by its harmostes.dev rules.
	ctrlRole := findGolden(t, "Role", "harmostes-controller")
	recorderRuleFound := false
	for i, r := range ctrlRole.Rules {
		if !contains(stringsOf(r["apiGroups"]), "harmostes.dev") || !contains(stringsOf(r["resources"]), "workflowtemplates") {
			continue
		}
		recorderRuleFound = true
		verbs := stringsOf(r["verbs"])
		for _, want := range []string{"get", "list", "watch", "update", "patch"} {
			if !contains(verbs, want) {
				t.Errorf("controller Role rule %d (workflowtemplates) missing %q — the history recorder needs read + annotation update", i, want)
			}
		}
		for _, v := range verbs {
			if v == "delete" || v == "deletecollection" || v == "create" {
				t.Errorf("controller Role rule %d grants %q on workflowtemplates — the recorder only annotates; it never creates or deletes templates", i, v)
			}
		}
	}
	if !recorderRuleFound {
		t.Fatal("controller Role has no workflowtemplates rule")
	}

	// Bindings: both point their rules at the SA the ui pod runs as, in the
	// rendered namespace, with roleRefs naming the rendered roles.
	rb := findGolden(t, "RoleBinding", "harmostes-ui")
	if got, _ := rb.RoleRef["name"].(string); got != "harmostes-ui" {
		t.Errorf("RoleBinding roleRef.name = %q, want harmostes-ui", got)
	}
	if got, _ := rb.RoleRef["kind"].(string); got != "Role" {
		t.Errorf("RoleBinding roleRef.kind = %q, want Role", got)
	}
	crb := findGolden(t, "ClusterRoleBinding", "harmostes-ui-crd-reader")
	if got, _ := crb.RoleRef["name"].(string); got != "harmostes-ui-crd-reader" {
		t.Errorf("ClusterRoleBinding roleRef.name = %q, want harmostes-ui-crd-reader", got)
	}
	for kind, b := range map[string]goldenResource{"RoleBinding": rb, "ClusterRoleBinding": crb} {
		if len(b.Subjects) != 1 {
			t.Fatalf("%s subjects = %v, want exactly the ui SA", kind, b.Subjects)
		}
		if b.Subjects[0]["kind"] != "ServiceAccount" || b.Subjects[0]["name"] != "harmostes-ui" {
			t.Errorf("%s subject = %v, want ServiceAccount/harmostes-ui", kind, b.Subjects[0])
		}
		if b.Subjects[0]["namespace"] != "harmostes-ci" {
			t.Errorf("%s subject namespace = %v, want harmostes-ci", kind, b.Subjects[0]["namespace"])
		}
	}
}
