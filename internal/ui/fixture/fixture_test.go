package fixture

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The world parses and carries the fixture owner on every object.
func TestFixture_Objects(t *testing.T) {
	objs, err := Objects("fixture-ns")
	if err != nil {
		t.Fatalf("Objects: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("objects = %d, want 2 workflows", len(objs))
	}
	names := map[string]bool{}
	for _, o := range objs {
		wf, ok := o.(*v1alpha1.Workflow)
		if !ok {
			t.Fatalf("object %T is not a Workflow", o)
		}
		if wf.Labels[v1alpha1.OwnerLabel] != DevUser {
			t.Errorf("workflow %s missing owner label", wf.Name)
		}
		if wf.Spec.Graph == nil || len(wf.Spec.Graph.Nodes) == 0 {
			t.Errorf("workflow %s has no graph", wf.Name)
		}
		names[wf.Name] = true
	}
	for _, want := range []string{"pr-review-demo", "merge-sync-demo"} {
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
		if a.Labels[v1alpha1.OwnerLabel] != DevUser {
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /runs = %d, want 200", resp.StatusCode)
	}
}
