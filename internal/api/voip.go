package api

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // TURN static-auth-secret по RFC 5766 использует HMAC-SHA1
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
)

// turnCredentials считает TURN-креденшелы (coturn static-auth-secret).
func turnCredentials(uris []string, secret string, ttl time.Duration, userID string) map[string]any {
	username := fmt.Sprintf("%d:%s", time.Now().Add(ttl).Unix(), userID)

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))

	return map[string]any{
		"uris":     uris,
		"username": username,
		"password": base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		"ttl":      int(ttl.Seconds()),
	}
}

// handleTurnServer — GET /_matrix/client/v3/voip/turnServer.
//
// Креденшелы считаются по схеме coturn: username = "<expiry>:<userID>",
// password = base64(HMAC-SHA1(secret, username)). Без настроенных URIs — 403,
// как Synapse при выключенном TURN.
func (s *Server) handleTurnServer(w http.ResponseWriter, r *http.Request) {
	// Element запрашивает turnServer на старте до логина и ждёт 200: при выключенном
	// TURN отдаём пустой объект (как прежняя заглушка), при включённом — креды.
	session, ok := sessionFrom(r.Context())
	if !ok || len(s.cfg.VoIP.URIs) == 0 || s.cfg.VoIP.Secret == "" {
		writeJSON(w, http.StatusOK, map[string]any{})

		return
	}

	ttl := s.cfg.VoIP.TTL.Std()
	if ttl <= 0 {
		ttl = time.Hour
	}

	writeJSON(w, http.StatusOK, turnCredentials(s.cfg.VoIP.URIs, s.cfg.VoIP.Secret, ttl, session.UserID))
}
