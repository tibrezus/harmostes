package v1alpha1

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tibrezus/harmostes/internal/crdwalk"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
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
		{File: WorkflowCRDFile, Type: reflect.TypeOf(Workflow{})},
		{File: WorkflowTemplateCRDFile, Type: reflect.TypeOf(WorkflowTemplate{})},
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

// TestCRDFilesStrictDecode (issue #354): the walk above proves the schema
// has every Go field; it cannot prove the schema FILE itself decodes
// strictly, because LoadSchema parses leniently. A flow-style one-liner
// with an unquoted comma in a description parses as unknown schema KEYS
// ("cleared by a human request": null shipped to main inside #344), which
// lenient YAML accepts, CI never sees, and every strict consumer — Flux's
// server-side apply, kubectl --dry-run=server — rejects, blocking the CRD
// sync while the API server keeps pruning the new status fields live.
// Strict-decode every chart CRD as the API server would see it.
func TestCRDFilesStrictDecode(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "chart", "crds", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob chart/crds: %v (%d files)", err, len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for i, doc := range strings.Split(string(raw), "\n---") {
				if strings.TrimSpace(doc) == "" {
					continue
				}
				var crd apiextensionsv1.CustomResourceDefinition
				// UnmarshalStrict == YAML→JSON→json.DisallowUnknownFields:
				// unknown keys at ANY schema depth fail here, exactly as the
				// API server's strict decoder fails them in dry-run.
				if err := yaml.UnmarshalStrict([]byte(doc), &crd); err != nil {
					t.Fatalf("doc %d does not strict-decode as a CRD (unknown keys reach Flux/kubectl as rejections): %v", i+1, err)
				}
			}
		})
	}
}
