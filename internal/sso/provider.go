// Package sso - вход через OpenID Connect: провайдер по конфигу
// oidc_providers (discovery, client_id/secret, PKCE, проверка id_token по JWKS, iss,
// aud, exp, nonce), маппинг claims в localpart и displayname, сессия браузера в
// зашифрованной cookie и проверка адреса возврата клиента.
//
// Протокольная часть - github.com/coreos/go-oidc/v3 и golang.org/x/oauth2; учетные
// записи и loginToken - core.SSO.
package sso

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/sso/mapping"
)

// Ошибки входа через провайдера. Транспорт показывает их страницей ошибки.
var (
	// ErrProviderUnavailable - провайдер недоступен или ответил не по протоколу.
	ErrProviderUnavailable = errors.New("sso: provider unavailable")
	// ErrInvalidIDToken - id_token не прошел проверку (подпись, iss, aud, exp, nonce).
	ErrInvalidIDToken = errors.New("sso: invalid id_token")
	// ErrNotAllowed - claims не удовлетворяют attribute_requirements.
	ErrNotAllowed = errors.New("sso: user is not allowed by attribute_requirements")
	// ErrMapping - из claims не получается subject или localpart.
	ErrMapping = errors.New("sso: user mapping failed")
)

// CallbackPath - адрес callback OIDC, как в Synapse: IdP при переезде не
// перенастраивается.
const CallbackPath = "/_synapse/client/oidc/callback"

// defaultTimeout - запросы к провайдеру по умолчанию.
const defaultTimeout = 10 * time.Second

// Options - общие параметры провайдеров.
type Options struct {
	// RedirectURI - полный адрес callback (<public_baseurl>_synapse/client/oidc/callback).
	RedirectURI string
	// HTTPClient - клиент к провайдеру; nil - новый с таймаутом из конфига.
	HTTPClient *http.Client
	// Now - часы для проверки exp (тесты); nil - time.Now.
	Now func() time.Time
}

// Provider - один провайдер OIDC.
type Provider struct {
	// ID - auth_provider (idp_id с префиксом oidc-), Name/Icon/Brand - для клиента.
	ID    string
	Name  string
	Icon  string
	Brand string

	cfg         config.OIDCProvider
	secret      string
	client      *http.Client
	redirectURI string
	localpart   *mapping.Template
	displayName *mapping.Template
	now         func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	pkce     bool
}

// NewProvider собирает провайдера: секрет читается сразу (ошибка - при старте),
// discovery - при первом входе (недоступный IdP не мешает запуску сервера).
func NewProvider(cfg config.OIDCProvider, opts Options) (*Provider, error) {
	secret, err := cfg.ResolveClientSecret()
	if err != nil {
		return nil, err
	}

	localpart, err := mapping.Parse(cfg.UserMappingProvider.Config.LocalpartTemplate)
	if err != nil {
		return nil, err
	}

	displayName, err := mapping.Parse(cfg.UserMappingProvider.Config.DisplayNameTemplate)
	if err != nil {
		return nil, err
	}

	client := opts.HTTPClient
	if client == nil {
		timeout := cfg.Timeout.Std()
		if timeout <= 0 {
			timeout = defaultTimeout
		}

		client = &http.Client{Timeout: timeout}
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Provider{
		ID: cfg.AuthProviderID(), Name: cfg.DisplayName(), Icon: cfg.IDPIcon, Brand: cfg.IDPBrand,
		cfg: cfg, secret: secret, client: client, redirectURI: opts.RedirectURI,
		localpart: localpart, displayName: displayName, now: now,
	}, nil
}

// clientContext - контекст с HTTP-клиентом провайдера для go-oidc и oauth2.
func (p *Provider) clientContext(ctx context.Context) context.Context {
	return context.WithValue(oidc.ClientContext(ctx, p.client), oauth2.HTTPClient, p.client)
}

// metadata - метаданные провайдера (discovery или адреса из конфига). Успешный
// результат кешируется, ошибка - нет: следующий вход попробует снова.
func (p *Provider) metadata(ctx context.Context) (*oidc.Provider, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.provider != nil {
		return p.provider, p.pkce, nil
	}

	ctx = p.clientContext(ctx)

	var (
		provider *oidc.Provider
		err      error
	)

	if p.cfg.Discovers() {
		provider, err = oidc.NewProvider(ctx, p.cfg.Issuer)
		if err != nil {
			return nil, false, fmt.Errorf("%w: discovery %s: %w", ErrProviderUnavailable, p.cfg.Issuer, err)
		}
	} else {
		algs := p.cfg.IDTokenSigningAlgs
		if len(algs) == 0 {
			algs = []string{oidc.RS256}
		}

		provider = (&oidc.ProviderConfig{
			IssuerURL: p.cfg.Issuer, AuthURL: p.cfg.AuthorizationEndpoint, TokenURL: p.cfg.TokenEndpoint,
			UserInfoURL: p.cfg.UserinfoEndpoint, JWKSURL: p.cfg.JWKSURI, Algorithms: algs,
		}).NewProvider(ctx)
	}

	var claims struct {
		CodeChallengeMethods []string `json:"code_challenge_methods_supported"`
	}

	if p.cfg.Discovers() {
		if err := provider.Claims(&claims); err != nil {
			return nil, false, fmt.Errorf("%w: discovery %s: %w", ErrProviderUnavailable, p.cfg.Issuer, err)
		}
	}

	switch p.cfg.PKCEMethod {
	case "always":
		p.pkce = true
	case "never":
		p.pkce = false
	default: // auto - если провайдер объявил S256
		p.pkce = slices.Contains(claims.CodeChallengeMethods, "S256")
	}

	p.provider = provider

	return provider, p.pkce, nil
}

// oauth2Config - параметры клиента для authorize и token.
func (p *Provider) oauth2Config(provider *oidc.Provider) *oauth2.Config {
	scopes := []string{oidc.ScopeOpenID}

	for _, scope := range p.cfg.Scopes {
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}

	endpoint := provider.Endpoint()

	switch p.cfg.ClientAuthMethod {
	case "client_secret_post", "none":
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	default:
		endpoint.AuthStyle = oauth2.AuthStyleInHeader
	}

	return &oauth2.Config{
		ClientID: p.cfg.ClientID, ClientSecret: p.secret, Endpoint: endpoint,
		RedirectURL: p.redirectURI, Scopes: scopes,
	}
}

// AuthorizationURL - адрес authorize провайдера для сессии. PKCE используется, если
// включен для провайдера; иначе session.Verifier очищается и не уходит в token.
func (p *Provider) AuthorizationURL(ctx context.Context, session *Session) (string, error) {
	provider, pkce, err := p.metadata(ctx)
	if err != nil {
		return "", err
	}

	opts := []oauth2.AuthCodeOption{oidc.Nonce(session.Nonce)}

	if pkce {
		opts = append(opts, oauth2.S256ChallengeOption(session.Verifier))
	} else {
		session.Verifier = ""
	}

	return p.oauth2Config(provider).AuthCodeURL(session.State, opts...), nil
}

// Identity - пользователь, подтвержденный провайдером.
type Identity struct {
	// Subject - external_id (claim subject_claim).
	Subject string
	// Claims - claims id_token (или userinfo при user_profile_method: userinfo_endpoint).
	Claims map[string]any
}

// Exchange обменивает code на токены, проверяет id_token (подпись по JWKS, iss, aud,
// exp - go-oidc; nonce - здесь) и возвращает пользователя.
func (p *Provider) Exchange(ctx context.Context, code string, session Session) (Identity, error) {
	provider, _, err := p.metadata(ctx)
	if err != nil {
		return Identity{}, err
	}

	ctx = p.clientContext(ctx)

	var opts []oauth2.AuthCodeOption
	if session.Verifier != "" {
		opts = append(opts, oauth2.VerifierOption(session.Verifier))
	}

	token, err := p.oauth2Config(provider).Exchange(ctx, code, opts...)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: token: %w", ErrProviderUnavailable, err)
	}

	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return Identity{}, fmt.Errorf("%w: в ответе token нет id_token", ErrInvalidIDToken)
	}

	idToken, err := provider.Verifier(&oidc.Config{ClientID: p.cfg.ClientID, Now: p.now}).Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidIDToken, err)
	}

	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(session.Nonce)) != 1 || session.Nonce == "" {
		return Identity{}, fmt.Errorf("%w: nonce не совпадает с сессией", ErrInvalidIDToken)
	}

	var raw json.RawMessage
	if err := idToken.Claims(&raw); err != nil {
		return Identity{}, fmt.Errorf("%w: claims: %w", ErrInvalidIDToken, err)
	}

	claims, err := decodeClaims(raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: claims: %w", ErrInvalidIDToken, err)
	}

	if p.cfg.UserProfileMethod == "userinfo_endpoint" {
		claims, err = p.userInfo(ctx, provider, token, idToken.Subject)
		if err != nil {
			return Identity{}, err
		}
	}

	return p.identity(claims)
}

// userInfo - claims из userinfo (как user_profile_method: userinfo_endpoint в
// Synapse); sub обязан совпасть с id_token (OIDC Core 5.3.2).
func (p *Provider) userInfo(ctx context.Context, provider *oidc.Provider, token *oauth2.Token, subject string) (map[string]any, error) {
	info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %w", ErrProviderUnavailable, err)
	}

	if info.Subject != subject {
		return nil, fmt.Errorf("%w: sub userinfo не совпадает с id_token", ErrInvalidIDToken)
	}

	var raw json.RawMessage
	if err := info.Claims(&raw); err != nil {
		return nil, fmt.Errorf("%w: userinfo: %w", ErrProviderUnavailable, err)
	}

	claims, err := decodeClaims(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: userinfo: %w", ErrProviderUnavailable, err)
	}

	return claims, nil
}

// decodeClaims разбирает claims с json.Number: числовой sub не теряет точность.
func decodeClaims(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return nil, err
	}

	return claims, nil
}

// identity - subject и проверка attribute_requirements.
func (p *Provider) identity(claims map[string]any) (Identity, error) {
	for _, req := range p.cfg.AttributeRequirements {
		if !claimMatches(claims[req.Attribute], req.Value) {
			return Identity{}, fmt.Errorf("%w: %s", ErrNotAllowed, req.Attribute)
		}
	}

	subject := mapping.Claim(claims, p.cfg.SubjectClaim())
	if subject == "" {
		return Identity{}, fmt.Errorf("%w: нет claim %s", ErrMapping, p.cfg.SubjectClaim())
	}

	return Identity{Subject: subject, Claims: claims}, nil
}

// claimMatches - claim равен value или (список) содержит его.
func claimMatches(claim any, value string) bool {
	if list, ok := claim.([]any); ok {
		for _, item := range list {
			if mapping.ClaimString(item) == value {
				return true
			}
		}

		return false
	}

	return claim != nil && mapping.ClaimString(claim) == value
}

// Localpart - кандидаты localpart как JinjaOidcMappingProvider Synapse: шаблон,
// strip, map_username_to_mxid_localpart, при занятом имени - числовой суффикс.
func (p *Provider) Localpart(claims map[string]any) func(attempt int) (string, error) {
	base := mapping.MXIDLocalpart(strings.TrimSpace(p.localpart.Render(claims)))

	return func(attempt int) (string, error) {
		if base == "" {
			return "", fmt.Errorf("%w: localpart_template дал пустую строку", ErrMapping)
		}

		if attempt == 0 {
			return base, nil
		}

		return base + strconv.Itoa(attempt), nil
	}
}

// DisplayName - displayname по display_name_template; пусто - не задан.
func (p *Provider) DisplayName(claims map[string]any) string {
	return strings.TrimSpace(p.displayName.Render(claims))
}

// AllowExistingUsers - allow_existing_users провайдера.
func (p *Provider) AllowExistingUsers() bool {
	return p.cfg.AllowExistingUsers
}
