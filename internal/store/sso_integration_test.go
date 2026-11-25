//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// TestSSOStore - привязки user_external_ids и одноразовые loginToken
// (миграция 0037).
func TestSSOStore(t *testing.T) {
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

	db := openDB(t)
	ctx := context.Background()
	authStore := store.NewAuthStore(db)
	sso := store.NewSSOStore(db)

	if _, err := authStore.CreateUser(ctx, core.User{UserID: "@ivan:test.local", Localpart: "ivan"}); err != nil {
		t.Fatal(err)
	}

	if _, err := sso.UserIDByExternalID(ctx, "oidc-kc", "sub-1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("нет привязки: %v", err)
	}

	if err := sso.AddExternalID(ctx, "oidc-kc", "sub-1", "@ivan:test.local"); err != nil {
		t.Fatal(err)
	}

	if err := sso.AddExternalID(ctx, "oidc-gitlab", "42", "@ivan:test.local"); err != nil {
		t.Fatal(err)
	}

	if err := sso.AddExternalID(ctx, "oidc-kc", "sub-1", "@ivan:test.local"); !errors.Is(err, core.ErrExternalIDInUse) {
		t.Fatalf("повтор привязки: %v", err)
	}

	if err := sso.AddExternalID(ctx, "oidc-kc", "sub-2", "@ghost:test.local"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("привязка к несуществующей учетке: %v", err)
	}

	if id, err := sso.UserIDByExternalID(ctx, "oidc-kc", "sub-1"); err != nil || id != "@ivan:test.local" {
		t.Fatalf("UserIDByExternalID: %q %v", id, err)
	}

	providers, err := sso.ExternalProviders(ctx, "@ivan:test.local")
	if err != nil || len(providers) != 2 || providers[0] != "oidc-gitlab" || providers[1] != "oidc-kc" {
		t.Fatalf("ExternalProviders: %v %v", providers, err)
	}

	// loginToken: один обмен из нескольких параллельных, истекший не гасится.
	now := time.Now()
	hash := core.HashToken("syl_test")

	if err := sso.CreateLoginToken(ctx, core.LoginToken{
		TokenHash: hash, UserID: "@ivan:test.local", AuthProvider: "oidc-kc", ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	for range 8 {
		wg.Go(func() {
			token, err := sso.ConsumeLoginToken(ctx, hash, now)
			if err == nil && token.UserID == "@ivan:test.local" && token.AuthProvider == "oidc-kc" {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, core.ErrNotFound) {
				t.Errorf("ConsumeLoginToken: %v", err)
			}
		})
	}

	wg.Wait()

	if wins != 1 {
		t.Fatalf("токен погашен %d раз, ожидали 1", wins)
	}

	expired := core.HashToken("syl_expired")
	if err := sso.CreateLoginToken(ctx, core.LoginToken{
		TokenHash: expired, UserID: "@ivan:test.local", ExpiresAt: now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := sso.ConsumeLoginToken(ctx, expired, now); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("истекший токен: %v", err)
	}

	// Давно истекшие удаляются при выдаче следующего.
	old := core.HashToken("syl_old")
	if _, err := db.Pool().Exec(ctx, `INSERT INTO login_tokens (token_hash, user_id, expires_at)
		VALUES ($1, '@ivan:test.local', now() - interval '2 hours')`, old); err != nil {
		t.Fatal(err)
	}

	if err := sso.CreateLoginToken(ctx, core.LoginToken{
		TokenHash: core.HashToken("syl_next"), UserID: "@ivan:test.local", ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	var left int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM login_tokens WHERE token_hash = $1`, old).Scan(&left); err != nil || left != 0 {
		t.Fatalf("старый токен не удален: %d %v", left, err)
	}
}
