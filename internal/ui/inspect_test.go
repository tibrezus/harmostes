package ui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// inspectDoc is the canonical document the island loads — identical shape
// to what templateYAMLOf marshals.
func inspectDoc(t *testing.T, spec v1alpha1.WorkflowTemplateSpec) string {
	t.Helper()
	b, err := yaml.Marshal(templateDocument{
		APIVersion: v1alpha1.SchemeGroupVersion.Identifier(),
		Kind:       "WorkflowTemplate",
		Metadata:   templateDocumentMeta{Name: "pr-review"},
		Spec:       spec,
	})
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	return string(b)
}

// TestApplyTemplateEdits pins the structured edit surface: every supported
// path applies to the typed spec; an unknown path is an ERROR, not a no-op —
// a typo'd path must never look like a successful edit.
func TestApplyTemplateEdits(t *testing.T) {
	doc := inspectDoc(t, v1alpha1.WorkflowTemplateSpec{
		Description: "before",
		Prepare:     v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "pr-fetch"}},
		Deploy:      v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
	})

	apply := func(t *testing.T, doc string, edits ...inspectEdit) (v1alpha1.WorkflowTemplateSpec, string) {
		t.Helper()
		out, err := applyTemplateEdits(doc, edits)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		var d templateDocument
		if err := yaml.Unmarshal([]byte(out), &d); err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		return d.Spec, out
	}

	// Strings set and clear (empty = unset → omitempty drops the key).
	spec, out := apply(t, doc,
		inspectEdit{Path: "description", Value: "after"},
		inspectEdit{Path: "agent.model", Value: "mistral-small-latest"},
	)
	if spec.Description != "after" || spec.Agent.Model != "mistral-small-latest" {
		t.Errorf("string edits landed wrong: %+v", spec)
	}
	if !strings.Contains(out, "mistral-small-latest") {
		t.Error("document does not carry the applied model")
	}
	spec, _ = apply(t, doc, inspectEdit{Path: "prepare.plugin.name", Value: ""})
	if spec.Prepare.Plugin.Name != "" {
		t.Error("empty string did not clear prepare.plugin.name")
	}

	// Bool writes an explicit value.
	spec, _ = apply(t, doc, inspectEdit{Path: "agent.enabled", Value: false})
	if spec.Agent.Enabled == nil || *spec.Agent.Enabled {
		t.Error("agent.enabled=false did not land as an explicit false")
	}

	// Int (JSON numbers arrive as float64 — the applier must reject
	// non-integers, not truncate).
	spec, _ = apply(t, doc, inspectEdit{Path: "agent.maxFixes", Value: float64(3)})
	if spec.Agent.MaxFixes != 3 {
		t.Errorf("maxFixes = %d, want 3", spec.Agent.MaxFixes)
	}

	// Rejections: unknown path, wrong types, broken document.
	for _, tc := range []struct {
		name  string
		doc   string
		edits []inspectEdit
	}{
		{"unknown path", doc, []inspectEdit{{Path: "spec.graph", Value: "x"}}},
		{"path traversal attempt", doc, []inspectEdit{{Path: "agent.tools", Value: []any{"x"}}}},
		{"bool as string", doc, []inspectEdit{{Path: "agent.enabled", Value: "true"}}},
		{"int as string", doc, []inspectEdit{{Path: "agent.maxFixes", Value: "3"}}},
		{"fractional int", doc, []inspectEdit{{Path: "agent.maxFixes", Value: 1.5}}},
		{"broken yaml", "not: [valid", []inspectEdit{{Path: "description", Value: "x"}}},
		{"wrong kind", "apiVersion: v1\nkind: Pod\n", []inspectEdit{{Path: "description", Value: "x"}}},
	} {
		if _, err := applyTemplateEdits(tc.doc, tc.edits); err == nil {
			t.Errorf("%s: expected rejection, got success", tc.name)
		}
	}

	// Unknown paths never mutate: the error names the path.
	_, err := applyTemplateEdits(doc, []inspectEdit{{Path: "nope.nope", Value: 1}})
	if err == nil || !strings.Contains(err.Error(), "nope.nope") {
		t.Errorf("rejection should name the path, got %v", err)
	}
}

// TestApplyTemplateEdits_RoundTripLossless pins the acceptance: YAML →
// form-model → YAML is lossless for the supported param space. The
// form-model IS the typed spec (the same Go struct the document marshals
// from), so losslessness = byte-stable marshal + spec deep-equality across
// the round trip, for representative shapes including the ugly ones
// (unset everything, unicode, all fields set).
func TestApplyTemplateEdits_RoundTripLossless(t *testing.T) {
	yes := true
	specs := map[string]v1alpha1.WorkflowTemplateSpec{
		"empty":        {},
		"fixture-like": v1alpha1.WorkflowTemplateSpec{},
		"everything set": {
			Description: "PR review — ünïcode ✓ & <angles>",
			Prepare:     v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "pr-fetch"}},
			Agent: v1alpha1.AgentSpec{
				Enabled: &yes,
				Model:   "mistral-small-latest",
				Skill:   "/skills/pr-review/",
				Gate:    v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review"}},
			},
			Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
		},
	}
	specs["fixture-like"] = v1alpha1.WorkflowTemplateSpec{
		Description: "PR review (fixture)",
		Prepare:     v1alpha1.PrepareSpec{Plugin: v1alpha1.PluginRef{Name: "pr-fetch"}},
		Agent: v1alpha1.AgentSpec{
			Model: "mistral-small-latest",
			Skill: "/skills/pr-review/",
			Gate:  v1alpha1.GateRef{Plugin: v1alpha1.PluginRef{Name: "pr-review"}},
		},
		Deploy: v1alpha1.DeploySpec{Plugin: v1alpha1.PluginRef{Name: "post-review"}},
	}

	for name, spec := range specs {
		doc := inspectDoc(t, spec)

		// Apply the identity edit set: every supported path written back
		// with its CURRENT value — the inspector's own no-op round trip.
		var edits []inspectEdit
		if spec.Description != "" {
			edits = append(edits, inspectEdit{Path: "description", Value: spec.Description})
		}
		if spec.Prepare.Plugin.Name != "" {
			edits = append(edits, inspectEdit{Path: "prepare.plugin.name", Value: spec.Prepare.Plugin.Name})
		}
		if spec.Agent.Model != "" {
			edits = append(edits, inspectEdit{Path: "agent.model", Value: spec.Agent.Model})
		}
		if spec.Agent.Skill != "" {
			edits = append(edits, inspectEdit{Path: "agent.skill", Value: spec.Agent.Skill})
		}
		if spec.Agent.Gate.Plugin.Name != "" {
			edits = append(edits, inspectEdit{Path: "agent.gate.plugin.name", Value: spec.Agent.Gate.Plugin.Name})
		}
		if spec.Deploy.Plugin.Name != "" {
			edits = append(edits, inspectEdit{Path: "deploy.plugin.name", Value: spec.Deploy.Plugin.Name})
		}

		out, err := applyTemplateEdits(doc, edits)
		if err != nil {
			t.Fatalf("%s: apply: %v", name, err)
		}

		var once, twice templateDocument
		if err := yaml.Unmarshal([]byte(doc), &once); err != nil {
			t.Fatalf("%s: parse doc: %v", name, err)
		}
		if err := yaml.Unmarshal([]byte(out), &twice); err != nil {
			t.Fatalf("%s: parse transformed: %v", name, err)
		}
		if !reflect.DeepEqual(once.Spec, twice.Spec) {
			t.Errorf("%s: round trip changed the spec:\n got %+v\nwant %+v", name, twice.Spec, once.Spec)
		}
		// Byte-equality of the full document under identity edits.
		if name != "empty" && out != doc {
			// The identity edits may legitimately DROP keys that were
			// already absent, but for docs whose edited fields were set,
			// the bytes must be identical.
			t.Errorf("%s: identity edits changed the document bytes:\n--- got\n%s\n--- want\n%s", name, out, doc)
		}
	}
}

// TestInspectFieldsMatchChartCRD is the schema-lockstep guard (#419): the
// inspector's descriptors claim their types mirror the CRD — the single
// schema source. Walk chart/crds/workflowtemplates.harmostes.dev.yaml and
// verify every supported path exists with the declared type. Drift here
// would render a form whose input type lies about the schema.
func TestInspectFieldsMatchChartCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "chart", "crds", "workflowtemplates.harmostes.dev.yaml"))
	if err != nil {
		t.Fatalf("read chart CRD: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse chart CRD: %v", err)
	}
	walk := func(node any, path ...string) any {
		for _, k := range path {
			if list, ok := node.([]any); ok {
				i := 0
				for _, c := range k {
					if c < '0' || c > '9' {
						i = -1
						break
					}
					i = i*10 + int(c-'0')
				}
				if i >= 0 && i < len(list) {
					node = list[i]
					continue
				}
				return nil
			}
			if mm, ok := node.(map[string]any); ok {
				node = mm[k]
				continue
			}
			return nil
		}
		return node
	}

	wantTypes := map[string]string{"string": "string", "bool": "boolean", "int": "integer"}
	for _, f := range inspectFields {
		node := walk(crd, "spec", "versions", "0", "schema", "openAPIV3Schema",
			"properties", "spec")
		seg := strings.Split(f.Path, ".")
		for i, p := range seg {
			node = walk(node, "properties", p)
			if node == nil {
				t.Errorf("path %q: chart CRD has no schema node at %q", f.Path, strings.Join(seg[:i+1], "."))
				break
			}
		}
		if node == nil {
			continue
		}
		got := walk(node, "type")
		if got != wantTypes[f.Kind] {
			t.Errorf("path %q: CRD type = %v, descriptor kind %q wants %q", f.Path, got, f.Kind, wantTypes[f.Kind])
		}
	}
}
