package v1alpha1

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tibrezus/harmostes/internal/crdwalk"
)

// CRD-conformance: every Go field reachable from a CRD root type must exist in
// the chart's CRD schema. The fake controller-runtime client used in tests does
// not validate against CRDs, so a Go field added without its schema property is
// invisible to the whole test suite while the real API server prunes the field
// on every Status().Patch — production-only data loss (this exact class bit us
// twice: a2091a5 and #298's durationMs).
//
// The walk itself lives in internal/crdwalk so the integration tier can reuse
// it: the same walk that reports conformance issues here also BUILDS the
// probe instance whose round-trip through the real API server proves field
// survival (test/integration) — new fields inherit that acceptance canary
// automatically instead of waiting for a hand-picked test (#315).
func TestCRDConformance(t *testing.T) {
	roots := []crdwalk.Root{
		{File: "attempts.harmostes.dev.yaml", Type: reflect.TypeOf(Attempt{})},
		{File: "workflows.harmostes.dev.yaml", Type: reflect.TypeOf(Workflow{})},
		{File: "workflowtemplates.harmostes.dev.yaml", Type: reflect.TypeOf(WorkflowTemplate{})},
		{File: "connectionprofiles.harmostes.dev.yaml", Type: reflect.TypeOf(ConnectionProfile{})},
	}
	crdDir := filepath.Join("..", "..", "chart", "crds") + string(filepath.Separator)
	for _, root := range roots {
		t.Run(root.File, func(t *testing.T) {
			schema, err := crdwalk.LoadSchema(crdDir, root.File)
			if err != nil {
				t.Fatalf("load schema: %v", err)
			}
			for _, is := range crdwalk.Check(root.Type, schema) {
				t.Errorf("%s: %s", root.File, is)
			}
		})
	}
}
