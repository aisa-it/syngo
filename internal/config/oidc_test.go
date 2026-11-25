package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/config"
)

// TestOIDCConfig - oidc_providers в формате Synapse: префикс oidc- у
// idp_id, значения по умолчанию, секрет из файла и из окружения.
func TestOIDCConfig(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")

	if err := os.WriteFile(secretFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "config.yaml")
	body := `env: prod
matrix:
  server_name: matrix.example.org
database:
  dsn: postgres://syngo:pw@db:5432/syngo
sync:
  token_secret: "` + strings.Repeat("s", 32) + `"
server:
  public_baseurl: https://matrix.example.org
auth:
  password_localdb_enabled: false
  sso:
    client_whitelist: ["https://element.example.org/"]
    update_profile_information: true
  oidc_providers:
    - idp_id: keycloak
      idp_name: "Корпоративный вход"
      idp_brand: "org.matrix.keycloak"
      issuer: "https://sso.example.org/realms/main"
      client_id: matrix
      client_secret_path: ` + secretFile + `
      scopes: ["openid", "profile"]
      user_mapping_provider:
        config:
          localpart_template: "{{ user.preferred_username }}"
          display_name_template: "{{ user.name }}"
          email_template: "{{ user.email }}"
    - issuer: "https://gitlab.example.org"
      client_id: matrix2
      client_secret: inline
      client_auth_method: client_secret_post
      pkce_method: always
      discover: false
      authorization_endpoint: https://gitlab.example.org/oauth/authorize
      token_endpoint: https://gitlab.example.org/oauth/token
      jwks_uri: https://gitlab.example.org/oauth/discovery/keys
      user_mapping_provider:
        module: synapse.handlers.oidc.JinjaOidcMappingProvider
        config:
          subject_claim: id
          localpart_template: "{{ user.nickname | lower }}"
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	providers := cfg.Auth.EnabledOIDCProviders()
	if len(providers) != 2 {
		t.Fatalf("providers = %+v", providers)
	}

	keycloak, gitlab := providers[0], providers[1]
	if keycloak.AuthProviderID() != "oidc-keycloak" || gitlab.AuthProviderID() != "oidc" ||
		keycloak.DisplayName() != "Корпоративный вход" || gitlab.DisplayName() != "OIDC" ||
		keycloak.SubjectClaim() != "sub" || gitlab.SubjectClaim() != "id" ||
		!keycloak.Discovers() || gitlab.Discovers() {
		t.Fatalf("провайдеры: %+v", providers)
	}

	if secret, err := keycloak.ResolveClientSecret(); err != nil || secret != "from-file" {
		t.Fatalf("секрет из файла: %q %v", secret, err)
	}

	if keycloak.ClientSecretEnv() != "SYNGOD_OIDC_CLIENT_SECRET_KEYCLOAK" || gitlab.ClientSecretEnv() != "SYNGOD_OIDC_CLIENT_SECRET_OIDC" {
		t.Fatalf("env: %s %s", keycloak.ClientSecretEnv(), gitlab.ClientSecretEnv())
	}

	missing := keycloak
	missing.ClientSecret, missing.ClientSecretPath = "", filepath.Join(dir, "absent")

	if _, err := missing.ResolveClientSecret(); err == nil {
		t.Fatal("нечитаемый client_secret_path без ошибки")
	}

	t.Setenv("SYNGOD_OIDC_CLIENT_SECRET_KEYCLOAK", "from-env")

	if secret, _ := keycloak.ResolveClientSecret(); secret != "from-env" {
		t.Fatalf("секрет из env: %q", secret)
	}

	if cfg.Server.PublicBaseURL() != "https://matrix.example.org/" || !cfg.Auth.SSO.UpdateProfileInformation {
		t.Fatalf("public_baseurl/sso: %q %+v", cfg.Server.PublicBaseURL(), cfg.Auth.SSO)
	}
}

// TestOIDCConfigValidation - ошибки конфига OIDC видны при старте, а не при входе.
func TestOIDCConfigValidation(t *testing.T) {
	base := func() config.Config {
		cfg := validProd()
		cfg.Server.PublicBaseurl = "https://matrix.example.org/"
		cfg.Auth.OIDCProviders = []config.OIDCProvider{{
			IDPID: "kc", Issuer: "https://sso.example.org", ClientID: "matrix", ClientSecret: "s",
			UserMappingProvider: config.OIDCUserMapping{Config: config.OIDCUserMappingConfig{
				LocalpartTemplate: "{{ user.preferred_username }}",
			}},
		}}

		return cfg
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("базовый конфиг: %v", err)
	}

	disabled := false
	cases := map[string]func(*config.Config){
		"public_baseurl обязателен":  func(c *config.Config) { c.Server.PublicBaseurl = "" },
		"public_baseurl http в prod": func(c *config.Config) { c.Server.PublicBaseurl = "http://matrix.example.org" },
		"public_baseurl не адрес":    func(c *config.Config) { c.Server.PublicBaseurl = "matrix" },
		"issuer http в prod":         func(c *config.Config) { c.Auth.OIDCProviders[0].Issuer = "http://sso.example.org" },
		"issuer пуст":                func(c *config.Config) { c.Auth.OIDCProviders[0].Issuer = "" },
		"issuer не адрес":            func(c *config.Config) { c.Auth.OIDCProviders[0].Issuer = "sso" },
		"client_id":                  func(c *config.Config) { c.Auth.OIDCProviders[0].ClientID = "" },
		"секрет":                     func(c *config.Config) { c.Auth.OIDCProviders[0].ClientSecret = "" },
		"секрет дважды":              func(c *config.Config) { c.Auth.OIDCProviders[0].ClientSecretPath = "/x" },
		"client_auth_method":         func(c *config.Config) { c.Auth.OIDCProviders[0].ClientAuthMethod = "private_key_jwt" },
		"pkce_method":                func(c *config.Config) { c.Auth.OIDCProviders[0].PKCEMethod = "plain" },
		"user_profile_method":        func(c *config.Config) { c.Auth.OIDCProviders[0].UserProfileMethod = "x" },
		"idp_id":                     func(c *config.Config) { c.Auth.OIDCProviders[0].IDPID = "a b" },
		"timeout":                    func(c *config.Config) { c.Auth.OIDCProviders[0].Timeout = -1 },
		"attribute_requirements": func(c *config.Config) {
			c.Auth.OIDCProviders[0].AttributeRequirements = []config.OIDCAttributeRequirement{{}}
		},
		"discover false без адресов":    func(c *config.Config) { f := false; c.Auth.OIDCProviders[0].Discover = &f },
		"localpart_template обязателен": func(c *config.Config) { c.Auth.OIDCProviders[0].UserMappingProvider.Config.LocalpartTemplate = "" },
		"localpart_template Jinja": func(c *config.Config) {
			c.Auth.OIDCProviders[0].UserMappingProvider.Config.LocalpartTemplate = "{{ user.x.split('@') }}"
		},
		"display_name_template Jinja": func(c *config.Config) {
			c.Auth.OIDCProviders[0].UserMappingProvider.Config.DisplayNameTemplate = "{% x %}"
		},
		"module":           func(c *config.Config) { c.Auth.OIDCProviders[0].UserMappingProvider.Module = "my.Module" },
		"client_whitelist": func(c *config.Config) { c.Auth.SSO.ClientWhitelist = []string{"element.example.org"} },
		"повтор idp_id": func(c *config.Config) {
			c.Auth.OIDCProviders = append(c.Auth.OIDCProviders, c.Auth.OIDCProviders[0])
		},
		"discover false http в prod": func(c *config.Config) {
			f := false
			p := &c.Auth.OIDCProviders[0]
			p.Discover = &f
			p.AuthorizationEndpoint, p.TokenEndpoint, p.JWKSURI = "https://a/x", "http://a/token", "https://a/keys"
		},
	}

	for name, mutate := range cases {
		cfg := base()
		mutate(&cfg)

		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: конфиг принят", name)
		}
	}

	// Выключенный провайдер не проверяется; dev разрешает http (стенд, тесты).
	cfg := base()
	cfg.Auth.OIDCProviders[0].Enabled = &disabled
	cfg.Auth.OIDCProviders[0].Issuer = ""

	if err := cfg.Validate(); err != nil {
		t.Fatalf("выключенный провайдер: %v", err)
	}

	dev := base()
	dev.Env = "dev"
	dev.Server.PublicBaseurl = "http://localhost:8008"
	dev.Auth.OIDCProviders[0].Issuer = "http://127.0.0.1:5556"
	dev.Auth.OIDCProviders[0].ClientAuthMethod = "none"
	dev.Auth.OIDCProviders[0].ClientSecret = ""

	if err := dev.Validate(); err != nil {
		t.Fatalf("dev с http: %v", err)
	}

	// Только SSO без локальных паролей и LDAP - допустимо.
	ssoOnly := base()
	f := false
	ssoOnly.Auth.PasswordLocalDB = &f

	if err := ssoOnly.Validate(); err != nil {
		t.Fatalf("только SSO: %v", err)
	}
}
