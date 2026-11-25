package api

import (
	"net/http"
)

// handleMetrics отдаёт метрики в текстовом формате Prometheus.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	s.writeMetrics(r.Context(), w)
}
