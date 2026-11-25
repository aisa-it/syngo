package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// UIA по спецификации (User-Interactive Authentication) для действий с паролем:
// смена пароля, деактивация, удаление устройств, замена ключей кросс-подписи.
// Раньше ответы расходились со спецификацией (stages строкой, пустая session,
// неверный пароль - 403 без flows), и клиент не мог повторить шаг.

const (
	// uiaMaxFailures неверных паролей за uiaWindow - дальше 429: иначе украденный
	// токен позволял перебирать пароль через повторную аутентификацию.
	uiaMaxFailures = 5
	uiaWindow      = 15 * time.Minute
)

type uiaFailures struct {
	mu    sync.Mutex
	byKey map[string][]time.Time
}

// recent возвращает число неудач за окно (и чистит старые).
func (f *uiaFailures) recent(userID string, now time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	kept := f.byKey[userID][:0]

	for _, at := range f.byKey[userID] {
		if now.Sub(at) < uiaWindow {
			kept = append(kept, at)
		}
	}

	if len(kept) == 0 {
		delete(f.byKey, userID)
	} else {
		f.byKey[userID] = kept
	}

	return len(kept)
}

func (f *uiaFailures) add(userID string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.byKey == nil {
		f.byKey = map[string][]time.Time{}
	}

	f.byKey[userID] = append(f.byKey[userID], now)
}

// writeUIAChallenge отвечает 401 со стадиями пользователя (каждая - отдельный
// flow); errcode - при неверном шаге. session - сохраненная сессия UIA: по ней
// fallback m.login.sso привязывает вход у провайдера к пользователю и действию.
func writeUIAChallenge(w http.ResponseWriter, session string, stages []string, errcode, message string) {
	if session == "" {
		buf := make([]byte, 18)
		_, _ = rand.Read(buf)
		session = base64.RawURLEncoding.EncodeToString(buf)
	}

	flows := make([]map[string][]string, 0, len(stages))
	for _, stage := range stages {
		flows = append(flows, map[string][]string{"stages": {stage}})
	}

	body := map[string]any{
		"flows":   flows,
		"params":  map[string]any{},
		"session": session,
	}

	if errcode != "" {
		body["errcode"] = errcode
		body["error"] = message
	}

	writeJSON(w, http.StatusUnauthorized, body)
}

// passwordUIA проверяет шаг UIA: m.login.password или пройденный через fallback
// m.login.sso (учетки SSO без пароля); false - ответ уже записан.
func (s *Server) passwordUIA(w http.ResponseWriter, r *http.Request, userID string, auth *accountAuth) bool {
	var requested string
	if auth != nil {
		requested = auth.Session
	}

	// Клиент повторяет запрос с session после fallback: type пуст или m.login.sso.
	if auth != nil && (auth.Type == "" || auth.Type == ssoStage) && s.ssoUIACompleted(r, userID, requested) {
		s.uia.Delete(requested)

		return true
	}

	if auth == nil || auth.Type != "m.login.password" || auth.Password == "" {
		s.uiaChallenge(w, r, userID, requested, "", "")

		return false
	}

	// Пароль другого пользователя UIA владельца сессии не проходит: 403, как в
	// Synapse («Requested user does not match», Complement TestDeviceManagement).
	if requestedUser := auth.identifiedUser(); requestedUser != "" && !s.sameLocalUser(requestedUser, userID) {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "Requested user does not match the authenticated user")

		return false
	}

	now := time.Now()

	if s.uiaFails.recent(userID, now) >= uiaMaxFailures {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"errcode": ErrCodeLimitExceeded, "error": "Too many failed attempts",
			"retry_after_ms": uiaWindow.Milliseconds(),
		})

		return false
	}

	if err := s.auth.VerifyPassword(r.Context(), userID, auth.Password); err != nil {
		if errors.Is(err, core.ErrInvalidCredentials) {
			s.uiaFails.add(userID, now)
			s.uiaChallenge(w, r, userID, requested, ErrCodeForbidden, "Invalid password")

			return false
		}

		s.writeAuthError(w, err)

		return false
	}

	return true
}

// uiaChallenge - 401 со стадиями пользователя и сохраненной сессией UIA.
func (s *Server) uiaChallenge(w http.ResponseWriter, r *http.Request, userID, requested, errcode, message string) {
	writeUIAChallenge(w, s.uiaSessionFor(r, userID, requested), s.uiaStages(r.Context(), userID), errcode, message)
}

// sameLocalUser сравнивает идентификатор из UIA (localpart или полный user_id, в
// любом регистре) с user_id сессии.
func (s *Server) sameLocalUser(identifier, userID string) bool {
	if !strings.HasPrefix(identifier, "@") {
		identifier = "@" + identifier + ":" + s.cfg.Matrix.ServerName
	}

	return strings.EqualFold(identifier, userID)
}
