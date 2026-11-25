package api

import (
	"net/http"
	"strconv"
)

// userDirectoryResponse — ответ GET /user_directory/search.
type userDirectoryResponse struct {
	Results []userDirectoryResult `json:"results"`
	Limited bool                  `json:"limited"`
}

type userDirectoryResult struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// handleUserDirectorySearch — GET /user_directory/search?search_term=…:
// поиск локальных пользователей по localpart или display_name. Маршрута не было
// вовсе, Element не мог найти собеседника по имени.
func (s *Server) handleUserDirectorySearch(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	if s.auth == nil {
		writeMatrixError(w, http.StatusServiceUnavailable, ErrCodeUnknown, "Auth storage is not configured")

		return
	}

	// Спека: POST с телом {"search_term": …}; query-форму тоже принимаем —
	// ею пользуются простые клиенты и наши тесты.
	var body struct {
		SearchTerm string `json:"search_term"`
		Limit      int    `json:"limit"`
	}

	if r.Body != nil && r.ContentLength != 0 {
		if !decodeRoomBody(w, r, &body) {
			return
		}
	}

	term := body.SearchTerm
	if term == "" {
		term = r.URL.Query().Get("search_term")
	}

	if term == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "search_term is required")

		return
	}

	limit := body.Limit
	if limit == 0 {
		limit = 10
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid limit")

			return
		}

		limit = min(parsed, 50)
	}

	users, err := s.auth.SearchUsers(r.Context(), term, limit)
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	results := make([]userDirectoryResult, 0, len(users))

	for i := range users {
		// Себя в выдачу не включаем: Element и так знает свой профиль.
		if users[i].UserID == session.UserID {
			continue
		}

		results = append(results, userDirectoryResult{
			UserID:      users[i].UserID,
			DisplayName: users[i].DisplayName,
			AvatarURL:   users[i].AvatarURL,
		})
	}

	writeJSON(w, http.StatusOK, userDirectoryResponse{Results: results, Limited: len(results) >= limit})
}
