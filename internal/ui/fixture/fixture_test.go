package fixture

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/ui"
)

// The world parses and carries the fixture owner on every object.
func TestFixture_Objects(t *testing.T) {
	objs, err := Objects("fixture-ns")
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("objects = %d, want 3 workflows", len(objs))
	}
	names := map[string]bool{}
	for _, o := range objs {
		wf, ok := o.(*v1alpha1.Workflow)
		if !ok {
			t.Fatalf("object %T is not a Workflow", o)
		}
		if wf.Labels[v1alpha1.OwnerLabel] != ui.DevOwnerPrefix+DevUser {
			t.Errorf("workflow %s missing owner label", wf.Name)
		}
		// Graph-native workflows carry their graph; the thin instance (r1 of
		// the world) instead carries only a templateRef — its merged shape is
		// resolved at render time (#417).
		if wf.Spec.Graph == nil && wf.Spec.TemplateRef == "" {
			t.Errorf("workflow %s has neither graph nor templateRef", wf.Name)
		}
		names[wf.Name] = true
	}
	for _, want := range []string{"pr-review-demo", "merge-sync-demo", "pr-review-instance"} {
		if !names[want] {
			t.Errorf("workflow %q missing", want)
		}
	}
}

// The three attempts cover the narrative states with honest owner labels.
func TestFixture_Attempts(t *testing.T) {
	atts, err := Attempts("fixture-ns")
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(atts))
	}
	phases := map[string]int{}
	for _, o := range atts {
		a := o.(*v1alpha1.Attempt)
		if a.Labels[v1alpha1.OwnerLabel] != ui.DevOwnerPrefix+DevUser {
			t.Errorf("attempt %s missing owner label", a.Name)
		}
		if a.Spec.WorkflowRef == "" || a.Spec.Objective.Kind == "" {
			t.Errorf("attempt %s missing objective/workflowRef", a.Name)
		}
		phases[a.Status.Phase]++
	}
	if phases["validated"] != 1 || phases["reconciling"] != 1 || phases["superseded"] != 1 {
		t.Errorf("phase distribution %v, want one of each terminal+running", phases)
	}
}

// NewServer serves the world through the real Routes() — the same handler
// the binary mounts; `-fixture` and the component tests ride identical code.
func TestFixture_NewServer_ServesWorld(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := NewServer("fixture-ns", logger)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs", nil)
	req.Header.Set("X-Harmostes-Dev-User", DevUser)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /runs = %d, want 200", resp.StatusCode)
	}
}

// The fixture's graph node-type enum must mirror the chart CRD's — the
// topology palette (#417) derives from it, and drift here would make the
// fixture world disagree with dev about what a node can be.
func TestFixture_GraphNodeTypeEnumMirrorsChartCRD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "chart", "crds", "workflows.harmostes.dev.yaml"))
	if err != nil {
		t.Fatalf("read chart CRD: %v", err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse chart CRD: %v", err)
	}
	// walk: spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.graph.properties.nodes.items.properties.type.enum
	var walk func(node any, path ...string) any
	walk = func(node any, path ...string) any {
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
	enumNode := walk(crd, "spec", "versions", "0", "schema", "openAPIV3Schema",
		"properties", "spec", "properties", "graph", "properties", "nodes",
		"items", "properties", "type", "enum")
	enumList, ok := enumNode.([]any)
	if !ok || len(enumList) == 0 {
		t.Fatal("chart CRD carries no graph.nodes.type enum — update the fixture helper deliberately")
	}
	got := map[string]bool{}
	for _, e := range graphNodeTypeEnum() {
		var name string
		if err := json.Unmarshal(e.Raw, &name); err != nil {
			t.Fatalf("unmarshal enum entry: %v", err)
		}
		got[name] = true
	}
	for _, want := range enumList {
		w, ok := want.(string)
		if !ok {
			continue
		}
		if !got[w] {
			t.Errorf("fixture enum missing %q (chart CRD has it)", w)
		}
	}
}
