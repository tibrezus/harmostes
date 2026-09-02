package crdwalk

import (
	"path/filepath"
	"reflect"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The probe must be deterministic (a read-back probe is compared against the
// original leaf-for-leaf) and every CRD root must yield leaves.
func TestProbeDeterministicAndPopulated(t *testing.T) {
	dir := filepath.Join("..", "..", "chart", "crds") + string(filepath.Separator)
	roots := []Root{
		{File: "attempts.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.Attempt{})},
		{File: "workflows.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.Workflow{})},
		{File: "workflowtemplates.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.WorkflowTemplate{})},
		{File: "connectionprofiles.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.ConnectionProfile{})},
	}
	for _, root := range roots {
		schema, err := LoadSchema(dir, root.File)
		if err != nil {
			t.Fatalf("%s: %v", root.File, err)
		}
		p1, l1, issues := Analyze(root.Type, schema)
		if len(issues) > 0 {
			t.Errorf("%s: %d conformance issues (first: %s)", root.File, len(issues), issues[0])
		}
		if len(l1) < 10 {
			t.Errorf("%s: only %d leaves discovered — the walk is not reaching the tree", root.File, len(l1))
		}
		p2, l2, _ := Analyze(root.Type, schema)
		if len(l1) != len(l2) {
			t.Fatalf("%s: leaf count differs between probes: %d vs %d", root.File, len(l1), len(l2))
		}
		for i := range l1 {
			if l1[i] != l2[i] {
				t.Errorf("%s: probe not deterministic at %s: %q vs %q", root.File, l1[i].Path, l1[i].Value, l2[i].Value)
			}
		}
		_ = p1
		_ = p2
	}
}
