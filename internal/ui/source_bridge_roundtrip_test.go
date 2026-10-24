package ui

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/crdwalk"
	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// The UI typed round-trip guard (#704 class, #710 ask 2): every leaf the
// chart's WorkflowTemplate schema DECLARES must survive the propose path —
// sigsyaml.Unmarshal into the typed templateDocument and back out through
// spliceTemplateIntoValues. The fast tier's reverse walk (CheckReverse)
// guarantees the Go side can hold each schema property, so the typed
// unmarshal cannot drop one; this test proves that guarantee END-TO-END on
// the exact path that erased spec.attachments at 06a4397: had the reverse
// guard's invariant been broken, the leaf would go missing here too.
func TestTemplateDocumentRoundTripPreservesEverySchemaLeaf(t *testing.T) {
	schema, err := crdwalk.LoadSchema("../../chart/crds/", v1alpha1.WorkflowTemplateCRDFile)
	if err != nil {
		t.Fatalf("load schema: %v", err)
	}
	specNode := schema["properties"].(map[string]interface{})["spec"].(map[string]interface{})
	leaves := crdwalk.SchemaLeaves(reflect.TypeOf(v1alpha1.WorkflowTemplateSpec{}), specNode)
	if len(leaves) < 20 {
		t.Fatalf("the schema walk enumerated only %d leaves — the generator went blind, this guard would prove nothing", len(leaves))
	}

	// Build the spec carrying a probe value at every declared leaf.
	probe := map[string]interface{}{}
	for _, l := range leaves {
		setPath(probe, l.Path, l.Value, l.Type)
	}
	specYAML, err := yaml.Marshal(probe)
	if err != nil {
		t.Fatalf("marshal probe spec: %v", err)
	}
	doc := fmt.Sprintf("apiVersion: %s\nkind: WorkflowTemplate\nmetadata:\n  name: probe-template\nspec:\n%s",
		v1alpha1.SchemeGroupVersion.Identifier(), indent(string(specYAML), "  "))

	// The propose path's own typed decode.
	var parsed templateDocument
	if err := sigsyaml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("the probe document must decode through the typed shape: %v", err)
	}

	// The splice: spec → canonical YAML inside the values file.
	_, _, newSpecYAML, err := spliceTemplateIntoValues(fixtureValuesFile, "workflowTemplates", "pr-review", parsed.Spec)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}

	// Every declared leaf must be present with its probe value.
	var stored map[string]interface{}
	if err := yaml.Unmarshal([]byte(newSpecYAML), &stored); err != nil {
		t.Fatalf("spliced spec is not valid YAML: %v", err)
	}
	for _, l := range leaves {
		if got, ok := lookupPath(stored, l.Path); !ok {
			t.Errorf("schema leaf %s was DROPPED by the typed round-trip (the #704 class, live in the UI write path)", l.Path)
		} else if fmt.Sprintf("%v", got) != l.Value {
			t.Errorf("schema leaf %s = %v, want %q (value mangled by the round-trip)", l.Path, got, l.Value)
		}
	}
}

// setPath writes a probe value at a dotted crdwalk path ("a.b[].c{}" →
// a: {b: [{c: {probe: value}}]}). Existing intermediates are REUSED — the
// leaves arrive sorted, siblings share every prefix.
func setPath(m map[string]interface{}, path, value, typ string) {
	segs := strings.Split(path, ".")
	cur := m
	for i, seg := range segs {
		last := i == len(segs)-1
		switch {
		case strings.HasSuffix(seg, "[]"):
			key := strings.TrimSuffix(seg, "[]")
			arr, _ := cur[key].([]interface{})
			if arr == nil {
				arr = []interface{}{map[string]interface{}{}}
				cur[key] = arr
			}
			elem, _ := arr[0].(map[string]interface{})
			if last {
				arr[0] = native(value, typ)
				return
			}
			if elem == nil {
				elem = map[string]interface{}{}
				arr[0] = elem
			}
			cur = elem
		case strings.HasSuffix(seg, "{}"):
			key := strings.TrimSuffix(seg, "{}")
			if last {
				cur[key] = map[string]interface{}{"probe": native(value, typ)}
				return
			}
			bag, _ := cur[key].(map[string]interface{})
			if bag == nil {
				bag = map[string]interface{}{}
				cur[key] = bag
			}
			elem, _ := bag["probe"].(map[string]interface{})
			if elem == nil {
				elem = map[string]interface{}{}
				bag["probe"] = elem
			}
			cur = elem
		default:
			if last {
				cur[seg] = native(value, typ)
				return
			}
			next, _ := cur[seg].(map[string]interface{})
			if next == nil {
				next = map[string]interface{}{}
				cur[seg] = next
			}
			cur = next
		}
	}
}

// native converts a scalar probe to its schema-declared type — a quoted
// "7" would be rejected by the typed decode (json: cannot unmarshal string
// into int), which is the document generator's bug, not the round-trip's.
func native(value, typ string) interface{} {
	switch typ {
	case "integer":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	case "boolean":
		return value == "true"
	}
	return value
}

// lookupPath reads a value back at the same dotted path convention.
func lookupPath(m map[string]interface{}, path string) (interface{}, bool) {
	segs := strings.Split(path, ".")
	var cur interface{} = m
	for _, seg := range segs {
		switch {
		case strings.HasSuffix(seg, "[]"):
			arr, ok := cur.(map[string]interface{})[strings.TrimSuffix(seg, "[]")].([]interface{})
			if !ok || len(arr) == 0 {
				return nil, false
			}
			cur = arr[0]
		case strings.HasSuffix(seg, "{}"):
			mm, ok := cur.(map[string]interface{})[strings.TrimSuffix(seg, "{}")].(map[string]interface{})
			if !ok {
				return nil, false
			}
			cur = mm["probe"]
		default:
			mm, ok := cur.(map[string]interface{})
			if !ok {
				return nil, false
			}
			cur, ok = mm[seg]
			if !ok {
				return nil, false
			}
		}
	}
	return cur, true
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
