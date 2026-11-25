package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // HMAC-SHA1 задан протоколом Synapse admin register, не хеш паролей
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// Регистрация через общий секрет: совместима с Synapse
// /_synapse/admin/v1/register и утилитой register_new_matrix_user. Нужна, чтобы
// при закрытой регистрации (по умолчанию) оператор мог создать пользователей и
// первого администратора без SQL.
//
//	GET  /_synapse/admin/v1/register -> {"nonce": "..."}
//	POST /_synapse/admin/v1/register {nonce, username, password, admin, displayname, mac}
//
// mac = hex(HMAC-SHA1(secret, nonce \x00 username \x00 password \x00 admin|notadmin)).

// sharedSecretNonceTTL - срок жизни nonce (как в Synapse: 60 секунд).
const sharedSecretNonceTTL = time.Minute

// nonceStore - одноразовые nonce в памяти процесса.
type nonceStore struct {
	mu     sync.Mutex
	nonces map[string]time.Time
	now    func() time.Time
}

func newNonceStore() *nonceStore {
	return &nonceStore{nonces: map[string]time.Time{}, now: time.Now}
}

// issue выдает новый nonce и чистит просроченные.
func (n *nonceStore) issue() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	nonce := hex.EncodeToString(raw)
	now := n.now()

	n.mu.Lock()
	defer n.mu.Unlock()

	for k, exp := range n.nonces {
		if now.After(exp) {
			delete(n.nonces, k)
		}
	}

	n.nonces[nonce] = now.Add(sharedSecretNonceTTL)

	return nonce, nil
}

// consume гасит nonce: повторное использование и просроченный nonce - false.
func (n *nonceStore) consume(nonce string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	exp, ok := n.nonces[nonce]
	if !ok {
		return false
	}

	delete(n.nonces, nonce)

	return !n.now().After(exp)
}

// sharedSecretRegisterRequest - тело POST /_synapse/admin/v1/register.
type sharedSecretRegisterRequest struct {
	Nonce       string `json:"nonce"`
	Username    string `json:"username"`
	DisplayName string `json:"displayname"`
	Password    string `json:"password"`
	Admin       bool   `json:"admin"`
	MAC         string `json:"mac"`
}

// sharedSecretMAC считает подпись запроса по схеме Synapse.
func sharedSecretMAC(secret, nonce, username, password string, admin bool) []byte {
	role := "notadmin"
	if admin {
		role = "admin"
	}

	mac := hmac.New(sha1.New, []byte(secret))
	for i, part := range []string{nonce, username, password, role} {
		if i > 0 {
			mac.Write([]byte{0})
		}

		mac.Write([]byte(part))
	}

	return mac.Sum(nil)
}

// sharedSecretEnabled отвечает 400, если общий секрет не настроен (как Synapse).
func (s *Server) sharedSecretEnabled(w http.ResponseWriter) bool {
	if s.cfg.Auth.RegistrationSharedSecret == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Shared secret registration is not enabled")

		return false
	}

	return true
}

// handleSharedSecretNonce - GET /_synapse/admin/v1/register.
func (s *Server) handleSharedSecretNonce(w http.ResponseWriter, _ *http.Request) {
	if !s.sharedSecretEnabled(w) {
		return
	}

	nonce, err := s.registerNonces.issue()
	if err != nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Could not generate nonce")

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"nonce": nonce})
}

// handleSharedSecretRegister - POST /_synapse/admin/v1/register.
func (s *Server) handleSharedSecretRegister(w http.ResponseWriter, r *http.Request) {
	if !s.sharedSecretEnabled(w) || !s.authReady(w) {
		return
	}

	var req sharedSecretRegisterRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Nonce == "" || req.Username == "" || req.Password == "" || req.MAC == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "nonce, username, password and mac are required")

		return
	}

	// Nonce гасим до проверки подписи: перебор mac по одному nonce невозможен.
	if !s.registerNonces.consume(req.Nonce) {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "unrecognised nonce")

		return
	}

	got, err := hex.DecodeString(req.MAC)
	want := sharedSecretMAC(s.cfg.Auth.RegistrationSharedSecret, req.Nonce, req.Username, req.Password, req.Admin)

	if err != nil || !hmac.Equal(got, want) {
		writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "HMAC incorrect")

		return
	}

	result, err := s.auth.Register(r.Context(), core.RegisterRequest{
		Localpart:   req.Username,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Operator:    true,
		Admin:       req.Admin,
	})
	if err != nil {
		s.writeAuthError(w, err)

		return
	}

	s.log.Info("auth: user registered via shared secret", "user_id", result.UserID, "admin", req.Admin)

	// Регистрация оператором - admin-действие: пишем в аудит без пароля и mac.
	// Пользователь уже создан, поэтому сбой записи - в лог.
	if err := s.recordAdminAudit(r.Context(), "shared_secret", "register", result.UserID,
		map[string]any{"admin": req.Admin}); err != nil {
		s.log.ErrorContext(r.Context(), "api: shared secret register audit failed", "user_id", result.UserID, "error", err)
	}

	writeJSON(w, http.StatusOK, registerResponse{
		UserID:      result.UserID,
		AccessToken: result.AccessToken,
		DeviceID:    result.DeviceID,
		HomeServer:  s.cfg.Matrix.ServerName,
		ExpiresInMS: result.ExpiresInMS,
	})
}
