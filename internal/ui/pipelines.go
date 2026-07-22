package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/rbac"
)

// ---------------------------------------------------------------------------
// Audit logging (G8 enterprise)
// ---------------------------------------------------------------------------
//
// Every pipeline CRUD operation is logged as a structured audit event. These
// events flow to stdout (JSON) → OTel collector → SigNoz, where they can be
// queried to answer "who changed pipeline X, and when?"

// auditLog emits a structured audit event.
func (s *Server) auditLog(action, resource, user string, extra ...any) {
	attrs := []any{"audit", "true", "action", action, "resource", resource, "user", user}
	attrs = append(attrs, extra...)
	s.logger.Info("audit: "+action, attrs...)
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Error("write json response", "err", err)
	}
}

func (s *Server) writeAPIError(w http.ResponseWriter, status int, format string, args ...any) {
	s.writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// ---------------------------------------------------------------------------
// Pipeline CRUD API
// ---------------------------------------------------------------------------
//
// The SPA calls these JSON endpoints to list, get, create/update, and delete
// Pipeline CRs. All endpoints are behind the auth middleware — the Authentik
// reverse proxy injects identity headers on SPA fetch() requests too.

// pipelineSummary is the list-item view (no spec, just metadata).
type pipelineSummary struct {
	Name      string `json:"name"`
	Nodes     int    `json:"nodes"`
	Trigger   string `json:"trigger"`
	Phase     string `json:"phase"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// handlePipelineAPIList returns all Pipeline CRs owned by the user.
func (s *Server) handlePipelineAPIList(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	var list v1alpha1.PipelineList
	opts := []client.ListOption{client.InNamespace(s.namespace)}
	if ownerName != "" {
		opts = append(opts, client.MatchingLabels{v1alpha1.OwnerLabel: ownerName})
	}
	if err := s.k8sClient.List(r.Context(), &list, opts...); err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "list pipelines: %v", err)
		return
	}

	summaries := make([]pipelineSummary, 0, len(list.Items))
	for _, p := range list.Items {
		updatedAt := ""
		if !p.Status.LastRunAt.IsZero() {
			updatedAt = p.Status.LastRunAt.Format("2006-01-02 15:04")
		}
		summaries = append(summaries, pipelineSummary{
			Name:      p.Name,
			Nodes:     len(p.Spec.Graph.Nodes),
			Trigger:   p.Spec.Trigger.Type,
			Phase:     p.Status.Phase,
			UpdatedAt: updatedAt,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})

	s.writeJSON(w, http.StatusOK, map[string]any{"pipelines": summaries})
}

// handlePipelineAPIGet returns a single Pipeline CR.
func (s *Server) handlePipelineAPIGet(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	name := r.PathValue("name")
	var pipe v1alpha1.Pipeline
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &pipe); err != nil {
		s.writeAPIError(w, http.StatusNotFound, "pipeline %q not found", name)
		return
	}

	// Ownership check: hide other users' pipelines (don't leak existence).
	if ownerName != "" && pipe.Labels[v1alpha1.OwnerLabel] != ownerName {
		s.writeAPIError(w, http.StatusNotFound, "pipeline %q not found", name)
		return
	}

	s.writeJSON(w, http.StatusOK, pipe)
}

// pipelinePutRequest is the JSON body for PUT /api/pipelines/{name}.
type pipelinePutRequest struct {
	Spec v1alpha1.PipelineSpec `json:"spec"`
}

// handlePipelineAPIPut creates or updates a Pipeline CR.
func (s *Server) handlePipelineAPIPut(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	name := r.PathValue("name")

	var req pipelinePutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeAPIError(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}

	// Basic validation.
	if req.Spec.Trigger.Type == "" {
		req.Spec.Trigger.Type = "manual"
	}

	// RBAC enforcement (G8): check if the user is authorized to use all node
	// types in the graph. Deployment node types (vela-app, flux-reconcile) are
	// restricted to ops/admin by default.
	var userGroups []string
	if owner != nil {
		userGroups = owner.Groups
	}
	if s.nodePolicy != nil {
		if err := s.nodePolicy.Authorize(userGroups, req.Spec.Graph.Nodes); err != nil {
			var rbacErr *rbac.Error
			if errors.As(err, &rbacErr) {
				s.auditLog("pipeline.create_denied", name, ownerName, "violations", rbacErr.Violations)
				s.writeJSON(w, http.StatusForbidden, map[string]any{
					"error":      "unauthorized node types in pipeline graph",
					"violations": rbacErr.Violations,
				})
				return
			}
			s.writeAPIError(w, http.StatusForbidden, "authorization error: %v", err)
			return
		}
	}

	// Check if the Pipeline already exists (update vs create).
	var existing v1alpha1.Pipeline
	err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &existing)
	if err == nil {
		// Update existing.
		if ownerName != "" && existing.Labels[v1alpha1.OwnerLabel] != ownerName {
			s.writeAPIError(w, http.StatusForbidden, "pipeline %q is owned by another user", name)
			return
		}
		existing.Spec = req.Spec
		// Provenance annotations (G8): record who last modified the pipeline.
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations["harmostes.dev/last-modified-by"] = ownerName
		existing.Annotations["harmostes.dev/last-modified-at"] = time.Now().UTC().Format(time.RFC3339)
		if err := s.k8sClient.Update(r.Context(), &existing); err != nil {
			s.writeAPIError(w, http.StatusInternalServerError, "update pipeline: %v", err)
			return
		}
		s.auditLog("pipeline.update", name, ownerName, "nodes", len(req.Spec.Graph.Nodes))
		s.writeJSON(w, http.StatusOK, existing)
		return
	}

	// Create new.
	pipe := v1alpha1.Pipeline{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels:    map[string]string{},
		},
		Spec: req.Spec,
	}
	if ownerName != "" {
		pipe.Labels[v1alpha1.OwnerLabel] = ownerName
	}
	// Provenance annotations (G8): record who created the pipeline.
	if pipe.Annotations == nil {
		pipe.Annotations = map[string]string{}
	}
	pipe.Annotations["harmostes.dev/created-by"] = ownerName
	pipe.Annotations["harmostes.dev/last-modified-by"] = ownerName
	pipe.Annotations["harmostes.dev/last-modified-at"] = time.Now().UTC().Format(time.RFC3339)

	if err := s.k8sClient.Create(r.Context(), &pipe); err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "create pipeline: %v", err)
		return
	}
	s.auditLog("pipeline.create", name, ownerName, "nodes", len(req.Spec.Graph.Nodes))
	s.writeJSON(w, http.StatusCreated, pipe)
}

// handlePipelineAPIDelete deletes a Pipeline CR.
func (s *Server) handlePipelineAPIDelete(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	name := r.PathValue("name")
	var pipe v1alpha1.Pipeline
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &pipe); err != nil {
		s.writeAPIError(w, http.StatusNotFound, "pipeline %q not found", name)
		return
	}
	if ownerName != "" && pipe.Labels[v1alpha1.OwnerLabel] != ownerName {
		s.writeAPIError(w, http.StatusForbidden, "pipeline %q is owned by another user", name)
		return
	}
	if err := s.k8sClient.Delete(r.Context(), &pipe); err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "delete pipeline: %v", err)
		return
	}
	s.auditLog("pipeline.delete", name, ownerName)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------------
// SPA serving
// ---------------------------------------------------------------------------

// handleSPA serves the React SPA's index.html. The SPA handles client-side
// routing for /pipelines and /pipelines/{name}. This handler is a catch-all
// for the SPA routes.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(staticFS, "static/spa/index.html")
	if err != nil {
		http.Error(w, "SPA not built (run npm run build in web/)", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// isValidPipelineName checks that a name is a valid k8s resource name
// (DNS-1123 subdomain).
func isValidPipelineName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '.' {
			return false
		}
	}
	// Must start and end with alphanumeric.
	return name[0] != '-' && name[0] != '.' && name[len(name)-1] != '-' && name[len(name)-1] != '.'
}

// sanitizePipelineName converts to lowercase and replaces invalid chars.
func sanitizePipelineName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == ' ', c == '_', c == '-':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
