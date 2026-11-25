package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// openIDToken — выданный OpenID-токен (живёт TTL, хранится в памяти процесса).
type openIDToken struct {
	userID    string
	expiresAt time.Time
}

// openIDStore — in-memory хранилище OpenID-токенов.
type openIDStore struct {
	mu     sync.Mutex
	tokens map[string]openIDToken
	now    func() time.Time
}

func newOpenIDStore() *openIDStore {
	return &openIDStore{tokens: map[string]openIDToken{}, now: time.Now}
}

const openIDTTL = 10 * time.Minute

// issue выдаёт токен для пользователя.
func (s *openIDStore) issue(userID string) string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)

	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	s.tokens[token] = openIDToken{userID: userID, expiresAt: s.now().Add(openIDTTL)}
	s.mu.Unlock()

	return token
}

// lookup возвращает user_id по токену (и удаляет протухшие).
func (s *openIDStore) lookup(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tokens[token]
	if !ok {
		return "", false
	}

	if s.now().After(entry.expiresAt) {
		delete(s.tokens, token)

		return "", false
	}

	delete(s.tokens, token) // одноразовый, как в Synapse

	return entry.userID, true
}

// handleOpenIDRequestToken — POST /user/{userId}/openid/request_token.
func (s *Server) handleOpenIDRequestToken(w http.ResponseWriter, r *http.Request) {
	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	userID := normalizeUserID(pathParamValue(r, "userId"))
	if userID != session.UserID {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Cannot request token for another user")

		return
	}

	token := s.openid.issue(userID)

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":       token,
		"token_type":         "Bearer",
		"matrix_server_name": s.cfg.Matrix.ServerName,
		"expires_in":         int(openIDTTL.Seconds()),
	})
}

// handleOpenIDUserInfo — GET /_matrix/federation/v1/openid/userinfo?access_token=.
func (s *Server) handleOpenIDUserInfo(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("access_token")
	if token == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingToken, "access_token is required")

		return
	}

	userID, ok := s.openid.lookup(token)
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeUnknownToken, "Unknown or expired token")

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"sub": userID})
}
