package v1alpha1

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// CRD-conformance: every Go field reachable from a CRD root type must exist in
// the chart's CRD schema. The fake controller-runtime client used in tests does
// not validate against CRDs, so a Go field added without its schema property is
// invisible to the whole test suite while the real API server prunes the field
// on every Status().Patch — production-only data loss (this exact class bit us
// twice: a2091a5 and #298's durationMs). This test walks each root type and its
// CRD schema together and fails on the first Go path the schema cannot hold.

// crdRootTypes maps each chart CRD to the Go root type whose JSON shape it must
// carry. New CRDs must be registered here alongside their schema file.
var crdRootTypes = []struct {
	file string
	typ  reflect.Type
}{
	{"attempts.harmostes.dev.yaml", reflect.TypeOf(Attempt{})},
	{"workflows.harmostes.dev.yaml", reflect.TypeOf(Workflow{})},
	{"workflowtemplates.harmostes.dev.yaml", reflect.TypeOf(WorkflowTemplate{})},
	{"connectionprofiles.harmostes.dev.yaml", reflect.TypeOf(ConnectionProfile{})},
}

const crdDir = "../../chart/crds/"

// wellKnownLeaves are Go types whose JSON encoding is scalar/arbitrary and
// which CRD schemas represent as leaf schemas (string, anyOf, x-kubernetes-*).
func wellKnownLeaf(t reflect.Type) bool {
	s := t.String()
	switch {
	case strings.HasSuffix(s, ".Time"), // metav1.Time, v1.Time
		strings.HasSuffix(s, ".Duration"), // metav1.Duration
		strings.HasSuffix(s, ".Quantity"), // resource.Quantity
		strings.HasSuffix(s, ".IntOrString"),
		strings.HasSuffix(s, ".RawExtension"),
		strings.HasSuffix(s, ".JSON"):
		return true
	}
	return false
}

func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name := strings.Split(tag, ",")[0]
	if name == "" {
		if f.Anonymous {
			return "", true // inline embed: merge into the parent's schema node
		}
		return f.Name, false // untagged exported field: kubebuilder requires tags; treat as drift if reached
	}
	return name, true
}

// checkType walks a Go type and its schema node together, appending every Go
// path the schema cannot hold to issues.
func checkType(t reflect.Type, schema map[string]interface{}, path string, issues *[]string) {
	t = deref(t)

	if wellKnownLeaf(t) {
		return // presence at `path` already established by the caller
	}

	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return // []byte and named byte-slice types (jsontext.Value) are JSON leaves
	}

	switch t.Kind() {
	case reflect.Struct:
		name := t.String()
		if name == "metav1.ObjectMeta" || name == "v1.ObjectMeta" || name == "metav1.TypeMeta" || name == "v1.TypeMeta" {
			return // metadata/type meta are preserved wholesale by CRDs
		}
		props, _ := schema["properties"].(map[string]interface{})
		if props == nil {
			if preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
				return // nothing under this subtree can be pruned
			}
			*issues = append(*issues, fmt.Sprintf("%s: Go struct %s but schema has no properties (would prune everything under %s)", path, name, path))
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			child, ok := jsonName(f)
			if !ok {
				continue
			}
			childPath := child
			if path != "" {
				childPath = path + "." + child
			}
			if f.Anonymous && child == "" { // inline embed: same schema node
				checkType(f.Type, schema, path, issues)
				continue
			}
			ft := deref(f.Type).String()
			if strings.HasSuffix(ft, ".ObjectMeta") || strings.HasSuffix(ft, ".TypeMeta") {
				continue // preserved wholesale by CRDs, never pruned
			}
			node, ok := props[child].(map[string]interface{})
			if !ok {
				*issues = append(*issues, fmt.Sprintf("%s: field %q (Go %s) missing from CRD schema — the API server will prune it; add it to the chart CRD", childPath, child, f.Type))
				continue
			}
			checkType(f.Type, node, childPath, issues)
		}
	case reflect.Slice, reflect.Array:
		items, ok := schema["items"].(map[string]interface{})
		if !ok {
			*issues = append(*issues, fmt.Sprintf("%s: Go %s but schema has no items schema", path, t))
			return
		}
		checkType(t.Elem(), items, path+"[]", issues)
	case reflect.Map:
		if preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
			return // arbitrary-object map: keys and values are preserved as-is
		}
		ap, ok := schema["additionalProperties"].(map[string]interface{})
		if !ok {
			*issues = append(*issues, fmt.Sprintf("%s: Go %s but schema has no additionalProperties", path, t))
			return
		}
		checkType(t.Elem(), ap, path+"{}", issues)
	case reflect.Ptr, reflect.Interface:
		if t.Kind() == reflect.Ptr {
			checkType(t.Elem(), schema, path, issues)
		}
		// interface{} is arbitrary JSON — presence suffices
	default:
		// string/number/bool leaf: presence suffices
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// crdRootSchema extracts the v1alpha1 openAPIV3Schema root node from a CRD file.
func crdRootSchema(t *testing.T, file string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(crdDir + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var doc struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Schema struct {
					OpenAPIV3Schema map[string]interface{} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, v := range doc.Spec.Versions {
		if v.Name == "v1alpha1" {
			if v.Schema.OpenAPIV3Schema == nil {
				t.Fatalf("%s: v1alpha1 has no openAPIV3Schema", file)
			}
			return v.Schema.OpenAPIV3Schema
		}
	}
	t.Fatalf("%s: no v1alpha1 version", file)
	return nil
}

func TestCRDConformance(t *testing.T) {
	for _, tc := range crdRootTypes {
		t.Run(tc.file, func(t *testing.T) {
			root := crdRootSchema(t, tc.file)
			var issues []string
			checkType(tc.typ, root, "", &issues)
			for _, is := range issues {
				t.Errorf("%s: %s", tc.file, is)
			}
		})
	}
}
