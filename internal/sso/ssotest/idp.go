// Package ssotest - подставной провайдер OpenID Connect для тестов:
// discovery, authorize, token, JWKS и userinfo на httptest. Провайдер честно
// проверяет client_id/secret, redirect_uri, одноразовость code и PKCE S256, а поля
// для атак (чужой ключ подписи, чужой nonce, истекший токен) подменяют id_token.
package ssotest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// KeyID - kid ключа провайдера в JWKS.
const KeyID = "test-key"

// grant - выданный authorize code.
type grant struct {
	nonce       string
	challenge   string
	redirectURI string
	claims      map[string]any
}

// Provider - подставной провайдер.
type Provider struct {
	Server       *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string
	// Key - ключ подписи из JWKS.
	Key *rsa.PrivateKey

	mu sync.Mutex
	// Claims - пользователь, который «войдет» на следующем authorize (sub обязателен).
	Claims map[string]any
	// SignWith - подписывать id_token другим ключом (атака: подпись чужим ключом).
	SignWith *rsa.PrivateKey
	// Nonce - подменить nonce в id_token (атака: чужой nonce).
	Nonce *string
	// TTL - срок id_token; отрицательный - уже истек.
	TTL time.Duration
	// Audience, TokenIssuer - подменить aud и iss в id_token.
	Audience    string
	TokenIssuer string
	// AdvertisePKCE - объявлять S256 в discovery.
	AdvertisePKCE bool
	// RequirePKCE - authorize без code_challenge отклоняется.
	RequirePKCE bool
	// UserInfo - claims userinfo; nil - те же, что в id_token.
	UserInfo map[string]any

	codes map[string]grant
	// Authorizations - запросы authorize (для проверки scope, nonce, PKCE).
	Authorizations []url.Values
	// TokenRequests - число запросов token.
	TokenRequests int
}

// New поднимает провайдера; закрывается в t.Cleanup.
func New(t testing.TB, clientID, clientSecret string) *Provider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ssotest: key: %v", err)
	}

	idp := &Provider{
		ClientID: clientID, ClientSecret: clientSecret, Key: key,
		TTL: time.Hour, AdvertisePKCE: true, codes: map[string]grant{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /authorize", idp.authorize)
	mux.HandleFunc("POST /token", idp.token)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("GET /userinfo", idp.userinfo)

	idp.Server = httptest.NewServer(mux)
	idp.Issuer = idp.Server.URL
	t.Cleanup(idp.Server.Close)

	return idp
}

// Login задает пользователя для следующих входов.
func (i *Provider) Login(claims map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.Claims = claims
}

// Configure меняет настройки под мьютексом (поля читаются обработчиками).
func (i *Provider) Configure(change func(*Provider)) {
	i.mu.Lock()
	defer i.mu.Unlock()

	change(i)
}

func (i *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	doc := map[string]any{
		"issuer":                                i.Issuer,
		"authorization_endpoint":                i.Issuer + "/authorize",
		"token_endpoint":                        i.Issuer + "/token",
		"jwks_uri":                              i.Issuer + "/jwks",
		"userinfo_endpoint":                     i.Issuer + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}

	if i.AdvertisePKCE {
		doc["code_challenge_methods_supported"] = []string{"S256"}
	}

	writeJSON(w, http.StatusOK, doc)
}

// authorize сразу «входит» пользователем Claims и возвращает code на redirect_uri.
func (i *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	q := r.URL.Query()
	i.Authorizations = append(i.Authorizations, q)

	if q.Get("client_id") != i.ClientID || q.Get("response_type") != "code" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)

		return
	}

	if q.Get("code_challenge") != "" && q.Get("code_challenge_method") != "S256" ||
		i.RequirePKCE && q.Get("code_challenge") == "" {
		http.Error(w, "bad PKCE", http.StatusBadRequest)

		return
	}

	code := random()
	i.codes[code] = grant{
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), claims: i.Claims,
	}

	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)

		return
	}

	back := target.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	target.RawQuery = back.Encode()

	http.Redirect(w, r, target.String(), http.StatusFound) //nolint:gosec // тестовый IdP: redirect_uri задает тест
}

// token обменивает code (одноразовый) на токены с проверкой клиента и PKCE.
func (i *Provider) token(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.TokenRequests++

	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})

		return
	}

	clientID, secret, ok := r.BasicAuth()
	if !ok {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}

	if clientID != i.ClientID || subtle.ConstantTimeCompare([]byte(secret), []byte(i.ClientSecret)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client"})

		return
	}

	code := r.PostForm.Get("code")

	g, found := i.codes[code]
	delete(i.codes, code)

	if !found || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirectURI {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})

		return
	}

	if g.challenge != "" {
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "PKCE"})

			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "at-" + random(), "token_type": "Bearer", "expires_in": 300,
		"id_token": i.idToken(g),
	})
}

// idToken подписывает id_token с учетом подмен для атак.
func (i *Provider) idToken(g grant) string {
	now := time.Now()
	claims := map[string]any{}

	for k, v := range g.claims {
		claims[k] = v
	}

	claims["iss"] = i.Issuer
	if i.TokenIssuer != "" {
		claims["iss"] = i.TokenIssuer
	}

	claims["aud"] = i.ClientID
	if i.Audience != "" {
		claims["aud"] = i.Audience
	}

	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(i.TTL).Unix()

	claims["nonce"] = g.nonce
	if i.Nonce != nil {
		claims["nonce"] = *i.Nonce
	}

	key := i.Key
	if i.SignWith != nil {
		key = i.SignWith
	}

	return SignRS256(key, KeyID, claims)
}

func (i *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := i.Key.PublicKey

	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": KeyID, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (i *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	claims := i.UserInfo
	if claims == nil {
		claims = i.Claims
	}

	writeJSON(w, http.StatusOK, claims)
}

// SignRS256 подписывает claims как JWT (RS256).
func SignRS256(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	payload, _ := json.Marshal(claims)

	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(input))

	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic("ssotest: sign: " + err.Error())
	}

	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func random() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)

	return base64.RawURLEncoding.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
