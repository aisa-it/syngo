package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/auth"
	"github.com/aisa-it/syngo/internal/core"
)

// fakeProvider - подставной внешний провайдер (LDAP) для тестов.
type fakeProvider struct {
	users map[string]string // username -> пароль
	names map[string]string // username -> displayname
	err   error
	calls int
}

func (p *fakeProvider) Name() string { return "fake-ldap" }

func (p *fakeProvider) CheckPassword(_ context.Context, username, password string) (core.ExternalIdentity, bool, error) {
	p.calls++

	if p.err != nil {
		return core.ExternalIdentity{}, false, p.err
	}

	// Каталог сравнивает имена без учета регистра, как LDAP.
	for name, want := range p.users {
		if strings.EqualFold(name, username) && want == password {
			return core.ExternalIdentity{Localpart: username, DisplayName: p.names[name], SyncDisplayName: true}, true, nil
		}
	}

	return core.ExternalIdentity{}, false, nil
}

type externalLogin struct {
	user     core.User
	identity core.ExternalIdentity
	created  bool
}

// TestLoginEmptyHashForbidden - пользователь без локального пароля (LDAP, SSO,
// password_hash IS NULL в Synapse) получает тот же отказ, что и при неверном
// пароле; раньше MultiHasher возвращал ошибку, и /login отвечал 500.
func TestLoginEmptyHashForbidden(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.users["ldapuser"] = core.User{UserID: "@ldapuser:test.local", Localpart: "ldapuser"}

	svc := core.NewAuth(store, auth.NewMultiHasher(), &fakeTokens{}, core.AuthOptions{ServerName: "test.local"})

	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "ldapuser", Password: "anything",
	}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login(пустой хеш) = %v, ожидали ErrInvalidCredentials", err)
	}

	if err := svc.VerifyPassword(context.Background(), "@ldapuser:test.local", "anything"); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("VerifyPassword(пустой хеш) = %v, ожидали ErrInvalidCredentials", err)
	}
}

// TestLoginViaProviderCreatesUser - первый вход через провайдер создает учетную
// запись (localpart в нижнем регистре, имя из каталога, без пароля), повторный
// входит в нее же (как synapse-ldap3).
func TestLoginViaProviderCreatesUser(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{
		users: map[string]string{"Ivanov": "ldap-pass"},
		names: map[string]string{"Ivanov": "Иванов Иван"},
	}

	var logins []externalLogin

	svc, store := newAuth(t, core.AuthOptions{
		PasswordProviders: []core.PasswordProvider{provider},
		OnExternalLogin: func(_ context.Context, user core.User, identity core.ExternalIdentity, created bool) {
			logins = append(logins, externalLogin{user, identity, created})
		},
	})

	result, err := svc.Login(context.Background(), core.LoginRequest{Identifier: "@Ivanov:test.local", Password: "ldap-pass"})
	if err != nil {
		t.Fatalf("Login через провайдер: %v", err)
	}

	user := store.users["ivanov"]
	if result.UserID != "@ivanov:test.local" || user.DisplayName != "Иванов Иван" || user.PasswordHash != "" {
		t.Fatalf("result = %+v, user = %+v", result, user)
	}

	if _, err := svc.Login(context.Background(), core.LoginRequest{Identifier: "Ivanov", Password: "ldap-pass"}); err != nil {
		t.Fatalf("повторный Login: %v", err)
	}

	if len(logins) != 2 || !logins[0].created || logins[1].created || logins[1].identity.Provider != "fake-ldap" {
		t.Fatalf("OnExternalLogin = %+v", logins)
	}

	// Неверный пароль в каталоге: локального пароля нет - отказ.
	if _, err := svc.Login(context.Background(), core.LoginRequest{Identifier: "ivanov", Password: "wrong"}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login(неверный) = %v", err)
	}

	// UIA (удаление устройств, сброс кросс-подписи) подтверждается паролем каталога.
	if err := svc.VerifyPassword(context.Background(), "@ivanov:test.local", "ldap-pass"); err != nil {
		t.Fatalf("VerifyPassword через провайдер: %v", err)
	}

	// Деактивированный пользователь каталога не входит.
	user.Deactivated = true
	store.users["ivanov"] = user

	if _, err := svc.Login(context.Background(), core.LoginRequest{Identifier: "ivanov", Password: "ldap-pass"}); !errors.Is(err, core.ErrUserDeactivated) {
		t.Fatalf("Login(деактивирован) = %v", err)
	}
}

// TestLoginProviderFallsBackToLocal - недоступный или отказавший провайдер не мешает
// входу локальным паролем; password_localdb_enabled=false отключает локальные пароли.
func TestLoginProviderFallsBackToLocal(t *testing.T) {
	t.Parallel()

	var providerErrors []string

	provider := &fakeProvider{err: errors.New("ldap down")}

	svc, _ := newAuth(t, core.AuthOptions{
		PasswordProviders: []core.PasswordProvider{provider},
		OnProviderError:   func(name string, _ error) { providerErrors = append(providerErrors, name) },
	})

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "local", Password: "1234567890", InhibitLogin: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Login(context.Background(), core.LoginRequest{Identifier: "local", Password: "1234567890"}); err != nil {
		t.Fatalf("Login локальный при недоступном LDAP: %v", err)
	}

	if len(providerErrors) != 1 || providerErrors[0] != "fake-ldap" {
		t.Fatalf("OnProviderError = %v", providerErrors)
	}

	onlyLDAP, store := newAuth(t, core.AuthOptions{
		PasswordProviders:     []core.PasswordProvider{&fakeProvider{}},
		DisableLocalPasswords: true,
	})
	store.users["local"] = core.User{UserID: "@local:test.local", Localpart: "local", PasswordHash: "plain:1234567890"}

	if _, err := onlyLDAP.Login(context.Background(), core.LoginRequest{Identifier: "local", Password: "1234567890"}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login при password_localdb_enabled=false = %v", err)
	}
}
