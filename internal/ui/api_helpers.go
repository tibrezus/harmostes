package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// auditLog emits a structured audit event. These events flow to stdout (JSON)
// → OTel collector → SigNoz, where they can be queried to answer "who changed
// resource X, and when?"
func (s *Server) auditLog(action, resource, user string, extra ...any) {
	attrs := []any{"audit", "true", "action", action, "resource", resource, "user", user}
	attrs = append(attrs, extra...)
	s.logger.Info("audit: "+action, attrs...)
}

// writeJSON encodes data as JSON and writes it with the given status code.
func (s *Server) writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Error("write json response", "err", err)
	}
}

// writeAPIError writes a structured JSON error response.
func (s *Server) writeAPIError(w http.ResponseWriter, status int, format string, args ...any) {
	s.writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}
