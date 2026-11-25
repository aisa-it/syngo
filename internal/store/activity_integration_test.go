//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
)

// freshActivityDB поднимает схему с нуля и включает отложенную запись отметок
// активности без фонового сброса: сброс в тестах явный.
func freshActivityDB(t *testing.T) (*store.DB, *store.Activity) {
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

	db := openDB(t)

	return db, db.EnableActivity(store.ActivityOptions{FlushInterval: time.Hour, TokenInterval: time.Minute})
}

// seedToken создает пользователя, устройство и токен с заданной меткой проверки.
func seedToken(t *testing.T, db *store.DB, authStore *store.AuthStore, token string, lastValidated *time.Time) []byte {
	t.Helper()

	ctx := context.Background()

	if _, err := authStore.UserByID(ctx, "@bob:test.local"); err != nil {
		if _, err := authStore.CreateUser(ctx, core.User{UserID: "@bob:test.local", Localpart: "bob", PasswordHash: "x"}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		if err := authStore.UpsertDevice(ctx, core.Device{UserID: "@bob:test.local", DeviceID: "DEV1"}); err != nil {
			t.Fatalf("UpsertDevice: %v", err)
		}
	}

	hash := core.HashToken(token)
	if err := authStore.CreateToken(ctx, core.TokenRecord{TokenHash: hash, UserID: "@bob:test.local", DeviceID: "DEV1"}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE access_tokens SET last_validated_at = $2 WHERE token_hash = $1`,
		hash, lastValidated); err != nil {
		t.Fatalf("set last_validated_at: %v", err)
	}

	return hash
}

// lastValidatedAt читает метку токена.
func lastValidatedAt(t *testing.T, db *store.DB, hash []byte) *time.Time {
	t.Helper()

	var at *time.Time
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT last_validated_at FROM access_tokens WHERE token_hash = $1`, hash).Scan(&at); err != nil {
		t.Fatalf("select last_validated_at: %v", err)
	}

	return at
}

// tokenXmin - xmin строки токена: любой UPDATE строки его меняет.
func tokenXmin(t *testing.T, db *store.DB, hash []byte) string {
	t.Helper()

	var xmin string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT xmin::text FROM access_tokens WHERE token_hash = $1`, hash).Scan(&xmin); err != nil {
		t.Fatalf("select xmin: %v", err)
	}

	return xmin
}

// TestSessionByTokenHashDoesNotWrite - проверка токена с устаревшей
// меткой не пишет в БД; метка уходит одним пакетным UPDATE при сбросе.
func TestSessionByTokenHashDoesNotWrite(t *testing.T) {
	db, activity := freshActivityDB(t)
	authStore := store.NewAuthStore(db)
	ctx := context.Background()

	old := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	stale := seedToken(t, db, authStore, "syt_stale", &old)
	never := seedToken(t, db, authStore, "syt_never", nil)
	fresh := time.Now().Add(-10 * time.Second).Truncate(time.Microsecond)
	recent := seedToken(t, db, authStore, "syt_recent", &fresh)

	xminBefore := tokenXmin(t, db, stale)
	before := time.Now()

	for range 20 {
		for _, hash := range [][]byte{stale, never, recent} {
			if _, err := authStore.SessionByTokenHash(ctx, hash); err != nil {
				t.Fatalf("SessionByTokenHash: %v", err)
			}
		}
	}

	if got := tokenXmin(t, db, stale); got != xminBefore {
		t.Fatalf("путь запроса обновил строку токена (xmin %s -> %s)", xminBefore, got)
	}

	if got := lastValidatedAt(t, db, stale); got == nil || !got.Equal(old) {
		t.Fatalf("до сброса last_validated_at = %v, ожидали прежнее %v", got, old)
	}

	// Свежая метка (моложе интервала) в очередь не попадает.
	if st := activity.Stats(); st.PendingTokens != 2 || st.Statements != 0 {
		t.Fatalf("накоплено %+v, ожидали 2 токена и ни одной инструкции", st)
	}

	if err := activity.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if st := activity.Stats(); st.Statements != 1 || st.PendingTokens != 0 {
		t.Fatalf("после сброса %+v, ожидали одну инструкцию", st)
	}

	for _, hash := range [][]byte{stale, never} {
		got := lastValidatedAt(t, db, hash)
		if got == nil || got.Before(before.Add(-time.Second)) {
			t.Fatalf("после сброса last_validated_at = %v, ожидали время запроса (>= %v)", got, before)
		}
	}

	if got := lastValidatedAt(t, db, recent); got == nil || !got.Equal(fresh) {
		t.Fatalf("свежая метка изменилась: %v, было %v", got, fresh)
	}

	// После сброса метка свежая - повторные проверки ничего не копят.
	if _, err := authStore.SessionByTokenHash(ctx, stale); err != nil {
		t.Fatalf("SessionByTokenHash: %v", err)
	}

	if st := activity.Stats(); st.PendingTokens != 0 {
		t.Fatalf("после сброса снова накоплено %+v", st)
	}
}

// TestActivityFlushKeepsTokenSemantics - отложенная метка не воскрешает
// отозванный или просроченный токен и не откатывает более свежую метку.
func TestActivityFlushKeepsTokenSemantics(t *testing.T) {
	db, activity := freshActivityDB(t)
	authStore := store.NewAuthStore(db)
	ctx := context.Background()

	revoked := seedToken(t, db, authStore, "syt_revoked", nil)
	expiring := seedToken(t, db, authStore, "syt_expiring", nil)
	newer := seedToken(t, db, authStore, "syt_newer", nil)

	for _, hash := range [][]byte{revoked, expiring, newer} {
		if _, err := authStore.SessionByTokenHash(ctx, hash); err != nil {
			t.Fatalf("SessionByTokenHash: %v", err)
		}
	}

	if err := authStore.RevokeToken(ctx, revoked); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if _, err := db.Pool().Exec(ctx, `UPDATE access_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`,
		expiring); err != nil {
		t.Fatalf("expire: %v", err)
	}

	future := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := db.Pool().Exec(ctx, `UPDATE access_tokens SET last_validated_at = $2 WHERE token_hash = $1`,
		newer, future); err != nil {
		t.Fatalf("set newer: %v", err)
	}

	if err := activity.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for name, hash := range map[string][]byte{"отозванный": revoked, "просроченный": expiring} {
		if _, err := authStore.SessionByTokenHash(ctx, hash); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("%s токен после сброса: %v, ожидали ErrNotFound", name, err)
		}
	}

	if got := lastValidatedAt(t, db, newer); got == nil || !got.Equal(future) {
		t.Fatalf("сброс откатил метку: %v, ожидали %v", got, future)
	}
}

// TestMediaTouchBatched - скачивание медиа не пишет last_accessed_at в пути
// запроса; сброс пишет все медиа одной инструкцией, Close - накопленное.
func TestMediaTouchBatched(t *testing.T) {
	db, activity := freshActivityDB(t)
	repo := store.NewMediaStore(db)
	ctx := context.Background()

	uris := []string{"mxc://test.local/m1", "mxc://test.local/m2", "mxc://test.local/m3"}
	for i, uri := range uris {
		id := uri[len("mxc://test.local/"):]
		if err := repo.Insert(ctx, media.Meta{
			ContentURI: uri, MediaID: id, ServerName: "test.local", Uploader: "@a:test.local",
			ContentType: "text/plain", SizeBytes: int64(i), SHA256: sha256.New().Sum(nil), StorageKey: "k/" + id,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	accessed := func(uri string) *time.Time {
		var at *time.Time
		if err := db.Pool().QueryRow(ctx, `SELECT last_accessed_at FROM media WHERE content_uri = $1`, uri).Scan(&at); err != nil {
			t.Fatalf("select: %v", err)
		}

		return at
	}

	for range 10 {
		for _, uri := range uris[:2] {
			if err := repo.TouchAccess(ctx, uri); err != nil {
				t.Fatalf("touch: %v", err)
			}
		}
	}

	if got := accessed(uris[0]); got != nil {
		t.Fatalf("путь запроса записал last_accessed_at = %v", got)
	}

	if err := activity.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if st := activity.Stats(); st.Statements != 1 {
		t.Fatalf("инструкций %d, ожидали 1 на все медиа", st.Statements)
	}

	if accessed(uris[0]) == nil || accessed(uris[1]) == nil || accessed(uris[2]) != nil {
		t.Fatalf("после сброса: %v %v %v", accessed(uris[0]), accessed(uris[1]), accessed(uris[2]))
	}

	// Остановка пишет накопленное.
	if err := repo.TouchAccess(ctx, uris[2]); err != nil {
		t.Fatalf("touch: %v", err)
	}

	if err := activity.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	if accessed(uris[2]) == nil {
		t.Fatal("Close не записал накопленное")
	}
}
