package ui

import (
	"encoding/json"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
type workflowGraphPutRequest struct {
	Graph v1alpha1.GraphSpec `json:"graph"`
}

// handleWorkflowGraphPut saves a graph to a Workflow CR's spec.graph field,
// converting it to graph-native mode. This is the "canvas → code" direction:
// the user edits the graph on the canvas, and the changes are persisted to the
// Workflow CR.
//
// If the Workflow was previously declarative (no spec.graph), this converts it
// to graph-native mode. The Prepare/Agent/Deploy fields are preserved for
// reference but ignored at execution time (the graph executor takes over).
//
// Route: PUT /api/workflows/{name}/graph
func (s *Server) handleWorkflowGraphPut(w http.ResponseWriter, r *http.Request) {
	owner := identityFromContext(r.Context())
	ownerName := ""
	if owner != nil {
		ownerName = owner.Username
	}

	name := r.PathValue("name")
	var req workflowGraphPutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeAPIError(w, http.StatusBadRequest, "invalid JSON body: %v", err)
		return
	}

	if len(req.Graph.Nodes) == 0 {
		s.writeAPIError(w, http.StatusBadRequest, "graph must have at least one node")
		return
	}

	var wf v1alpha1.Workflow
	if err := s.k8sClient.Get(r.Context(), client.ObjectKey{Namespace: s.namespace, Name: name}, &wf); err != nil {
		s.writeAPIError(w, http.StatusNotFound, "workflow %q not found", name)
		return
	}

	if ownerName != "" && wf.Labels[v1alpha1.OwnerLabel] != ownerName {
		s.writeAPIError(w, http.StatusForbidden, "workflow %q is owned by another user", name)
		return
	}

	base := wf.DeepCopy()
	wf.Spec.Graph = &req.Graph
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations["harmostes.dev/last-modified-by"] = ownerName
	wf.Annotations["harmostes.dev/last-modified-at"] = time.Now().UTC().Format(time.RFC3339)
	wf.Annotations["harmostes.dev/graph-native"] = "true"

	if err := s.k8sClient.Patch(r.Context(), &wf, client.MergeFrom(base)); err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "patch workflow graph: %v", err)
		return
	}

	s.auditLog("workflow.graph_update", name, ownerName, "nodes", len(req.Graph.Nodes))
	s.writeJSON(w, http.StatusOK, workflowGraphResponse{
		Workflow:    name,
		Disabled:    wf.Spec.Disabled,
		GraphNative: true,
		Source:      wf.Spec.Source,
		Trigger:     wf.Spec.Source.Kind,
		Graph:       req.Graph,
	})
}

// workflowGraphConvertRequest is the JSON body for POST /api/workflows/{name}/convert.
type workflowGraphConvertRequest struct{}

// handleWorkflowGraphConvert converts a declarative Workflow to graph-native
// mode by compiling its spec (prepare → agent → deploy) into spec.graph. After
// conversion, the workflow runs through the graph executor and the canvas
// becomes editable.
//
// Route: POST /api/workflows/{name}/convert
func (s *Server) handleWorkflowGraphConvert(w http.ResponseWriter, r *http.Request) {
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

	if ownerName != "" && wf.Labels[v1alpha1.OwnerLabel] != ownerName {
		s.writeAPIError(w, http.StatusNotFound, "workflow %q not found", name)
		return
	}

	if wf.Spec.Graph != nil {
		s.writeAPIError(w, http.StatusConflict, "workflow is already graph-native")
		return
	}

	gs := graph.CompileWorkflow(&wf)

	base := wf.DeepCopy()
	wf.Spec.Graph = &gs
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations["harmostes.dev/last-modified-by"] = ownerName
	wf.Annotations["harmostes.dev/last-modified-at"] = metav1.Time{Time: time.Now().UTC()}.Format(time.RFC3339)
	wf.Annotations["harmostes.dev/graph-native"] = "true"

	if err := s.k8sClient.Patch(r.Context(), &wf, client.MergeFrom(base)); err != nil {
		s.writeAPIError(w, http.StatusInternalServerError, "convert workflow: %v", err)
		return
	}

	s.auditLog("workflow.graph_convert", name, ownerName, "nodes", len(gs.Nodes))
	s.writeJSON(w, http.StatusOK, workflowGraphResponse{
		Workflow:    name,
		Disabled:    wf.Spec.Disabled,
		GraphNative: true,
		Source:      wf.Spec.Source,
		Trigger:     wf.Spec.Source.Kind,
		Graph:       gs,
	})
}
