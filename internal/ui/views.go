package ui

import (
	"net/http"
)

// handleMapView is the primary view: an interactive Cytoscape.js topology
// graph showing the selected workflow's compiled structure with live run
// status. Full implementation in Phase 3 (#137).
func (s *Server) handleMapView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	s.render(w, r, "pages/map.html", map[string]any{
		"WorkflowName": workflowName,
	})
}

// handleFlowsView is the real-time event table — workflow events streamed via
// SSE as they happen. Full implementation in Phase 4 (#138).
func (s *Server) handleFlowsView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	s.render(w, r, "pages/flows.html", map[string]any{
		"WorkflowName": workflowName,
	})
}

// handleMetricsView shows per-workflow agent metrics (token histograms,
// gate pass rates, durations) sourced from the SigNoz API.
// Full implementation in Phase 5 (#139).
func (s *Server) handleMetricsView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	s.render(w, r, "pages/metrics.html", map[string]any{
		"WorkflowName": workflowName,
	})
}

// handleSessionsView lists all agent session transcripts (from Dapr state),
// accessible per-workflow. Full implementation in Phase 7 (#141).
func (s *Server) handleSessionsView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	s.render(w, r, "pages/sessions.html", map[string]any{
		"WorkflowName": workflowName,
	})
}

// handleLiveView is the embedded retro terminal streaming the agent working in
// real time. Full implementation in Phase 6 (#140).
func (s *Server) handleLiveView(w http.ResponseWriter, r *http.Request) {
	workflowName := r.URL.Query().Get("workflow")
	s.render(w, r, "pages/live.html", map[string]any{
		"WorkflowName": workflowName,
	})
}
