//go:build integration

package integration

// The acceptance matrix (#315 follow-up): the crdwalk PROBE for every CRD
// root is written through the REAL API server, read back, and its stored
// leaf values are compared against the probe's. Every field the walk
// discovers inherits this round-trip canary automatically — the pruning
// class (#277/#289: a stale chart CRD silently zeroes unlisted status
// fields in production) is caught for ANY field, not just hand-picked ones.

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/crdwalk"
)

func TestCRDAcceptanceMatrix(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	dir := filepath.Join("..", "..", "chart", "crds") + string(filepath.Separator)
	roots := []crdwalk.Root{
		{File: "attempts.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.Attempt{})},
		{File: "workflows.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.Workflow{})},
		{File: "workflowtemplates.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.WorkflowTemplate{})},
		{File: "connectionprofiles.harmostes.dev.yaml", Type: reflect.TypeOf(v1alpha1.ConnectionProfile{})},
	}

	for _, root := range roots {
		t.Run(root.File, func(t *testing.T) {
			schema, err := crdwalk.LoadSchema(dir, root.File)
			if err != nil {
				t.Fatalf("load schema: %v", err)
			}
			probeAny, leaves, issues := crdwalk.Analyze(root.Type, schema)
			for _, is := range issues {
				t.Errorf("conformance: %s", is)
			}
			if len(leaves) == 0 {
				t.Fatal("probe discovered no leaves — the walk is not reaching the tree")
			}

			obj, ok := probeAny.(client.Object)
			if !ok {
				t.Fatalf("probe is not a client.Object: %T", probeAny)
			}
			obj.SetName(strings.TrimSuffix(root.File, ".harmostes.dev.yaml"))
			obj.SetNamespace("default")

			// Create's server response decodes OVER obj; for status-
			// subresource kinds the response carries an EMPTY status (the
			// #277 drop), so the probe status must be saved and restored
			// before the explicit status write. Kinds without a Status
			// field (workflowtemplates) skip the push entirely.
			statusField := reflect.ValueOf(obj).Elem().FieldByName("Status")
			hasStatus := statusField.IsValid()
			var probeStatus any
			if hasStatus {
				probeStatus = statusField.Interface()
			}

			if err := c.Create(ctx, obj); err != nil {
				t.Fatalf("create probe: %v — the probe violates the CRD schema; fix crdwalk's value generation", err)
			}

			if hasStatus {
				// Status subresource: create-time status is dropped (the
				// #277 fake-vs-real divergence), so status-bearing roots
				// push status explicitly. Kinds with a Status field but no
				// subresource carried status in the create body and
				// Status().Update 404s — that is expected.
				statusField.Set(reflect.ValueOf(probeStatus))
				if err := c.Status().Update(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
					t.Fatalf("status update: %v", err)
				}
			}

			got := reflect.New(root.Type).Interface().(client.Object)
			if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: obj.GetName()}, got); err != nil {
				t.Fatalf("get probe: %v", err)
			}

			want := map[string]string{}
			for _, l := range leaves {
				want[l.Path] = l.Value
			}
			gotMap := map[string]string{}
			for _, l := range crdwalk.Collect(root.Type, schema, got) {
				gotMap[l.Path] = l.Value
			}

			var pruned, altered []string
			for path, v := range want {
				g, ok := gotMap[path]
				if !ok {
					pruned = append(pruned, path)
					continue
				}
				if g != v {
					altered = append(altered, fmt.Sprintf("%s: probe %q → stored %q", path, v, g))
				}
			}
			for path := range gotMap {
				if _, ok := want[path]; !ok {
					altered = append(altered, path+": appeared only after the round-trip")
				}
			}
			sort.Strings(pruned)
			sort.Strings(altered)
			for _, p := range pruned {
				t.Errorf("PRUNED: %s did not survive the API-server round-trip — chart CRD schema is stale (the #277/#289 class)", p)
			}
			for _, a := range altered {
				t.Errorf("ALTERED: %s", a)
			}
		})
	}
}
