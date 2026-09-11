package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// schemaCRD builds a minimal but structurally truthful CustomResourceDefinition
// object: one storage version carrying an openAPIV3Schema with a marker
// property. The schema endpoint serves exactly this document — the fixture
// proves the endpoint is a CRD projection, not a hand-written blob.
func schemaCRD(name, kind, markerProp string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			ResourceVersion: "rv-" + markerProp,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "harmostes.dev",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {Type: "object", Description: markerProp},
						},
					},
				},
			}},
		},
	}
}

// TestSchemaEndpoint pins ADR-0012 §2: GET /api/schema serves the CRD-derived
// OpenAPI schemas for both harmostes kinds, cache-validated by an ETag built
// from the CRDs' resourceVersions, and 404s when the CRDs are absent (the
// consumer's cue to degrade — no schema, no editor).
func TestSchemaEndpoint(t *testing.T) {
	s := workflowTestServer(
		schemaCRD(workflowsCRDName, "Workflow", "workflow-spec-marker"),
		schemaCRD(workflowTemplatesCRDName, "WorkflowTemplate", "template-spec-marker"),
	)

	req := httptest.NewRequest(http.MethodGet, "/api/schema", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/schema status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	for _, key := range []string{"workflow", "workflowtemplate"} {
		if _, ok := body[key]; !ok {
			t.Errorf("schema body missing key %q", key)
		}
	}
	if !strings.Contains(string(body["workflow"]), "workflow-spec-marker") {
		t.Error("workflow schema is not the CRD's openAPIV3Schema (marker property missing)")
	}
	if !strings.Contains(string(body["workflowtemplate"]), "template-spec-marker") {
		t.Error("workflowtemplate schema is not the CRD's openAPIV3Schema (marker property missing)")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache (store, but revalidate via ETag)", cc)
	}

	// ETag revalidation: every If-None-Match form real clients send gets a
	// 304 — exact match, weak validator, star, and candidate lists (RFC 9110
	// §8.8.3.2). A CRD rollout (new resourceVersion) invalidates the cache.
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag set — schema caching impossible")
	}
	variants := map[string]string{
		"exact match":    etag,
		"weak validator": "W/" + etag,
		"star":           "*",
		"candidate list": `"rv-workflow-spec-marker-rv-template-spec-marker", ` + etag,
	}
	for name, inm := range variants {
		req = httptest.NewRequest(http.MethodGet, "/api/schema", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		req.Header.Set("If-None-Match", inm)
		rec = httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match (%s) status = %d, want 304", name, rec.Code)
		}
	}

	// A non-matching validator is a full 200 — the client's copy is stale.
	req = httptest.NewRequest(http.MethodGet, "/api/schema", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	req.Header.Set("If-None-Match", "\"stale-rv\"")
	rec = httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("stale If-None-Match status = %d, want 200", rec.Code)
	}
}

// TestSchemaEndpoint_CRDMissing: without the CRDs the endpoint fails honestly
// with 404 — never an empty schema that would silently hand the editor a
// fabricated contract.
func TestSchemaEndpoint_CRDMissing(t *testing.T) {
	s := workflowTestServer()

	req := httptest.NewRequest(http.MethodGet, "/api/schema", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("schema without CRDs status = %d, want 404", rec.Code)
	}
}
