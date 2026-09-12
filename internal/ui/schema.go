package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
)

// The CRDs whose OpenAPI schema GET /api/schema serves — imported from the
// API package, not restated (ADR-0012 §2: the schema is CRD-derived, read
// live from the cluster — never hand-written in the frontend; the same
// single-sourcing applies to the names). One document drives editor
// completion (#416), the topology palette (#417) and typed forms.
const (
	workflowsCRDName         = v1alpha1.WorkflowCRDName
	workflowTemplatesCRDName = v1alpha1.WorkflowTemplateCRDName
)

// handleSchema serves the OpenAPI v3 schemas of the Workflow and
// WorkflowTemplate CRDs (GET /api/schema). The response body is
//
//	{"workflow": <schema>, "workflowtemplate": <schema>}
//
// where each value is the CRD's storage version's openAPIV3Schema.
//
// Per-kind responses (#436): `?kind=workflow` / `?kind=workflowtemplate`
// serve one half with its OWN ETag, so a rollout of one CRD no longer
// invalidates the other — the template schema sits on the editor's critical
// path and must not be invalidated by an unrelated CRD change.
//
// Memoization (#436): CRD reads and schema derivation are cached behind a
// small TTL (schemaMemoTTL) so the editor's polling (#416) and conditional
// revalidation do not pay live cluster reads on every hit. The trade is
// deliberate and bounded: a CRD rollout invalidates cached copies within
// the TTL, not on the very next request — the old no-TTL property bought
// dev-first immediacy at the cost of a read per poll.
// CONTRACT: clients MUST treat ETags as opaque — the delimiter/format is an
// implementation detail that may switch to a content hash without notice
// (PR #427 review, L2).
func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	var wantWf, wantTmpl bool
	switch kindParam := r.URL.Query().Get("kind"); kindParam {
	case "":
		wantWf, wantTmpl = true, true
	case "workflow":
		wantWf = true
	case "workflowtemplate":
		wantTmpl = true
	default:
		s.writeAPIError(w, http.StatusBadRequest, "unknown kind — want workflow or workflowtemplate")
		return
	}

	wf, tmpl, errKind, err := s.schemaEntries(r.Context(), wantWf, wantTmpl)
	if err != nil {
		if errKind == workflowsCRDName || errKind == workflowTemplatesCRDName {
			s.schemaError(w, r, errKind, err)
			return
		}
		s.logger.Error("schema memo", "err", err)
		s.writeAPIError(w, http.StatusInternalServerError, "schema cache failure")
		return
	}

	var etag string
	switch {
	case wantWf && wantTmpl:
		etag = `"` + wf.rv + "-" + tmpl.rv + `"`
	case wantWf:
		etag = `"wf-` + wf.rv + `"`
	default:
		etag = `"tmpl-` + tmpl.rv + `"`
	}
	w.Header().Set("ETag", etag)
	// no-cache (not no-store): clients may KEEP the body but must revalidate
	// — the ETag dance below is the intended traffic pattern, so proxies and
	// browsers that send conditional requests get real 304s.
	w.Header().Set("Cache-Control", "no-cache")
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{}
	if wantWf {
		body["workflow"] = wf.schema
	}
	if wantTmpl {
		body["workflowtemplate"] = tmpl.schema
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Error("encode schema", "err", err)
	}
}

// schemaNow is the memo's clock (injectable in tests); nil-safe because
// test constructors may build the Server literal directly.
func (s *Server) schemaNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// schemaMemoCache is the Server's CRD cache for GET /api/schema.
type schemaMemoCache struct {
	mu      sync.Mutex
	expires time.Time
	wf      schemaEntry
	tmpl    schemaEntry
}

// schemaEntry is one memoized CRD half: the storage version's schema and
// the resourceVersion the ETag derives from.
type schemaEntry struct {
	schema *apiextensionsv1.JSONSchemaProps
	rv     string
}

// schemaMemoTTL bounds how long the CRD cache may answer without re-reading
// the cluster. Small on purpose: correctness on a CRD rollout lags at most
// this long, and the editor (#416) revalidates far more often than that.
const schemaMemoTTL = 30 * time.Second

// schemaEntries returns the requested halves, serving from the memo while
// it is fresh and re-reading (and refreshing) on expiry. On a read failure
// the returned errKind names the CRD — the caller maps it to the per-kind
// error response.
func (s *Server) schemaEntries(ctx context.Context, wantWf, wantTmpl bool) (wf, tmpl schemaEntry, errKind string, err error) {
	s.schemaMemo.mu.Lock()
	defer s.schemaMemo.mu.Unlock()

	if !s.schemaNow().Before(s.schemaMemo.expires) {
		read := func(name string, e *schemaEntry) error {
			schema, rv, err := s.crdOpenAPISchema(ctx, name)
			if err != nil {
				return err
			}
			*e = schemaEntry{schema: schema, rv: rv}
			return nil
		}
		if wantWf {
			errKind = workflowsCRDName
			if err = read(workflowsCRDName, &s.schemaMemo.wf); err != nil {
				return
			}
		}
		if wantTmpl {
			errKind = workflowTemplatesCRDName
			if err = read(workflowTemplatesCRDName, &s.schemaMemo.tmpl); err != nil {
				return
			}
		}
		s.schemaMemo.expires = s.schemaNow().Add(schemaMemoTTL)
	}
	return s.schemaMemo.wf, s.schemaMemo.tmpl, "", nil
}

// etagMatches implements RFC 9110 §8.8.3.2 If-None-Match comparison for the
// cases real clients send: `*` (any), a comma-separated candidate list, and
// weak validators (W/ prefix — for If-None-Match a weak match is a match).
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if ifNoneMatch == "*" {
		return true
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
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
// (the consumer's cue to degrade — no schema, no editor); Forbidden is the
// fingerprint of a drifted ClusterRoleBinding — surfaced as 403, not an
// anonymous 500; anything else is a 500.
func (s *Server) schemaError(w http.ResponseWriter, r *http.Request, crdName string, err error) {
	s.logger.Error("schema endpoint", "crd", crdName, "err", err)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	status := http.StatusInternalServerError
	switch {
	case apierrors.IsNotFound(err):
		status = http.StatusNotFound
	case apierrors.IsForbidden(err):
		status = http.StatusForbidden
	}
	http.Error(w, fmt.Sprintf("schema unavailable (%s)", crdName), status)
}
