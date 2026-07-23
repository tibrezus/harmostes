package ui

import (
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/tibrezus/harmostes/api/v1alpha1"
	"github.com/tibrezus/harmostes/internal/graph"
)

// workflowGraphResponse is the JSON returned by GET /api/workflows/{name}/graph.
// It carries the compiled GraphSpec plus enough metadata for the canvas to
// render a meaningful header (workflow name, trigger type, disabled state).
type workflowGraphResponse struct {
	Workflow string              `json:"workflow"`
	Disabled bool                `json:"disabled"`
	Source   v1alpha1.SourceSpec `json:"source"`
	Trigger  string              `json:"trigger"`
	Graph    v1alpha1.GraphSpec  `json:"graph"`
}

// handleWorkflowGraphAPI compiles a Workflow CR's spec into a pipeline graph
// and returns it as JSON. This gives every existing workflow a canvas
// representation via graph.CompileWorkflow(), bridging the declarative Workflow
// spec (prepare → agent → deploy) into the graph model that React Flow renders.
//
// The graph is READ-ONLY: it is derived from the Workflow spec, not stored
// separately. Changes to the Workflow spec (via the form UI or GitOps)
// automatically update the canvas view on reload.
//
// Route: GET /api/workflows/{name}/graph
func (s *Server) handleWorkflowGraphAPI(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	name := r.PathValue("name")
	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &wf); err != nil {
		s.writeAPIError(w, http.StatusNotFound, "workflow %q not found", name)
		return
	}

	// Ownership check: hide other users' workflows (don't leak existence).
	if ownerName != "" && wf.Labels[v1alpha1.OwnerLabel] != ownerName {
		s.writeAPIError(w, http.StatusNotFound, "workflow %q not found", name)
		return
	}

	trigger := wf.Spec.Source.Kind
	if wf.Spec.Scaling != nil && wf.Spec.Scaling.Kind != "" {
		trigger = wf.Spec.Scaling.Kind
	}
	if wf.Spec.Source.Webhook != nil {
		trigger = "webhook"
	}

	s.writeJSON(w, http.StatusOK, workflowGraphResponse{
		Workflow: name,
		Disabled: wf.Spec.Disabled,
		Source:   wf.Spec.Source,
		Trigger:  trigger,
		Graph:    graph.CompileWorkflow(&wf),
	})
}
