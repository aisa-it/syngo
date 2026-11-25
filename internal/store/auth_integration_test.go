//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// freshAuthStore поднимает схему с нуля и возвращает хранилище аутентификации.
func freshAuthStore(t *testing.T) *store.AuthStore {
	t.Helper()

	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	return store.NewAuthStore(openDB(t))
}

// TestAuthStoreUserLifecycle — пользователь, устройство и уникальность localpart.
func TestAuthStoreUserLifecycle(t *testing.T) {
	authStore := freshAuthStore(t)
	ctx := context.Background()

	created, err := authStore.CreateUser(ctx, core.User{
		UserID:       "@alice:test.local",
		Localpart:    "alice",
		PasswordHash: "$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$a2V5",
		DisplayName:  "Alice",
	})
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}

	if created.UserID != "@alice:test.local" || created.DisplayName != "Alice" {
		t.Fatalf("CreateUser() = %+v", created)
	}

	if _, err := authStore.CreateUser(ctx, core.User{
		UserID: "@alice2:test.local", Localpart: "alice", PasswordHash: "x",
	}); !errors.Is(err, core.ErrUserInUse) {
		t.Fatalf("CreateUser(дубликат) = %v, ожидали ErrUserInUse", err)
	}

	byLocalpart, err := authStore.UserByLocalpart(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByLocalpart() error: %v", err)
	}

	if byLocalpart.UserID != "@alice:test.local" || byLocalpart.PasswordHash != created.PasswordHash {
		t.Fatalf("UserByLocalpart() = %+v", byLocalpart)
	}

	byID, err := authStore.UserByID(ctx, "@alice:test.local")
	if err != nil {
		t.Fatalf("UserByID() error: %v", err)
	}

	if byID.Localpart != "alice" {
		t.Fatalf("UserByID() = %+v", byID)
	}

	if _, err := authStore.UserByLocalpart(ctx, "ghost"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("UserByLocalpart(ghost) = %v, ожидали ErrNotFound", err)
	}

	// Устройство создаётся и обновляется по ключу (user_id, device_id).
	if err := authStore.UpsertDevice(ctx, core.Device{
		UserID: "@alice:test.local", DeviceID: "DEV1", DisplayName: "phone",
	}); err != nil {
		t.Fatalf("UpsertDevice() error: %v", err)
	}

	if err := authStore.UpsertDevice(ctx, core.Device{
		UserID: "@alice:test.local", DeviceID: "DEV1", DisplayName: "tablet",
	}); err != nil {
		t.Fatalf("UpsertDevice(повтор) error: %v", err)
	}

	var devices int

	db := openDB(t)
	if err := db.Pool().QueryRow(ctx,
		"SELECT count(*) FROM devices WHERE user_id = $1", "@alice:test.local").Scan(&devices); err != nil {
		t.Fatalf("count devices: %v", err)
	}

	if devices != 1 {
		t.Fatalf("devices = %d, ожидали 1 (upsert, не insert)", devices)
	}
}

// TestAuthStoreTokens — выдача, проверка и отзыв токенов.
func TestAuthStoreTokens(t *testing.T) {
	authStore := freshAuthStore(t)
	ctx := context.Background()

	if _, err := authStore.CreateUser(ctx, core.User{
		UserID: "@bob:test.local", Localpart: "bob", PasswordHash: "x", DisplayName: "Bob",
	}); err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}

	if err := authStore.UpsertDevice(ctx, core.Device{
		UserID: "@bob:test.local", DeviceID: "DEV1", DisplayName: "laptop",
	}); err != nil {
		t.Fatalf("UpsertDevice() error: %v", err)
	}

	access := core.HashToken("syt_access")
	if err := authStore.CreateToken(ctx, core.TokenRecord{
		TokenHash: access, UserID: "@bob:test.local", DeviceID: "DEV1",
	}); err != nil {
		t.Fatalf("CreateToken() error: %v", err)
	}

	session, err := authStore.SessionByTokenHash(ctx, access)
	if err != nil {
		t.Fatalf("SessionByTokenHash() error: %v", err)
	}

	if session.UserID != "@bob:test.local" || session.DeviceID != "DEV1" || session.ExpiresAt != nil {
		t.Fatalf("SessionByTokenHash() = %+v", session)
	}

	// Просроченный токен не находится.
	past := time.Now().Add(-time.Minute)
	if err := authStore.CreateToken(ctx, core.TokenRecord{
		TokenHash: core.HashToken("syt_expired"), UserID: "@bob:test.local", DeviceID: "DEV1", ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("CreateToken(expired) error: %v", err)
	}

	if _, err := authStore.SessionByTokenHash(ctx, core.HashToken("syt_expired")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("SessionByTokenHash(просроченный) = %v, ожидали ErrNotFound", err)
	}

	// Отзыв одного токена идемпотентен по смыслу, но повтор даёт ErrInvalidToken.
	if err := authStore.RevokeToken(ctx, access); err != nil {
		t.Fatalf("RevokeToken() error: %v", err)
	}

	if _, err := authStore.SessionByTokenHash(ctx, access); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("SessionByTokenHash(отозванный) = %v, ожидали ErrNotFound", err)
	}

	if err := authStore.RevokeToken(ctx, access); !errors.Is(err, core.ErrInvalidToken) {
		t.Fatalf("повторный RevokeToken() = %v, ожидали ErrInvalidToken", err)
	}

	// Второй активный токен того же пользователя отзывается через RevokeAllTokens.
	second := core.HashToken("syt_second")
	if err := authStore.CreateToken(ctx, core.TokenRecord{
		TokenHash: second, UserID: "@bob:test.local", DeviceID: "DEV1",
	}); err != nil {
		t.Fatalf("CreateToken(second) error: %v", err)
	}

	count, err := authStore.RevokeAllTokens(ctx, "@bob:test.local")
	if err != nil {
		t.Fatalf("RevokeAllTokens() error: %v", err)
	}

	// Отзываются все неотозванные токены, включая просроченный.
	if count != 2 {
		t.Fatalf("RevokeAllTokens() = %d, ожидали 2 неотозванных токена", count)
	}

	if _, err := authStore.SessionByTokenHash(ctx, second); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("SessionByTokenHash(после revoke all) = %v, ожидали ErrNotFound", err)
	}
}

// TestPasswordHashMigrationStore — пере-хеширование и счётчики форматов хешей.
func TestPasswordHashMigrationStore(t *testing.T) {
	m := newMigrator(t)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := m.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = m.Down()
		_, _ = m.Close()
	})

	db := openDB(t)
	repo := store.NewAuthStore(db)
	ctx := context.Background()

	legacy := core.User{
		UserID:       "@legacy:test.local",
		Localpart:    "legacy",
		PasswordHash: "$2b$12$abcdefghijklmnopqrstuv",
	}
	modern := core.User{
		UserID:       "@modern:test.local",
		Localpart:    "modern",
		PasswordHash: "$argon2id$v=19$m=65536,t=2,p=1$c2FsdA$a2V5",
	}

	for _, u := range []core.User{legacy, modern} {
		if _, err := repo.CreateUser(ctx, u); err != nil {
			t.Fatalf("create %s: %v", u.UserID, err)
		}
	}

	argon, bcrypt, err := repo.CountPasswordHashes(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	if argon != 1 || bcrypt != 1 {
		t.Fatalf("счётчики: argon2id=%d bcrypt=%d, ожидали 1/1", argon, bcrypt)
	}

	if err := repo.UpdatePasswordHash(ctx, legacy.UserID, modern.PasswordHash); err != nil {
		t.Fatalf("update hash: %v", err)
	}

	argon, bcrypt, err = repo.CountPasswordHashes(ctx)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}

	if argon != 2 || bcrypt != 0 {
		t.Fatalf("после миграции: argon2id=%d bcrypt=%d, ожидали 2/0", argon, bcrypt)
	}

	if err := repo.UpdatePasswordHash(ctx, "@missing:test.local", "x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("неизвестный пользователь: ожидали ErrNotFound, получили %v", err)
	}
}

// TestAuthStoreCreateAdmin - регистрация через общий секрет с admin=true
// создает пользователя сразу с ролью администратора.
func TestAuthStoreCreateAdmin(t *testing.T) {
	authStore := freshAuthStore(t)
	ctx := context.Background()

	created, err := authStore.CreateUser(ctx, core.User{
		UserID: "@root:test.local", Localpart: "root", PasswordHash: "x", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}

	if !created.IsAdmin {
		t.Fatal("CreateUser(IsAdmin=true) вернул пользователя без роли администратора")
	}

	plain, err := authStore.CreateUser(ctx, core.User{
		UserID: "@user:test.local", Localpart: "user", PasswordHash: "x",
	})
	if err != nil || plain.IsAdmin {
		t.Fatalf("CreateUser() = %+v, %v; ожидали обычного пользователя", plain, err)
	}
}

// TestUserByLocalpartCaseInsensitive - поиск при входе регистронезависим, как в
// Synapse: одно совпадение - оно, несколько - только точное.
func TestUserByLocalpartCaseInsensitive(t *testing.T) {
	authStore := freshAuthStore(t)
	ctx := context.Background()

	for _, localpart := range []string{"Legacy", "Dup", "dup"} {
		if _, err := authStore.CreateUser(ctx, core.User{
			UserID: "@" + localpart + ":test.local", Localpart: localpart, PasswordHash: "x",
		}); err != nil {
			t.Fatalf("CreateUser(%s): %v", localpart, err)
		}
	}

	cases := map[string]string{"legacy": "Legacy", "LEGACY": "Legacy", "dup": "dup", "Dup": "Dup", "DUP": ""}

	for input, want := range cases {
		user, err := authStore.UserByLocalpart(ctx, input)

		switch {
		case want == "" && !errors.Is(err, core.ErrNotFound):
			t.Errorf("UserByLocalpart(%s) = %+v, %v; ожидали ErrNotFound (несколько неточных совпадений)", input, user, err)
		case want != "" && (err != nil || user.Localpart != want):
			t.Errorf("UserByLocalpart(%s) = %q, %v; ожидали %q", input, user.Localpart, err, want)
		}
	}
}
