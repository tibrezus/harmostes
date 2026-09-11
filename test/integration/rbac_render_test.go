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

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const goldenPath = "../../chart/ci/golden/full.yaml"

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
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatalf("parse golden doc: %v", err)
		}
		if r.Kind != "" {
			out = append(out, r)
		}
	}
	return out
}

func findGolden(t *testing.T, kind, name string) *goldenResource {
	t.Helper()
	resources := loadGoldenResources(t)
	for i := range resources {
		r := &resources[i]
		if r.Kind == kind && r.Metadata["name"] == name {
			return r
		}
	}
	t.Fatalf("golden render has no %s/%s", kind, name)
	return nil
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

// TestGoldenUIRBAC pins the right-sized UI RBAC (ADR-0012 §5).
func TestGoldenUIRBAC(t *testing.T) {
	// ClusterRole: exactly one rule — get on the two harmostes CRDs, narrowed
	// by resourceNames. Nothing cluster-scoped beyond that.
	cr := findGolden(t, "ClusterRole", "harmostes-ui-crd-reader")
	if len(cr.Rules) != 1 {
		t.Fatalf("ClusterRole rules = %d, want exactly 1", len(cr.Rules))
	}
	rule := cr.Rules[0]
	if got := stringsOf(rule["apiGroups"]); len(got) != 1 || got[0] != "apiextensions.k8s.io" {
		t.Errorf("ClusterRole apiGroups = %v", got)
	}
	if got := stringsOf(rule["verbs"]); len(got) != 1 || got[0] != "get" {
		t.Errorf("ClusterRole verbs = %v, want [get] only", got)
	}
	wantNames := []string{"workflows.harmostes.dev", "workflowtemplates.harmostes.dev"}
	if got := stringsOf(rule["resourceNames"]); len(got) != 2 || got[0] != wantNames[0] || got[1] != wantNames[1] {
		t.Errorf("ClusterRole resourceNames = %v, want %v", got, wantNames)
	}

	// Role: workflows create + reads, NO lifecycle verbs, NO secrets/ESO.
	role := findGolden(t, "Role", "harmostes-ui")
	var wfRule map[string]any
	for _, r := range role.Rules {
		if groups, _ := r["apiGroups"].([]any); len(groups) > 0 && groups[0] == "harmostes.dev" {
			if res, _ := r["resources"].([]any); len(res) > 0 && res[0] == "workflows" {
				wfRule = r
			}
			if res, _ := r["resources"].([]any); len(res) > 0 && res[0] == "secrets" {
				t.Error("golden Role still grants secrets — the token manager is gone (#291)")
			}
			if res, _ := r["resources"].([]any); len(res) > 0 && res[0] == "externalsecrets" {
				t.Error("golden Role still grants externalsecrets — dead privilege since ADR-0011")
			}
		}
	}
	if wfRule == nil {
		t.Fatal("golden Role has no workflows rule")
	}
	verbs := stringsOf(wfRule["verbs"])
	for _, forbidden := range []string{"update", "patch", "delete"} {
		for _, v := range verbs {
			if v == forbidden {
				t.Errorf("workflows verb %q granted — lifecycle routes do not exist yet (#418/#419 re-add with their routes)", forbidden)
			}
		}
	}
	for _, want := range []string{"get", "list", "watch", "create"} {
		found := false
		for _, v := range verbs {
			if v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("workflows verbs %v missing %q", verbs, want)
		}
	}

	// Both bindings must point the rules at the SA the ui pod runs as.
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		var b *goldenResource
		if kind == "RoleBinding" {
			b = findGolden(t, kind, "harmostes-ui")
		} else {
			b = findGolden(t, kind, "harmostes-ui-crd-reader")
		}
		if len(b.Subjects) != 1 {
			t.Fatalf("%s subjects = %v, want exactly the ui SA", kind, b.Subjects)
		}
		if b.Subjects[0]["kind"] != "ServiceAccount" || b.Subjects[0]["name"] != "harmostes-ui" {
			t.Errorf("%s subject = %v, want ServiceAccount/harmostes-ui (the SA the ui pod runs as)", kind, b.Subjects[0])
		}
	}
}
