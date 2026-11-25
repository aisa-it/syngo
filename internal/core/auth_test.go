package core_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
)

// fakeHasher — быстрый предсказуемый хешер: тесты домена не про криптографию.
type fakeHasher struct{}

func (fakeHasher) Hash(password string) (string, error) { return "plain:" + password, nil }

func (fakeHasher) Verify(hash, password string) (bool, error) { return hash == "plain:"+password, nil }

// fakeTokens выдаёт токены по счётчику.
type fakeTokens struct{ n int }

func (f *fakeTokens) NewToken() (plain string, hash []byte, err error) {
	f.n++

	plain = fmt.Sprintf("syt_test_%d", f.n)

	return plain, core.HashToken(plain), nil
}

// fakeStore — in-memory реализация core.AuthStore.
type fakeStore struct {
	users   map[string]core.User // по localpart
	tokens  map[string]core.Session
	taken   map[string]bool
	devices map[string]core.Device
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:   map[string]core.User{},
		tokens:  map[string]core.Session{},
		taken:   map[string]bool{},
		devices: map[string]core.Device{},
	}
}

// UpdatePasswordHash — заглушка для миграции bcrypt → argon2id.
func (f *fakeStore) UpdatePasswordHash(_ context.Context, userID, passwordHash string) error {
	for localpart, user := range f.users {
		if user.UserID == userID {
			user.PasswordHash = passwordHash
			f.users[localpart] = user

			return nil
		}
	}

	return core.ErrNotFound
}

// SessionByRefreshHash — по хешу refresh ищем сессию в общем наборе токенов.
func (f *fakeStore) SessionByRefreshHash(_ context.Context, refreshHash []byte) (core.Session, error) {
	for _, session := range f.tokens {
		return session, nil
	}

	return core.Session{}, core.ErrNotFound
}

// RevokeRefresh — заглушка отзыва refresh-токена.
func (f *fakeStore) RevokeRefresh(_ context.Context, _ []byte) error { return nil }

func (f *fakeStore) CreateUser(_ context.Context, user core.User) (core.User, error) {
	if f.taken[user.Localpart] {
		return core.User{}, core.ErrUserInUse
	}

	f.taken[user.Localpart] = true
	f.users[user.Localpart] = user

	return user, nil
}

func (f *fakeStore) UserByLocalpart(_ context.Context, localpart string) (core.User, error) {
	user, ok := f.users[localpart]
	if !ok {
		return core.User{}, fmt.Errorf("user %s: %w", localpart, core.ErrNotFound)
	}

	return user, nil
}

func (f *fakeStore) UserByID(_ context.Context, userID string) (core.User, error) {
	for _, user := range f.users {
		if user.UserID == userID {
			return user, nil
		}
	}

	return core.User{}, fmt.Errorf("user %s: %w", userID, core.ErrNotFound)
}

func (f *fakeStore) UpsertDevice(_ context.Context, device core.Device) error {
	f.devices[device.UserID+"/"+device.DeviceID] = device

	return nil
}

func (f *fakeStore) CreateToken(_ context.Context, token core.TokenRecord) error {
	user := core.User{}
	for _, candidate := range f.users {
		if candidate.UserID == token.UserID {
			user = candidate
		}
	}

	f.tokens[hex.EncodeToString(token.TokenHash)] = core.Session{
		UserID:      token.UserID,
		Localpart:   user.Localpart,
		DisplayName: user.DisplayName,
		DeviceID:    token.DeviceID,
		Deactivated: user.Deactivated,
		ExpiresAt:   token.ExpiresAt,
	}

	return nil
}

func (f *fakeStore) SessionByTokenHash(_ context.Context, tokenHash []byte) (core.Session, error) {
	session, ok := f.tokens[hex.EncodeToString(tokenHash)]
	if !ok {
		return core.Session{}, fmt.Errorf("token: %w", core.ErrNotFound)
	}

	return session, nil
}

func (f *fakeStore) RevokeToken(_ context.Context, tokenHash []byte) error {
	key := hex.EncodeToString(tokenHash)
	if _, ok := f.tokens[key]; !ok {
		return core.ErrInvalidToken
	}

	delete(f.tokens, key)

	return nil
}

func (f *fakeStore) SearchUsers(_ context.Context, term string, limit int) ([]core.User, error) {
	var out []core.User

	for _, user := range f.users {
		if strings.Contains(strings.ToLower(user.Localpart), strings.ToLower(term)) ||
			strings.Contains(strings.ToLower(user.DisplayName), strings.ToLower(term)) {
			out = append(out, user)
		}
	}

	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

func (f *fakeStore) DeactivateUser(_ context.Context, userID string) error {
	for key, user := range f.users {
		if user.UserID != userID {
			continue
		}

		user.Deactivated = true
		f.users[key] = user

		return nil
	}

	return core.ErrNotFound
}

func (f *fakeStore) RevokeAllTokens(_ context.Context, userID string) (int64, error) {
	var count int64

	for key, session := range f.tokens {
		if session.UserID == userID {
			delete(f.tokens, key)

			count++
		}
	}

	return count, nil
}

// newAuth собирает сервис на фейках с открытой регистрацией.
func newAuth(t *testing.T, opts core.AuthOptions) (*core.Auth, *fakeStore) {
	t.Helper()

	store := newFakeStore()

	if opts.ServerName == "" {
		opts.ServerName = "test.local"
	}

	if opts.MinPasswordLength == 0 {
		opts.MinPasswordLength = 8
	}

	opts.RegistrationOpen = true

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return core.NewAuth(store, fakeHasher{}, &fakeTokens{}, opts), store
}

func TestValidateLocalpart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "простой", input: "alice", want: "alice"},
		{name: "верхний регистр приводится", input: "Alice", want: "alice"},
		{name: "цифры и спецсимволы", input: "user.name_1-2/3=4+5", want: "user.name_1-2/3=4+5"},
		{name: "пробелы по краям", input: "  bob  ", want: "bob"},
		{name: "пустой", input: "", wantErr: true},
		{name: "пробел внутри", input: "bad name", wantErr: true},
		{name: "полный user id", input: "@alice:test.local", wantErr: true},
		{name: "кириллица", input: "алиса", wantErr: true},
		{name: "двоеточие", input: "alice:test", wantErr: true},
		{name: "слишком длинный", input: strings.Repeat("a", core.LocalpartMaxLength+1), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := core.ValidateLocalpart(tc.input)
			if tc.wantErr {
				if !errors.Is(err, core.ErrInvalidLocalpart) {
					t.Fatalf("ValidateLocalpart(%q) = %v, ожидали ErrInvalidLocalpart", tc.input, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ValidateLocalpart(%q) error: %v", tc.input, err)
			}

			if got != tc.want {
				t.Fatalf("ValidateLocalpart(%q) = %q, ожидали %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRegisterIssuesSession(t *testing.T) {
	t.Parallel()

	svc, store := newAuth(t, core.AuthOptions{})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "alice",
		Password:  "correct horse",
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if result.UserID != "@alice:test.local" {
		t.Fatalf("UserID = %q", result.UserID)
	}

	if result.AccessToken == "" {
		t.Fatal("AccessToken пустой")
	}

	if len(result.DeviceID) != 10 {
		t.Fatalf("device_id = %q, ожидали 10 символов", result.DeviceID)
	}

	if _, ok := store.devices["@alice:test.local/"+result.DeviceID]; !ok {
		t.Fatal("устройство не сохранено")
	}

	session, err := svc.WhoAmI(context.Background(), result.AccessToken)
	if err != nil {
		t.Fatalf("WhoAmI() error: %v", err)
	}

	if session.UserID != result.UserID || session.DeviceID != result.DeviceID {
		t.Fatalf("сессия = %+v", session)
	}
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{MinPasswordLength: 10})

	cases := []struct {
		name string
		req  core.RegisterRequest
		want error
	}{
		{name: "плохой localpart", req: core.RegisterRequest{Localpart: "bad name", Password: "1234567890"}, want: core.ErrInvalidLocalpart},
		{name: "короткий пароль", req: core.RegisterRequest{Localpart: "bob", Password: "short"}, want: core.ErrWeakPassword},
		{name: "занятый localpart", req: core.RegisterRequest{Localpart: "alice", Password: "1234567890"}, want: core.ErrUserInUse},
	}

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "alice", Password: "1234567890",
	}); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := svc.Register(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Register() = %v, ожидали %v", err, tc.want)
			}
		})
	}
}

func TestRegisterDisabled(t *testing.T) {
	t.Parallel()

	svc := core.NewAuth(newFakeStore(), fakeHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName:       "test.local",
		RegistrationOpen: false,
	})

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "alice", Password: "1234567890",
	}); !errors.Is(err, core.ErrRegistrationDisabled) {
		t.Fatalf("Register() = %v, ожидали ErrRegistrationDisabled", err)
	}
}

func TestRegisterInhibitLogin(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "carol", Password: "1234567890", InhibitLogin: true,
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if result.UserID != "@carol:test.local" {
		t.Fatalf("UserID = %q", result.UserID)
	}

	if result.AccessToken != "" || result.DeviceID != "" {
		t.Fatalf("при inhibit_login не должно быть сессии: %+v", result)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "dave", Password: "1234567890", InhibitLogin: true,
	}); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	// Логин по localpart.
	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "dave", Password: "1234567890",
	}); err != nil {
		t.Fatalf("Login(localpart) error: %v", err)
	}

	// Логин по полному user_id с явным device_id.
	result, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "@dave:test.local", Password: "1234567890", DeviceID: "MYDEVICE",
	})
	if err != nil {
		t.Fatalf("Login(user_id) error: %v", err)
	}

	if result.DeviceID != "MYDEVICE" {
		t.Fatalf("device_id = %q, ожидали MYDEVICE", result.DeviceID)
	}

	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "dave", Password: "wrong",
	}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login(wrong password) = %v, ожидали ErrInvalidCredentials", err)
	}

	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "nobody", Password: "1234567890",
	}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login(unknown) = %v, ожидали ErrInvalidCredentials", err)
	}

	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "@dave:other.local", Password: "1234567890",
	}); !errors.Is(err, core.ErrInvalidCredentials) {
		t.Fatalf("Login(чужой сервер) = %v, ожидали ErrInvalidCredentials", err)
	}
}

func TestLoginDeactivated(t *testing.T) {
	t.Parallel()

	svc, store := newAuth(t, core.AuthOptions{})

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "erin", Password: "1234567890", InhibitLogin: true,
	}); err != nil {
		t.Fatalf("подготовка: %v", err)
	}

	user := store.users["erin"]
	user.Deactivated = true
	store.users["erin"] = user

	if _, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "erin", Password: "1234567890",
	}); !errors.Is(err, core.ErrUserDeactivated) {
		t.Fatalf("Login() = %v, ожидали ErrUserDeactivated", err)
	}
}

func TestRefreshTokens(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{RefreshTokens: true})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "frank", Password: "1234567890", RequestRefreshToken: true,
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if result.RefreshToken == "" {
		t.Fatal("refresh_token не выдан при включённой ротации")
	}

	svcOff, _ := newAuth(t, core.AuthOptions{RefreshTokens: false})

	off, err := svcOff.Register(context.Background(), core.RegisterRequest{
		Localpart: "grace", Password: "1234567890", RequestRefreshToken: true,
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if off.RefreshToken != "" {
		t.Fatal("refresh_token выдан при выключенной ротации")
	}
}

func TestAccessTokenTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	svc, _ := newAuth(t, core.AuthOptions{
		AccessTokenTTL: time.Minute,
		Now:            func() time.Time { return now },
	})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "heidi", Password: "1234567890",
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if result.ExpiresInMS == nil || *result.ExpiresInMS != 60_000 {
		t.Fatalf("expires_in_ms = %v, ожидали 60000", result.ExpiresInMS)
	}

	// Сдвигаем время за пределы TTL: токен должен стать недействительным.
	svcExpired := core.NewAuth(newFakeStore(), fakeHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName:        "test.local",
		RegistrationOpen:  true,
		MinPasswordLength: 8,
		AccessTokenTTL:    time.Minute,
		Now:               func() time.Time { return now.Add(2 * time.Minute) },
	})

	if _, err := svcExpired.WhoAmI(context.Background(), result.AccessToken); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("WhoAmI(просроченный) = %v, ожидали ErrInvalidToken", err)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "ivan", Password: "1234567890",
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	if err := svc.Logout(context.Background(), result.AccessToken); err != nil {
		t.Fatalf("Logout() error: %v", err)
	}

	if _, err := svc.WhoAmI(context.Background(), result.AccessToken); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("WhoAmI(после logout) = %v, ожидали ErrInvalidToken", err)
	}

	if err := svc.Logout(context.Background(), result.AccessToken); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("повторный Logout() = %v, ожидали ErrInvalidToken", err)
	}
}

func TestLogoutAll(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	first, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "judy", Password: "1234567890",
	})
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	second, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "judy", Password: "1234567890", DeviceID: "SECOND",
	})
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}

	count, err := svc.LogoutAll(context.Background(), first.UserID)
	if err != nil {
		t.Fatalf("LogoutAll() error: %v", err)
	}

	if count != 2 {
		t.Fatalf("LogoutAll() = %d, ожидали 2 токена", count)
	}

	for _, token := range []string{first.AccessToken, second.AccessToken} {
		if _, err := svc.WhoAmI(context.Background(), token); !errors.Is(err, core.ErrInvalidToken) {
			t.Fatalf("WhoAmI(%s) = %v, ожидали ErrInvalidToken", token, err)
		}
	}
}

func TestWhoAmIEmptyToken(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	if _, err := svc.WhoAmI(context.Background(), ""); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("WhoAmI(\"\") = %v, ожидали ErrInvalidToken", err)
	}
}

// rehashHasher — хешер с форматом "bcrypt:<pass>", который требует миграции.
type rehashHasher struct{}

func (rehashHasher) Hash(password string) (string, error) { return "argon:" + password, nil }

func (rehashHasher) Verify(hash, password string) (bool, error) {
	return hash == "bcrypt:"+password || hash == "argon:"+password, nil
}

func (rehashHasher) NeedsRehash(hash string) bool { return strings.HasPrefix(hash, "bcrypt:") }

// TestLoginRehashesLegacyPassword — пароль, импортированный из Synapse (bcrypt),
// проверяется и при входе пере-хешируется в современный формат.
func TestLoginRehashesLegacyPassword(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	auth := core.NewAuth(store, rehashHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName: "test.local",
	})

	legacy := core.User{
		UserID:       "@legacy:test.local",
		Localpart:    "legacy",
		PasswordHash: "bcrypt:old-pass",
		DisplayName:  "Legacy",
	}

	if _, err := store.CreateUser(context.Background(), legacy); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := auth.Login(context.Background(), core.LoginRequest{
		Identifier: "legacy",
		Password:   "old-pass",
	}); err != nil {
		t.Fatalf("login (legacy bcrypt): %v", err)
	}

	stored, err := store.UserByLocalpart(context.Background(), "legacy")
	if err != nil {
		t.Fatalf("read user: %v", err)
	}

	if stored.PasswordHash != "argon:old-pass" {
		t.Fatalf("хеш не пере-хеширован: %q", stored.PasswordHash)
	}

	// Повторный вход уже по argon-хешу.
	if _, err := auth.Login(context.Background(), core.LoginRequest{
		Identifier: "legacy",
		Password:   "old-pass",
	}); err != nil {
		t.Fatalf("второй login: %v", err)
	}
}

// TestLoginDoesNotRehashArgon — argon2id-пароли не трогаем, ошибки rehash не ломают вход.
func TestLoginDoesNotRehashArgon(t *testing.T) {
	t.Parallel()

	store := newFakeStore()

	var rehashed int

	auth := core.NewAuth(store, rehashHasher{}, &fakeTokens{}, core.AuthOptions{
		ServerName: "test.local",
		OnPasswordRehash: func(string, error) {
			rehashed++
		},
	})

	user := core.User{
		UserID:       "@modern:test.local",
		Localpart:    "modern",
		PasswordHash: "argon:modern-pass",
	}

	if _, err := store.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := auth.Login(context.Background(), core.LoginRequest{
		Identifier: "modern",
		Password:   "modern-pass",
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	if rehashed != 0 {
		t.Fatalf("argon2id-хеш не должен пере-хешироваться (вызовов: %d)", rehashed)
	}
}

// TestLoginUppercaseUsername — вход с @ALICE:SERVER работает (регистр не важен).
func TestLoginUppercaseUsername(t *testing.T) {
	t.Parallel()

	svc, _ := newAuth(t, core.AuthOptions{})

	if _, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "alice", Password: "password-1",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	result, err := svc.Login(context.Background(), core.LoginRequest{
		Identifier: "@ALICE:TEST.LOCAL", Password: "password-1", DeviceID: "DEV",
	})
	if err != nil {
		t.Fatalf("login с верхним регистром: %v", err)
	}

	if result.UserID != "@alice:test.local" {
		t.Fatalf("user_id = %q", result.UserID)
	}
}

// TestRefreshTokenExpires — refresh-токен старше auth.refresh_token_ttl не
// обменивается (раньше TTL не применялся).
func TestRefreshTokenExpires(t *testing.T) {
	t.Parallel()

	svc, store := newAuth(t, core.AuthOptions{RefreshTokens: true, RefreshTokenTTL: time.Hour})

	result, err := svc.Register(context.Background(), core.RegisterRequest{
		Localpart: "henry", Password: "1234567890", RequestRefreshToken: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	for key, session := range store.tokens {
		session.IssuedAt = time.Now().Add(-2 * time.Hour)
		store.tokens[key] = session
	}

	if _, err := svc.Refresh(context.Background(), result.RefreshToken); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("просроченный refresh: %v, ожидали ErrInvalidToken", err)
	}
}

// UserByID и UserByLogin (admin API): логин с @ - user_id, иначе
// localpart; неизвестный - ErrNotFound хранилища.
func TestAuthUserLookup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newAuth(t, core.AuthOptions{})

	if _, err := svc.Register(ctx, core.RegisterRequest{Localpart: "alice", Password: "correct horse"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	byID, err := svc.UserByID(ctx, "@alice:test.local")
	if err != nil || byID.Localpart != "alice" {
		t.Fatalf("UserByID: %+v %v", byID, err)
	}

	for _, login := range []string{"@alice:test.local", "alice"} {
		user, err := svc.UserByLogin(ctx, login)
		if err != nil || user.UserID != "@alice:test.local" {
			t.Fatalf("UserByLogin(%q): %+v %v", login, user, err)
		}
	}

	for _, login := range []string{"", "bob", "@bob:test.local"} {
		if _, err := svc.UserByLogin(ctx, login); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("UserByLogin(%q): %v, ожидали ErrNotFound", login, err)
		}
	}
}
