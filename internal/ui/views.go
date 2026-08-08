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

// handleFlowsView is implemented in flows.go
// handleMetricsView is implemented in metrics.go
// handleSessionsView is implemented in sessions.go

// handleLiveView is implemented in live.go
