package sso_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/sso"
	"github.com/aisa-it/syngo/internal/sso/ssotest"
)

const callback = "https://matrix.test/_synapse/client/oidc/callback"

func providerConfig(idp *ssotest.Provider) config.OIDCProvider {
	return config.OIDCProvider{
		IDPID: "test", IDPName: "Test IdP", Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		Scopes: []string{"openid", "profile"},
		UserMappingProvider: config.OIDCUserMapping{Config: config.OIDCUserMappingConfig{
			LocalpartTemplate:   "{{ user.preferred_username }}",
			DisplayNameTemplate: "{{ user.name }}",
		}},
	}
}

func newProvider(t *testing.T, cfg config.OIDCProvider) *sso.Provider {
	t.Helper()

	provider, err := sso.NewProvider(cfg, sso.Options{RedirectURI: callback})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	return provider
}

func newCodec(t *testing.T, now func() time.Time) *sso.SessionCodec {
	t.Helper()

	key := make([]byte, 32)
	_, _ = rand.Read(key)

	codec, err := sso.NewSessionCodec(key, now)
	if err != nil {
		t.Fatal(err)
	}

	return codec
}

// authorize проходит authorize у провайдера и возвращает code и state из редиректа.
func authorize(t *testing.T, authURL string) (code, state string, query url.Values) {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, authURL, http.NoBody)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}

	location, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(location.String(), callback) {
		t.Fatalf("redirect не на callback: %s", location)
	}

	parsed, _ := url.Parse(authURL)

	return location.Query().Get("code"), location.Query().Get("state"), parsed.Query()
}

// login - полный вход: сессия, authorize, обмен code.
func login(t *testing.T, provider *sso.Provider, codec *sso.SessionCodec) (sso.Identity, error) {
	t.Helper()

	session, err := codec.NewSession(provider.ID, "https://app/", "")
	if err != nil {
		t.Fatal(err)
	}

	authURL, err := provider.AuthorizationURL(context.Background(), &session)
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}

	code, state, _ := authorize(t, authURL)
	if state != session.State {
		t.Fatalf("state не вернулся: %q", state)
	}

	return provider.Exchange(context.Background(), code, session)
}

// TestProviderLogin - полный путь OIDC с discovery, PKCE и проверкой id_token.
func TestProviderLogin(t *testing.T) {
	t.Parallel()

	idp := ssotest.New(t, "matrix", "secret")
	idp.Login(map[string]any{"sub": "u-1", "preferred_username": "Ivan.Petrov", "name": " Иван Петров ", "email": "ivan@x"})

	provider := newProvider(t, providerConfig(idp))
	if provider.ID != "oidc-test" || provider.Name != "Test IdP" {
		t.Fatalf("провайдер: %+v", provider)
	}

	codec := newCodec(t, nil)

	session, _ := codec.NewSession(provider.ID, "https://app/", "")

	authURL, err := provider.AuthorizationURL(context.Background(), &session)
	if err != nil {
		t.Fatal(err)
	}

	code, _, query := authorize(t, authURL)

	if query.Get("scope") != "openid profile" || query.Get("nonce") != session.Nonce ||
		query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != callback {
		t.Fatalf("authorize: %v", query)
	}

	identity, err := provider.Exchange(context.Background(), code, session)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if identity.Subject != "u-1" || provider.DisplayName(identity.Claims) != "Иван Петров" {
		t.Fatalf("identity: %+v", identity)
	}

	candidates := provider.Localpart(identity.Claims)
	if first, _ := candidates(0); first != "ivan.petrov" {
		t.Fatalf("localpart: %q", first)
	}

	if second, _ := candidates(2); second != "ivan.petrov2" {
		t.Fatalf("localpart с суффиксом: %q", second)
	}

	// code одноразовый: повторный обмен отклоняет сам провайдер.
	if _, err := provider.Exchange(context.Background(), code, session); !errors.Is(err, sso.ErrProviderUnavailable) {
		t.Fatalf("повтор code: %v", err)
	}

	if provider.AllowExistingUsers() {
		t.Fatal("allow_existing_users по умолчанию false")
	}
}

// TestProviderRejectsForgedIDToken - id_token, подписанный чужим ключом, с чужим
// nonce, истекший, с чужими aud или iss, не принимается.
func TestProviderRejectsForgedIDToken(t *testing.T) {
	t.Parallel()

	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	otherNonce := "attacker-nonce"

	cases := map[string]func(*ssotest.Provider){
		"чужой ключ":  func(i *ssotest.Provider) { i.SignWith = foreign },
		"чужой nonce": func(i *ssotest.Provider) { i.Nonce = &otherNonce },
		"истекший":    func(i *ssotest.Provider) { i.TTL = -time.Minute },
		"чужой aud":   func(i *ssotest.Provider) { i.Audience = "other-client" },
		"чужой iss":   func(i *ssotest.Provider) { i.TokenIssuer = "https://evil.example" },
	}

	for name, attack := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			idp := ssotest.New(t, "matrix", "secret")
			idp.Login(map[string]any{"sub": "u-1", "preferred_username": "ivan"})
			idp.Configure(attack)

			_, err := login(t, newProvider(t, providerConfig(idp)), newCodec(t, nil))
			if !errors.Is(err, sso.ErrInvalidIDToken) {
				t.Fatalf("Exchange = %v, ожидали ErrInvalidIDToken", err)
			}
		})
	}
}

// TestProviderOptions - PKCE never/always, discover: false, client_secret_post,
// userinfo, attribute_requirements, недоступный провайдер, неверный секрет.
func TestProviderOptions(t *testing.T) {
	t.Parallel()

	t.Run("pkce never", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		idp.Login(map[string]any{"sub": "1", "preferred_username": "a"})

		cfg := providerConfig(idp)
		cfg.PKCEMethod = "never"
		provider := newProvider(t, cfg)
		codec := newCodec(t, nil)

		session, _ := codec.NewSession(provider.ID, "", "")
		authURL, _ := provider.AuthorizationURL(context.Background(), &session)

		code, _, query := authorize(t, authURL)
		if query.Get("code_challenge") != "" || session.Verifier != "" {
			t.Fatalf("PKCE при never: %v", query)
		}

		if _, err := provider.Exchange(context.Background(), code, session); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("pkce always без discovery", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		idp.Login(map[string]any{"id": float64(42), "nick": "Bob"})
		idp.Configure(func(i *ssotest.Provider) { i.AdvertisePKCE, i.RequirePKCE = false, true })

		disabled := false
		cfg := providerConfig(idp)
		cfg.Discover = &disabled
		cfg.AuthorizationEndpoint, cfg.TokenEndpoint, cfg.JWKSURI = idp.Issuer+"/authorize", idp.Issuer+"/token", idp.Issuer+"/jwks"
		cfg.PKCEMethod = "always"
		cfg.ClientAuthMethod = "client_secret_post"
		cfg.UserMappingProvider.Config.SubjectClaim = "id"
		cfg.UserMappingProvider.Config.LocalpartTemplate = "{{ user.nick }}"

		identity, err := login(t, newProvider(t, cfg), newCodec(t, nil))
		if err != nil || identity.Subject != "42" {
			t.Fatalf("discover: false: %+v %v", identity, err)
		}
	})

	t.Run("pkce auto без S256", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		idp.Login(map[string]any{"sub": "1", "preferred_username": "a"})
		idp.Configure(func(i *ssotest.Provider) { i.AdvertisePKCE = false })

		provider := newProvider(t, providerConfig(idp))
		session, _ := newCodec(t, nil).NewSession(provider.ID, "", "")
		authURL, _ := provider.AuthorizationURL(context.Background(), &session)

		if _, _, query := authorize(t, authURL); query.Get("code_challenge") != "" {
			t.Fatal("PKCE без объявления S256")
		}
	})

	t.Run("userinfo", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		idp.Login(map[string]any{"sub": "1"})
		idp.Configure(func(i *ssotest.Provider) {
			i.UserInfo = map[string]any{"sub": "1", "preferred_username": "from-userinfo"}
		})

		cfg := providerConfig(idp)
		cfg.UserProfileMethod = "userinfo_endpoint"
		provider := newProvider(t, cfg)

		identity, err := login(t, provider, newCodec(t, nil))
		if err != nil || mustLocalpart(t, provider, identity) != "from-userinfo" {
			t.Fatalf("userinfo: %+v %v", identity, err)
		}

		idp.Configure(func(i *ssotest.Provider) { i.UserInfo = map[string]any{"sub": "2"} })

		if _, err := login(t, provider, newCodec(t, nil)); !errors.Is(err, sso.ErrInvalidIDToken) {
			t.Fatalf("sub userinfo другой: %v", err)
		}
	})

	t.Run("attribute_requirements", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		cfg := providerConfig(idp)
		cfg.AttributeRequirements = []config.OIDCAttributeRequirement{
			{Attribute: "groups", Value: "matrix"}, {Attribute: "email_verified", Value: "true"},
		}
		provider := newProvider(t, cfg)

		idp.Login(map[string]any{"sub": "1", "preferred_username": "a", "groups": []any{"staff", "matrix"}, "email_verified": true})

		if _, err := login(t, provider, newCodec(t, nil)); err != nil {
			t.Fatalf("требования выполнены: %v", err)
		}

		idp.Login(map[string]any{"sub": "1", "preferred_username": "a", "groups": []any{"staff"}, "email_verified": true})

		if _, err := login(t, provider, newCodec(t, nil)); !errors.Is(err, sso.ErrNotAllowed) {
			t.Fatalf("нет группы: %v", err)
		}

		idp.Login(map[string]any{"sub": "1", "preferred_username": "a", "groups": "matrix"})

		if _, err := login(t, provider, newCodec(t, nil)); !errors.Is(err, sso.ErrNotAllowed) {
			t.Fatalf("нет email_verified: %v", err)
		}
	})

	t.Run("нет subject и localpart", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		cfg := providerConfig(idp)
		cfg.UserMappingProvider.Config.SubjectClaim = "oid"
		provider := newProvider(t, cfg)
		idp.Login(map[string]any{"sub": "1"})

		if _, err := login(t, provider, newCodec(t, nil)); !errors.Is(err, sso.ErrMapping) {
			t.Fatalf("нет claim subject: %v", err)
		}

		if _, err := provider.Localpart(map[string]any{})(0); !errors.Is(err, sso.ErrMapping) {
			t.Fatalf("пустой localpart: %v", err)
		}
	})

	t.Run("неверный секрет", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		idp.Login(map[string]any{"sub": "1", "preferred_username": "a"})

		cfg := providerConfig(idp)
		cfg.ClientSecret = "wrong"

		if _, err := login(t, newProvider(t, cfg), newCodec(t, nil)); !errors.Is(err, sso.ErrProviderUnavailable) {
			t.Fatalf("неверный секрет: %v", err)
		}
	})

	t.Run("провайдер недоступен", func(t *testing.T) {
		t.Parallel()

		idp := ssotest.New(t, "matrix", "secret")
		cfg := providerConfig(idp)
		idp.Server.Close()

		provider := newProvider(t, cfg)
		session, _ := newCodec(t, nil).NewSession(provider.ID, "", "")

		if _, err := provider.AuthorizationURL(context.Background(), &session); !errors.Is(err, sso.ErrProviderUnavailable) {
			t.Fatalf("недоступен: %v", err)
		}

		if _, err := provider.Exchange(context.Background(), "x", session); !errors.Is(err, sso.ErrProviderUnavailable) {
			t.Fatalf("недоступен при обмене: %v", err)
		}
	})

	t.Run("ошибки конфига", func(t *testing.T) {
		t.Parallel()

		cfg := config.OIDCProvider{ClientSecretPath: "/nonexistent/secret"}
		if _, err := sso.NewProvider(cfg, sso.Options{}); err == nil {
			t.Fatal("нечитаемый секрет")
		}

		cfg = config.OIDCProvider{UserMappingProvider: config.OIDCUserMapping{Config: config.OIDCUserMappingConfig{LocalpartTemplate: "{{ x }}"}}}
		if _, err := sso.NewProvider(cfg, sso.Options{}); err == nil {
			t.Fatal("плохой localpart_template")
		}

		cfg.UserMappingProvider.Config = config.OIDCUserMappingConfig{LocalpartTemplate: "{{ user.a }}", DisplayNameTemplate: "{{ x }}"}
		if _, err := sso.NewProvider(cfg, sso.Options{}); err == nil {
			t.Fatal("плохой display_name_template")
		}
	})
}

func mustLocalpart(t *testing.T, provider *sso.Provider, identity sso.Identity) string {
	t.Helper()

	localpart, err := provider.Localpart(identity.Claims)(0)
	if err != nil {
		t.Fatal(err)
	}

	return localpart
}
