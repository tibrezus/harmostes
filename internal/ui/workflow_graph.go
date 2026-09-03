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
	Workflow    string              `json:"workflow"`
	Disabled    bool                `json:"disabled"`
	GraphNative bool                `json:"graphNative"` // true = spec.graph is set (editable canvas); false = compiled from declarative spec (read-only)
	Source      v1alpha1.SourceSpec `json:"source"`
	Trigger     string              `json:"trigger"`
	Graph       v1alpha1.GraphSpec  `json:"graph"`
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
	ownerName := s.visibleOwner(identityFromContext(r.Context()))

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

	// Thin templateRef instances compile from their merged spec (template
	// defaults + instance config) so the canvas shows the real structure.
	resolved := s.resolveWorkflow(r.Context(), &wf)
	wf = resolved

	// If the Workflow has an explicit spec.graph (graph-native mode), return it
	// directly. Otherwise, compile the declarative spec (prepare → agent → deploy)
	// into a graph for the read-only canvas view.
	var gs v1alpha1.GraphSpec
	if wf.Spec.Graph != nil {
		gs = *wf.Spec.Graph
	} else {
		gs = graph.CompileWorkflow(&wf)
	}

	s.writeJSON(w, http.StatusOK, workflowGraphResponse{
		Workflow:    name,
		Disabled:    wf.Spec.Disabled,
		GraphNative: wf.Spec.Graph != nil,
		Source:      wf.Spec.Source,
		Trigger:     trigger,
		Graph:       gs,
	})
}

// workflowGraphPutRequest is the JSON body for PUT /api/workflows/{name}/graph.
// It carries the graph to save as spec.graph on the Workflow CR.
