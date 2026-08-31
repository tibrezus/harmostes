package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

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
