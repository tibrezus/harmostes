package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestSchemaEndpoint_PerKindETag (#436): `?kind=` serves one half with its
// own ETag; a rollout of one CRD invalidates only that half; the combined
// response keeps its combined ETag; and the memo is transparent except for
// its bounded TTL staleness.
func TestSchemaEndpoint_PerKindETag(t *testing.T) {
	wfCRD := schemaCRD(workflowsCRDName, "Workflow", "workflow-marker")
	tmplCRD := schemaCRD(workflowTemplatesCRDName, "WorkflowTemplate", "template-marker")
	s := workflowTestServer(wfCRD, tmplCRD)

	clock := time.Unix(1700000000, 0).UTC()
	s.now = func() time.Time { return clock }

	getETag := func(url string) (int, string, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("X-Authentik-Username", "alice")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("ETag"), rec.Body.String()
	}

	// Warm the memo, then per-kind responses carry per-kind validators.
	code, etag, _ := getETag("/api/schema")
	if code != http.StatusOK || !strings.HasPrefix(etag, `"`+"rv-workflow-marker") {
		t.Fatalf("combined warm-up: code=%d etag=%q", code, etag)
	}
	code, wfETag1, wfBody := getETag("/api/schema?kind=workflow")
	if code != http.StatusOK || wfETag1 != `"wf-rv-workflow-marker"` {
		t.Fatalf("kind=workflow: code=%d etag=%q", code, wfETag1)
	}
	if !strings.Contains(wfBody, "workflow-marker") || strings.Contains(wfBody, "template-marker") {
		t.Error("kind=workflow must serve exactly the workflow half")
	}
	_, tmplETag1, _ := getETag("/api/schema?kind=workflowtemplate")
	if tmplETag1 != `"tmpl-rv-template-marker"` {
		t.Fatalf("kind=workflowtemplate etag = %q", tmplETag1)
	}

	// Within the TTL the memo serves stale copies even after a CRD change —
	// the bounded staleness the handler's contract documents. (The rollout
	// is modeled as delete+create: the fake client requires numeric
	// resourceVersions for updates, and the seeded CRDs use marker RVs.)
	updated := schemaCRD(workflowsCRDName, "Workflow", "workflow-marker-2")
	updated.ResourceVersion = ""
	if err := s.k8sClient.Delete(context.Background(),
		&apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: workflowsCRDName}}); err != nil {
		t.Fatalf("delete CRD: %v", err)
	}
	if err := s.k8sClient.Create(context.Background(), updated); err != nil {
		t.Fatalf("create CRD: %v", err)
	}
	if _, etag, _ := getETag("/api/schema?kind=workflow"); etag != wfETag1 {
		t.Errorf("memo must serve the fresh-in-TTL copy: etag=%q want %q", etag, wfETag1)
	}

	// After expiry: the workflow half revalidates to the new RV, the
	// template half keeps its old validator — one rollout must not
	// invalidate the editor's other half.
	clock = clock.Add(2 * schemaMemoTTL)
	_, wfETag2, _ := getETag("/api/schema?kind=workflow")
	if wfETag2 == wfETag1 || !strings.HasPrefix(wfETag2, `"wf-`) {
		t.Fatalf("workflow etag did not revalidate after expiry: %q", wfETag2)
	}
	_, tmplETag2, _ := getETag("/api/schema?kind=workflowtemplate")
	if tmplETag2 != tmplETag1 {
		t.Errorf("template etag moved with an unrelated CRD rollout: %q", tmplETag2)
	}

	// The combined ETag decomposes into the two raw RVs the per-kind
	// validators are prefixed from.
	wfRV := strings.TrimSuffix(strings.TrimPrefix(wfETag2, `"wf-`), `"`)
	tmplRV := strings.TrimSuffix(strings.TrimPrefix(tmplETag1, `"tmpl-`), `"`)
	_, combined, _ := getETag("/api/schema")
	if combined != `"`+wfRV+"-"+tmplRV+`"` {
		t.Errorf("combined etag %q does not decompose into %q/%q", combined, wfRV, tmplRV)
	}

	// Unknown kind is a named 400, never a silent combined response.
	if code, _, _ := getETag("/api/schema?kind=plugin"); code != http.StatusBadRequest {
		t.Errorf("unknown kind = %d, want 400", code)
	}
}
