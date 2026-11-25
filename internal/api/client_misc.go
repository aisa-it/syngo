package api

import "net/http"

// Element Web при старте обязательно запрашивает push-правила и ряд служебных
// эндпоинтов; 404 на них валит синхронизацию.
// Здесь — минимальные корректные ответы.

// handleThirdPartyProtocols — GET /_matrix/client/v3/thirdparty/protocols.
func (s *Server) handleThirdPartyProtocols(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{})
}
