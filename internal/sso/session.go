package sso

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// ErrSession - cookie сессии отсутствует, подделана или истекла.
var ErrSession = errors.New("sso: invalid or expired session")

// CookieName - cookie с сессией входа (state, nonce, PKCE verifier, адрес возврата).
const CookieName = "syngo_oidc_session"

// CookiePath - cookie уходит только на callback.
const CookiePath = "/_synapse/client/oidc"

// SessionTTL - сколько ждем возврата от провайдера.
const SessionTTL = 15 * time.Minute

// MaxRedirectURLLength - предел длины redirectUrl (cookie не больше 4 КБ).
const MaxRedirectURLLength = 2048

// Session - состояние одного входа, привязанное к браузеру cookie. Хранится у
// клиента зашифрованным (AES-256-GCM): подделать state или подменить адрес
// возврата нельзя, серверной памяти под незавершенные входы не нужно.
type Session struct {
	IDP      string `json:"i"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v,omitempty"`
	// RedirectURL - адрес клиента для loginToken (вход); пусто - UIA.
	RedirectURL string `json:"r,omitempty"`
	// UIASession - сессия UIA, которую подтверждает этот вход (m.login.sso).
	UIASession string `json:"u,omitempty"`
	ExpiresAt  int64  `json:"e"`
}

// SessionCodec шифрует сессию в значение cookie и обратно.
type SessionCodec struct {
	aead cipher.AEAD
	now  func() time.Time
}

// NewSessionCodec - ключ 32 байта; now nil - time.Now.
func NewSessionCodec(key []byte, now func() time.Time) (*SessionCodec, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sso: session key: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sso: session key: %w", err)
	}

	if now == nil {
		now = time.Now
	}

	return &SessionCodec{aead: aead, now: now}, nil
}

// NewSession создает сессию входа со случайными state, nonce и PKCE verifier.
func (c *SessionCodec) NewSession(idp, redirectURL, uiaSession string) (Session, error) {
	state, err := randomString(24)
	if err != nil {
		return Session{}, err
	}

	nonce, err := randomString(24)
	if err != nil {
		return Session{}, err
	}

	return Session{
		IDP: idp, State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(),
		RedirectURL: redirectURL, UIASession: uiaSession,
		ExpiresAt: c.now().Add(SessionTTL).Unix(),
	}, nil
}

// Encode шифрует сессию в значение cookie.
func (c *SessionCodec) Encode(session Session) (string, error) {
	plain, err := json.Marshal(session)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	sealed := c.aead.Seal(nonce, nonce, plain, []byte(CookieName))

	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Decode расшифровывает и проверяет срок.
func (c *SessionCodec) Decode(value string) (Session, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(sealed) < c.aead.NonceSize() {
		return Session{}, ErrSession
	}

	nonceSize := c.aead.NonceSize()

	plain, err := c.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte(CookieName))
	if err != nil {
		return Session{}, ErrSession
	}

	var session Session
	if err := json.Unmarshal(plain, &session); err != nil {
		return Session{}, ErrSession
	}

	if c.now().Unix() >= session.ExpiresAt {
		return Session{}, ErrSession
	}

	return session, nil
}

// randomString - n случайных байт в base64url.
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("sso: random: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// forbiddenSchemes - схемы, которые в ссылке страницы подтверждения исполняют код.
var forbiddenSchemes = map[string]bool{"javascript": true, "data": true, "vbscript": true, "file": true}

// CheckRedirectURL проверяет redirectUrl клиента: абсолютный адрес без опасной
// схемы (Element Desktop и мобильные клиенты используют свои схемы, их не режем).
func CheckRedirectURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > MaxRedirectURLLength {
		return nil, errors.New("redirectUrl пуст или слишком длинный")
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return nil, errors.New("redirectUrl должен быть абсолютным адресом")
	}

	if forbiddenSchemes[strings.ToLower(parsed.Scheme)] {
		return nil, fmt.Errorf("redirectUrl: схема %q запрещена", parsed.Scheme)
	}

	return parsed, nil
}

// Whitelisted - адрес начинается с одного из sso.client_whitelist (как в Synapse),
// причем префикс без завершающего "/" должен заканчиваться на границе пути:
// "https://app.example.org" не пропускает "https://app.example.org.evil.com".
func Whitelisted(target string, whitelist []string) bool {
	for _, prefix := range whitelist {
		if prefix == "" || !strings.HasPrefix(target, prefix) {
			continue
		}

		if strings.HasSuffix(prefix, "/") || len(target) == len(prefix) ||
			strings.ContainsRune("/?#", rune(target[len(prefix)])) {
			return true
		}
	}

	return false
}

// AddLoginToken дописывает loginToken в query адреса клиента (фрагмент сохраняется).
func AddLoginToken(target *url.URL, token string) string {
	out := *target
	query := out.Query()
	query.Set("loginToken", token)
	out.RawQuery = query.Encode()

	return out.String()
}
