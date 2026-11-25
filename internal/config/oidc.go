package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/aisa-it/syngo/internal/sso/mapping"
)

// EnvOIDCClientSecretPrefix - префикс переменной окружения с client_secret провайдера
// OIDC: SYNGOD_OIDC_CLIENT_SECRET_<IDP_ID>, idp_id в верхнем регистре,
// символы кроме латиницы и цифр заменены на "_". Переменная имеет приоритет над
// client_secret и client_secret_path.
//
//nolint:gosec // это префикс имени переменной окружения, а не значение секрета
const EnvOIDCClientSecretPrefix = "SYNGOD_OIDC_CLIENT_SECRET_"

// SSO - общие настройки входа через SSO (секция sso Synapse).
type SSO struct {
	// ClientWhitelist - адреса клиентов (префиксы), на которые после SSO выдается
	// loginToken без страницы подтверждения (sso.client_whitelist Synapse). Остальные
	// адреса возврата получают страницу «продолжить вход в <адрес>».
	ClientWhitelist []string `yaml:"client_whitelist"`
	// UpdateProfileInformation - обновлять displayname из claims при каждом входе
	// (sso.update_profile_information Synapse, по умолчанию false).
	UpdateProfileInformation bool `yaml:"update_profile_information"`
}

// OIDCProvider - провайдер OpenID Connect (элемент oidc_providers Synapse).
// Поля и значения по умолчанию - как в Synapse, конфиг переносится копированием.
type OIDCProvider struct {
	// IDPID - идентификатор провайдера (idp_id). Как в Synapse, к нему добавляется
	// префикс "oidc-" (кроме "oidc"): это auth_provider в user_external_ids и id в
	// /login/sso/redirect/{idpId}. По умолчанию "oidc".
	IDPID string `yaml:"idp_id"`
	// IDPName - название на кнопке «Continue with ...» (по умолчанию "OIDC").
	IDPName string `yaml:"idp_name"`
	// IDPIcon - mxc:// иконки для клиента, IDPBrand - бренд (org.matrix.gitlab и т.п.).
	IDPIcon  string `yaml:"idp_icon"`
	IDPBrand string `yaml:"idp_brand"`
	// Enabled - nil или true - включен.
	Enabled *bool `yaml:"enabled"`
	// Discover - читать .well-known/openid-configuration (по умолчанию true). При
	// false адреса задаются явно: authorization_endpoint, token_endpoint, jwks_uri.
	Discover *bool  `yaml:"discover"`
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client_id"`
	// ClientSecret или ClientSecretPath (файл, как client_secret_path Synapse);
	// переменная SYNGOD_OIDC_CLIENT_SECRET_<IDP_ID> имеет приоритет.
	ClientSecret     string `yaml:"client_secret"`
	ClientSecretPath string `yaml:"client_secret_path"`
	// ClientAuthMethod - client_secret_basic (по умолчанию), client_secret_post, none.
	ClientAuthMethod string `yaml:"client_auth_method"`
	// PKCEMethod - auto (если провайдер объявил S256), always, never.
	PKCEMethod string `yaml:"pkce_method"`
	// Scopes - запрашиваемые scope; openid добавляется всегда.
	Scopes                []string `yaml:"scopes"`
	AuthorizationEndpoint string   `yaml:"authorization_endpoint"`
	TokenEndpoint         string   `yaml:"token_endpoint"`
	UserinfoEndpoint      string   `yaml:"userinfo_endpoint"`
	JWKSURI               string   `yaml:"jwks_uri"`
	// IDTokenSigningAlgs - алгоритмы подписи id_token при discover: false (по
	// умолчанию RS256).
	IDTokenSigningAlgs []string `yaml:"id_token_signing_alg_values_supported"`
	// UserProfileMethod - auto (claims из id_token) или userinfo_endpoint
	// (дополнительно запрос userinfo).
	UserProfileMethod string `yaml:"user_profile_method"`
	// AllowExistingUsers - привязать SSO к существующей учетке с тем же localpart
	// (allow_existing_users Synapse). false - подбирается свободный localpart с
	// числовым суффиксом.
	AllowExistingUsers bool `yaml:"allow_existing_users"`
	// UserMappingProvider - шаблоны localpart и displayname (как в Synapse).
	UserMappingProvider OIDCUserMapping `yaml:"user_mapping_provider"`
	// AttributeRequirements - обязательные значения claims (attribute_requirements).
	AttributeRequirements []OIDCAttributeRequirement `yaml:"attribute_requirements"`
	// Timeout - запросы к провайдеру (по умолчанию 10s).
	Timeout Duration `yaml:"timeout"`
}

// OIDCUserMapping - user_mapping_provider Synapse; module - только встроенный
// JinjaOidcMappingProvider.
type OIDCUserMapping struct {
	Module string                `yaml:"module"`
	Config OIDCUserMappingConfig `yaml:"config"`
}

// OIDCUserMappingConfig - config JinjaOidcMappingProvider.
type OIDCUserMappingConfig struct {
	// SubjectClaim - claim с постоянным идентификатором (по умолчанию sub).
	SubjectClaim string `yaml:"subject_claim"`
	// LocalpartTemplate - обязателен: выбора имени пользователем (username picker
	// Synapse) нет.
	LocalpartTemplate   string `yaml:"localpart_template"`
	DisplayNameTemplate string `yaml:"display_name_template"`
	// EmailTemplate принимается для совместимости конфига и не используется: 3PID в
	// SYNGO нет.
	EmailTemplate string `yaml:"email_template"`
	// ConfirmLocalpart и подобные опции Synapse не поддержаны (KnownFields).
}

// OIDCAttributeRequirement - claim attribute должен быть равен value (или список
// содержать value).
type OIDCAttributeRequirement struct {
	Attribute string `yaml:"attribute"`
	Value     string `yaml:"value"`
}

// jinjaModule - встроенный модуль маппинга Synapse.
const jinjaModule = "synapse.handlers.oidc.JinjaOidcMappingProvider"

// idpIDPattern - допустимые символы idp_id (как в Synapse).
var idpIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// IsEnabled - провайдер включен.
func (p OIDCProvider) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// Discovers - читать ли discovery-документ (по умолчанию да).
func (p OIDCProvider) Discovers() bool {
	return p.Discover == nil || *p.Discover
}

// AuthProviderID - идентификатор провайдера с префиксом "oidc-", как в Synapse
// (synapse/config/oidc.py): так совпадают auth_provider в перенесенной
// user_external_ids и idpId в адресах клиентов.
func (p OIDCProvider) AuthProviderID() string {
	id := p.IDPID
	if id == "" {
		id = "oidc"
	}

	if id == "oidc" {
		return id
	}

	return "oidc-" + id
}

// DisplayName - название провайдера для кнопки входа.
func (p OIDCProvider) DisplayName() string {
	if p.IDPName != "" {
		return p.IDPName
	}

	return "OIDC"
}

// SubjectClaim - claim субъекта (по умолчанию sub).
func (p OIDCProvider) SubjectClaim() string {
	if claim := p.UserMappingProvider.Config.SubjectClaim; claim != "" {
		return claim
	}

	return "sub"
}

// ClientSecretEnv - имя переменной окружения с client_secret провайдера.
func (p OIDCProvider) ClientSecretEnv() string {
	id := p.IDPID
	if id == "" {
		id = "oidc"
	}

	var b strings.Builder

	b.WriteString(EnvOIDCClientSecretPrefix)

	for _, r := range strings.ToUpper(id) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	return b.String()
}

// ResolveClientSecret - секрет клиента: переменная окружения, файл или значение
// из конфига (в этом порядке).
func (p OIDCProvider) ResolveClientSecret() (string, error) {
	if v := os.Getenv(p.ClientSecretEnv()); v != "" {
		return v, nil
	}

	if p.ClientSecretPath != "" {
		data, err := os.ReadFile(p.ClientSecretPath)
		if err != nil {
			return "", fmt.Errorf("config: oidc %s: client_secret_path: %w", p.AuthProviderID(), err)
		}

		return strings.TrimSpace(string(data)), nil
	}

	return p.ClientSecret, nil
}

// PublicBaseURL - server.public_baseurl с завершающим "/".
func (s Server) PublicBaseURL() string {
	base := strings.TrimSpace(s.PublicBaseurl)
	if base != "" && !strings.HasSuffix(base, "/") {
		base += "/"
	}

	return base
}

// EnabledOIDCProviders - включенные провайдеры OIDC.
func (a Auth) EnabledOIDCProviders() []OIDCProvider {
	out := make([]OIDCProvider, 0, len(a.OIDCProviders))

	for i := range a.OIDCProviders {
		if a.OIDCProviders[i].IsEnabled() {
			out = append(out, a.OIDCProviders[i])
		}
	}

	return out
}

// validateOIDC проверяет oidc_providers и связанные настройки.
func (c Config) validateOIDC() error {
	providers := c.Auth.EnabledOIDCProviders()
	if len(providers) == 0 {
		return nil
	}

	if err := c.validatePublicBaseURL(); err != nil {
		return err
	}

	seen := map[string]bool{}

	for i := range providers {
		provider := &providers[i]
		if err := c.validateOIDCProvider(provider); err != nil {
			return fmt.Errorf("config: auth.oidc_providers[%s]: %w", provider.AuthProviderID(), err)
		}

		id := provider.AuthProviderID()
		if seen[id] {
			return fmt.Errorf("config: auth.oidc_providers: idp_id %q повторяется", id)
		}

		seen[id] = true
	}

	for i, raw := range c.Auth.SSO.ClientWhitelist {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" {
			return fmt.Errorf("config: auth.sso.client_whitelist[%d] %q: нужен абсолютный адрес", i, raw)
		}
	}

	return nil
}

// validatePublicBaseURL - public_baseurl нужен для адреса callback OIDC.
func (c Config) validatePublicBaseURL() error {
	base := c.Server.PublicBaseURL()
	if base == "" {
		return errors.New("config: server.public_baseurl обязателен при auth.oidc_providers " +
			"(адрес callback - <public_baseurl>_synapse/client/oidc/callback)")
	}

	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("config: server.public_baseurl %q: нужен http(s)-адрес", base)
	}

	if parsed.Scheme != "https" && !c.IsDev() {
		return fmt.Errorf("config: server.public_baseurl %q: вне dev/test нужен https", base)
	}

	return nil
}

// validateOIDCProvider проверяет одного провайдера.
func (c Config) validateOIDCProvider(p *OIDCProvider) error {
	if p.IDPID != "" && (!idpIDPattern.MatchString(p.IDPID) || len(p.IDPID) > 250) {
		return fmt.Errorf("idp_id %q: допустимы латиница, цифры и ._~-", p.IDPID)
	}

	if err := c.validateOIDCEndpoints(p); err != nil {
		return err
	}

	if p.ClientID == "" {
		return errors.New("client_id обязателен")
	}

	switch p.ClientAuthMethod {
	case "", "client_secret_basic", "client_secret_post":
		if p.ClientSecret == "" && p.ClientSecretPath == "" && os.Getenv(p.ClientSecretEnv()) == "" {
			return fmt.Errorf("нужен client_secret, client_secret_path или %s", p.ClientSecretEnv())
		}
	case "none":
	default:
		return fmt.Errorf("client_auth_method %q не поддержан (client_secret_basic|client_secret_post|none)",
			p.ClientAuthMethod)
	}

	if p.ClientSecret != "" && p.ClientSecretPath != "" {
		return errors.New("client_secret и client_secret_path взаимоисключающие")
	}

	switch p.PKCEMethod {
	case "", "auto", "always", "never":
	default:
		return fmt.Errorf("pkce_method %q: ожидается auto|always|never", p.PKCEMethod)
	}

	switch p.UserProfileMethod {
	case "", "auto", "userinfo_endpoint":
	default:
		return fmt.Errorf("user_profile_method %q: ожидается auto|userinfo_endpoint", p.UserProfileMethod)
	}

	if p.Timeout < 0 {
		return errors.New("timeout must not be negative")
	}

	for _, req := range p.AttributeRequirements {
		if req.Attribute == "" {
			return errors.New("attribute_requirements: attribute обязателен")
		}
	}

	return validateOIDCMapping(p.UserMappingProvider)
}

// validateOIDCEndpoints - issuer и адреса провайдера.
func (c Config) validateOIDCEndpoints(p *OIDCProvider) error {
	if p.Issuer == "" {
		return errors.New("issuer обязателен")
	}

	endpoints := map[string]string{"issuer": p.Issuer}

	if !p.Discovers() {
		if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" || p.JWKSURI == "" {
			return errors.New("при discover: false нужны authorization_endpoint, token_endpoint и jwks_uri")
		}

		endpoints["authorization_endpoint"] = p.AuthorizationEndpoint
		endpoints["token_endpoint"] = p.TokenEndpoint
		endpoints["jwks_uri"] = p.JWKSURI

		if p.UserinfoEndpoint != "" {
			endpoints["userinfo_endpoint"] = p.UserinfoEndpoint
		}
	}

	for name, raw := range endpoints {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return fmt.Errorf("%s %q: нужен http(s)-адрес", name, raw)
		}

		// Как в Synapse (без skip_verification): по http токены и секрет клиента
		// идут открытым текстом.
		if parsed.Scheme != "https" && !c.IsDev() {
			return fmt.Errorf("%s %q: вне dev/test нужен https", name, raw)
		}
	}

	return nil
}

// validateOIDCMapping - шаблоны маппинга разбираются при старте.
func validateOIDCMapping(m OIDCUserMapping) error {
	if m.Module != "" && m.Module != jinjaModule {
		return fmt.Errorf("user_mapping_provider.module %q не поддержан (только встроенный %s)", m.Module, jinjaModule)
	}

	if strings.TrimSpace(m.Config.LocalpartTemplate) == "" {
		return errors.New("user_mapping_provider.config.localpart_template обязателен " +
			"(выбор имени пользователем не поддержан)")
	}

	for name, source := range map[string]string{
		"localpart_template":    m.Config.LocalpartTemplate,
		"display_name_template": m.Config.DisplayNameTemplate,
	} {
		if _, err := mapping.Parse(source); err != nil {
			return fmt.Errorf("user_mapping_provider.config.%s: %w", name, err)
		}
	}

	return nil
}
