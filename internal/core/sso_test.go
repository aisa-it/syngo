package core_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// newSSO - сервис SSO на фейках; logins - вызовы OnExternalLogin.
func newSSO(t *testing.T, now func() time.Time) (*core.SSO, *core.Auth, *fakeStore, *coretest.MemSSOStore, *[]externalLogin) {
	t.Helper()

	var logins []externalLogin

	store := newFakeStore()
	ssoStore := coretest.NewMemSSOStore()

	if now == nil {
		now = time.Now
	}

	svc := core.NewAuth(store, fakeHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName: "test.local", Now: now,
		OnExternalLogin: func(_ context.Context, user core.User, identity core.ExternalIdentity, created bool) {
			logins = append(logins, externalLogin{user: user, identity: identity, created: created})
		},
	})

	return core.NewSSO(svc, ssoStore), svc, store, ssoStore, &logins
}

// suffixed - кандидаты localpart как у JinjaOidcMappingProvider: base, base1, base2...
func suffixed(base string) func(int) (string, error) {
	return func(attempt int) (string, error) {
		if attempt == 0 {
			return base, nil
		}

		return base + strconv.Itoa(attempt), nil
	}
}

// TestSSOResolveUser - первый вход создает учетку и привязку, повторный находит ее
// по subject (даже если имя в claims сменилось); занятое имя - суффикс.
func TestSSOResolveUser(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sso, _, store, ssoStore, logins := newSSO(t, nil)
	store.users["ivan"] = core.User{UserID: "@ivan:test.local", Localpart: "ivan", PasswordHash: "plain:x"}
	store.taken["ivan"] = true

	user, created, err := sso.ResolveUser(ctx, core.SSOIdentity{
		Provider: "oidc-kc", Subject: "sub-1", Localpart: suffixed("Ivan"), DisplayName: "Иван",
	})
	if err != nil || !created || user.UserID != "@ivan1:test.local" || user.DisplayName != "Иван" || user.PasswordHash != "" {
		t.Fatalf("первый вход: %+v %v %v", user, created, err)
	}

	again, created, err := sso.ResolveUser(ctx, core.SSOIdentity{
		Provider: "oidc-kc", Subject: "sub-1", Localpart: suffixed("renamed"), DisplayName: "Иван И.", UpdateProfile: true,
	})
	if err != nil || created || again.UserID != user.UserID {
		t.Fatalf("повторный вход: %+v %v %v", again, created, err)
	}

	if len(*logins) != 2 || !(*logins)[0].created || (*logins)[1].identity.DisplayName != "Иван И." ||
		!(*logins)[1].identity.SyncDisplayName {
		t.Fatalf("OnExternalLogin: %+v", *logins)
	}

	providers, err := sso.Providers(ctx, user.UserID)
	if err != nil || len(providers) != 1 || providers[0] != "oidc-kc" {
		t.Fatalf("Providers: %v %v", providers, err)
	}

	if id, err := sso.UserIDForIdentity(ctx, "oidc-kc", "sub-1"); err != nil || id != user.UserID {
		t.Fatalf("UserIDForIdentity: %q %v", id, err)
	}

	if id, err := sso.UserIDForIdentity(ctx, "oidc-kc", "nobody"); err != nil || id != "" {
		t.Fatalf("UserIDForIdentity(нет): %q %v", id, err)
	}

	// Тот же subject у другого провайдера - другой пользователь.
	other, created, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc-gitlab", Subject: "sub-1", Localpart: suffixed("petr")})
	if err != nil || !created || other.UserID != "@petr:test.local" || other.DisplayName != "petr" {
		t.Fatalf("другой провайдер: %+v %v %v", other, created, err)
	}

	// allow_existing_users: привязка к существующей учетке с тем же localpart.
	existing, created, err := sso.ResolveUser(ctx, core.SSOIdentity{
		Provider: "oidc-kc", Subject: "sub-ivan", Localpart: suffixed("ivan"), AllowExistingUsers: true,
	})
	if err != nil || created || existing.UserID != "@ivan:test.local" {
		t.Fatalf("allow_existing_users: %+v %v %v", existing, created, err)
	}

	if id, _ := ssoStore.UserIDByExternalID(ctx, "oidc-kc", "sub-ivan"); id != "@ivan:test.local" {
		t.Fatalf("привязка allow_existing_users: %q", id)
	}
}

// TestSSOResolveUserErrors - отказы: деактивированная учетка, плохой localpart,
// сбой хранилища.
func TestSSOResolveUserErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sso, svc, store, ssoStore, _ := newSSO(t, nil)

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Localpart: suffixed("x")}); !errors.Is(err, core.ErrSSOMapping) {
		t.Fatalf("пустой subject: %v", err)
	}

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "s", Localpart: suffixed("")}); !errors.Is(err, core.ErrSSOMapping) {
		t.Fatalf("пустой localpart: %v", err)
	}

	failing := func(int) (string, error) { return "", errors.New("нет claim") }
	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "s", Localpart: failing}); !errors.Is(err, core.ErrSSOMapping) {
		t.Fatalf("ошибка шаблона: %v", err)
	}

	// Все кандидаты заняты.
	constant := func(int) (string, error) { return "busy", nil }
	store.users["busy"] = core.User{UserID: "@busy:test.local", Localpart: "busy"}
	store.taken["busy"] = true

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "s", Localpart: constant}); !errors.Is(err, core.ErrSSOMapping) {
		t.Fatalf("все имена заняты: %v", err)
	}

	// Деактивированная привязанная учетка.
	user, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "d", Localpart: suffixed("dead")})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.DeactivateAccount(ctx, user.UserID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "d", Localpart: suffixed("dead")}); !errors.Is(err, core.ErrUserDeactivated) {
		t.Fatalf("деактивированный: %v", err)
	}

	// allow_existing_users к деактивированной учетке.
	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{
		Provider: "oidc", Subject: "d2", Localpart: suffixed("dead"), AllowExistingUsers: true,
	}); !errors.Is(err, core.ErrUserDeactivated) {
		t.Fatalf("allow_existing к деактивированному: %v", err)
	}

	// Привязка на несуществующую учетку - ошибка, а не новый пользователь.
	if err := ssoStore.AddExternalID(ctx, "oidc", "ghost", "@ghost:test.local"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "ghost", Localpart: suffixed("ghost")}); err == nil {
		t.Fatal("привязка без учетки принята")
	}

	ssoStore.Err = errors.New("db down")

	if _, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "x", Localpart: suffixed("x")}); err == nil {
		t.Fatal("сбой хранилища не дошел")
	}

	if _, err := sso.IssueLoginToken(ctx, user.UserID, "oidc"); err == nil {
		t.Fatal("сбой хранилища токенов не дошел")
	}

	if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: "syl_x"}); err == nil || errors.Is(err, core.ErrInvalidLoginToken) {
		t.Fatalf("сбой хранилища при обмене: %v", err)
	}
}

// TestSSOLoginToken - loginToken одноразовый и короткоживущий.
func TestSSOLoginToken(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Now()
	clock := func() time.Time { return now }
	sso, svc, _, _, _ := newSSO(t, clock)

	user, _, err := sso.ResolveUser(ctx, core.SSOIdentity{Provider: "oidc", Subject: "s1", Localpart: suffixed("anna")})
	if err != nil {
		t.Fatal(err)
	}

	token, err := sso.IssueLoginToken(ctx, user.UserID, "oidc")
	if err != nil || len(token) < 40 {
		t.Fatalf("IssueLoginToken: %q %v", token, err)
	}

	result, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: token, DeviceID: "PHONE"})
	if err != nil || result.UserID != user.UserID || result.DeviceID != "PHONE" || result.AccessToken == "" {
		t.Fatalf("обмен: %+v %v", result, err)
	}

	if _, err := svc.WhoAmI(ctx, result.AccessToken); err != nil {
		t.Fatalf("сессия после обмена: %v", err)
	}

	// Повтор того же токена.
	if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: token}); !errors.Is(err, core.ErrInvalidLoginToken) {
		t.Fatalf("повтор: %v", err)
	}

	for name, bad := range map[string]string{"пустой": "", "чужой": "syl_unknown"} {
		if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: bad}); !errors.Is(err, core.ErrInvalidLoginToken) {
			t.Fatalf("%s токен: %v", name, err)
		}
	}

	// Истекший: время ушло дальше LoginTokenTTL.
	expired, err := sso.IssueLoginToken(ctx, user.UserID, "oidc")
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(core.LoginTokenTTL + time.Second)

	if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: expired}); !errors.Is(err, core.ErrInvalidLoginToken) {
		t.Fatalf("истекший: %v", err)
	}

	// Токен деактивированного и удаленного пользователя.
	fresh, _ := sso.IssueLoginToken(ctx, user.UserID, "oidc")
	ghost, _ := sso.IssueLoginToken(ctx, "@ghost:test.local", "oidc")

	if err := svc.DeactivateAccount(ctx, user.UserID); err != nil {
		t.Fatal(err)
	}

	if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: fresh}); !errors.Is(err, core.ErrUserDeactivated) {
		t.Fatalf("деактивированный: %v", err)
	}

	if _, err := sso.LoginWithToken(ctx, core.TokenLoginRequest{Token: ghost}); !errors.Is(err, core.ErrInvalidLoginToken) {
		t.Fatalf("нет пользователя: %v", err)
	}
}

// TestPasswordUsable - стадия m.login.password в UIA доступна только тем, у кого
// есть пароль (локальный или внешний провайдер).
func TestPasswordUsable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newFakeStore()
	store.users["pw"] = core.User{UserID: "@pw:test.local", Localpart: "pw", PasswordHash: "plain:x"}
	store.users["sso"] = core.User{UserID: "@sso:test.local", Localpart: "sso"}

	local := core.NewAuth(store, fakeHasher{}, &fakeTokens{}, core.AuthOptions{ServerName: "test.local"})

	if ok, err := local.PasswordUsable(ctx, "@pw:test.local"); !ok || err != nil {
		t.Fatalf("с паролем: %v %v", ok, err)
	}

	if ok, err := local.PasswordUsable(ctx, "@sso:test.local"); ok || err != nil {
		t.Fatalf("без пароля: %v %v", ok, err)
	}

	if _, err := local.PasswordUsable(ctx, "@ghost:test.local"); err == nil {
		t.Fatal("нет пользователя без ошибки")
	}

	noLocal := core.NewAuth(store, fakeHasher{}, &fakeTokens{}, core.AuthOptions{ServerName: "test.local", DisableLocalPasswords: true})
	if ok, _ := noLocal.PasswordUsable(ctx, "@pw:test.local"); ok {
		t.Fatal("локальные пароли выключены")
	}

	ldap := core.NewAuth(store, fakeHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName: "test.local", PasswordProviders: []core.PasswordProvider{&fakeProvider{}},
	})
	if ok, _ := ldap.PasswordUsable(ctx, "@sso:test.local"); !ok {
		t.Fatal("LDAP подтверждает пароль")
	}
}
