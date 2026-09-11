package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The CRDs whose OpenAPI schema GET /api/schema serves. ADR-0012 §2: the
// schema is CRD-derived, read live from the cluster — never hand-written in
// the frontend. Editor completion (#416), the topology palette (#417) and
// typed forms all render from this one document.
const (
	workflowsCRDName         = "workflows.harmostes.dev"
	workflowTemplatesCRDName = "workflowtemplates.harmostes.dev"
)

// handleSchema serves the OpenAPI v3 schemas of the Workflow and
// WorkflowTemplate CRDs (GET /api/schema). The response body is
//
//	{"workflow": <schema>, "workflowtemplate": <schema>}
//
// where each value is the CRD's storage version's openAPIV3Schema. The ETag
// is derived from the CRDs' resourceVersions, so schema consumers get real
// caching for free: an unchanged CRD pair answers If-None-Match with 304, and
// a CRD rollout invalidates every cached copy on the next request — dev-first
// correctness without a TTL to tune.
func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	wfSchema, wfRV, err := s.crdOpenAPISchema(r.Context(), workflowsCRDName)
	if err != nil {
		s.schemaError(w, r, workflowsCRDName, err)
		return
	}
	tmplSchema, tmplRV, err := s.crdOpenAPISchema(r.Context(), workflowTemplatesCRDName)
	if err != nil {
		s.schemaError(w, r, workflowTemplatesCRDName, err)
		return
	}

	etag := `"` + wfRV + "-" + tmplRV + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"workflow":         wfSchema,
		"workflowtemplate": tmplSchema,
	}); err != nil {
		s.logger.Error("encode schema", "err", err)
	}
}

// crdOpenAPISchema reads one CRD and returns its storage version's
// openAPIV3Schema plus the CRD's resourceVersion (for the ETag).
func (s *Server) crdOpenAPISchema(ctx context.Context, name string) (*apiextensionsv1.JSONSchemaProps, string, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: name}, &crd); err != nil {
		return nil, "", fmt.Errorf("read CRD: %w", err)
	}
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Storage && v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
			return v.Schema.OpenAPIV3Schema, crd.ResourceVersion, nil
		}
	}
	return nil, "", fmt.Errorf("CRD %s has no storage version with an openAPIV3Schema", name)
}

// schemaError maps schema-read failures to responses: a missing CRD is a 404
// (the consumer's cue to degrade — no schema, no editor), anything else is a
// 500.
func (s *Server) schemaError(w http.ResponseWriter, r *http.Request, crdName string, err error) {
	s.logger.Error("schema endpoint", "crd", crdName, "err", err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	status := http.StatusInternalServerError
	if apierrors.IsNotFound(err) {
		status = http.StatusNotFound
	}
	http.Error(w, fmt.Sprintf("schema unavailable (%s)", crdName), status)
}
