//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/media"
	"github.com/aisa-it/syngo/internal/store"
)

// TestMediaStoreRoundTrip — метаданные медиа на живой БД (миграция 0002).
func TestMediaStoreRoundTrip(t *testing.T) {
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
	repo := store.NewMediaStore(db)
	ctx := context.Background()

	meta := media.Meta{
		ContentURI:  "mxc://test.local/abc123",
		MediaID:     "abc123",
		ServerName:  "test.local",
		Uploader:    "@alice:test.local",
		ContentType: "text/plain",
		SizeBytes:   11,
		SHA256:      sha256.New().Sum(nil),
		FileName:    "note.txt",
		StorageKey:  "test.local/abc123",
	}

	if err := repo.Insert(ctx, meta); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.ByID(ctx, "test.local", "abc123")
	if err != nil {
		t.Fatalf("by id: %v", err)
	}

	if got.ContentURI != meta.ContentURI || got.ContentType != meta.ContentType || got.SizeBytes != meta.SizeBytes {
		t.Fatalf("метаданные не совпали: %+v", got)
	}

	if got.FileName != "note.txt" {
		t.Fatalf("file_name = %q", got.FileName)
	}

	if err := repo.TouchAccess(ctx, meta.ContentURI); err != nil {
		t.Fatalf("touch: %v", err)
	}

	if _, err := repo.ByID(ctx, "test.local", "missing"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("ожидали ErrNotFound, получили %v", err)
	}

	// Чужой сервер — тоже «нет такого медиа».
	if _, err := repo.ByID(ctx, "other.host", "abc123"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("чужой сервер: ожидали ErrNotFound, получили %v", err)
	}
}

// TestMediaUpsertImported — импорт обновляет sha256/storage_key у уже существующей записи.
func TestMediaUpsertImported(t *testing.T) {
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
	repo := store.NewMediaStore(db)
	ctx := context.Background()

	sum := sha256.Sum256([]byte("x"))

	// Сначала запись «как из PoC»: без sha256.
	if err := repo.Insert(ctx, media.Meta{
		ContentURI: "mxc://test.local/imp1", MediaID: "imp1", ServerName: "test.local",
		Uploader: "@a:test.local", ContentType: "image/png", SizeBytes: 0,
		StorageKey: "local_content/im/p1/mp1",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := repo.UpsertImported(ctx, media.Meta{
		ContentURI: "mxc://test.local/imp1", MediaID: "imp1", ServerName: "test.local",
		Uploader: "@a:test.local", ContentType: "image/png", SizeBytes: 11,
		SHA256: sum[:], FileName: "p.png", StorageKey: "local_content/im/p1/mp1",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := repo.ByID(ctx, "test.local", "imp1")
	if err != nil {
		t.Fatalf("by id: %v", err)
	}

	if got.SizeBytes != 11 || len(got.SHA256) == 0 || got.FileName != "p.png" {
		t.Fatalf("после upsert: %+v", got)
	}

	// Повторный upsert не создаёт дублей.
	if err := repo.UpsertImported(ctx, media.Meta{
		ContentURI: "mxc://test.local/imp1", MediaID: "imp1", ServerName: "test.local",
		ContentType: "image/png", SizeBytes: 11, SHA256: sum[:], StorageKey: "local_content/im/p1/mp1",
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	var count int
	if err := db.Pool().QueryRow(ctx, "SELECT count(*) FROM media WHERE media_id = 'imp1'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}

	if count != 1 {
		t.Fatalf("записей %d, ожидали 1", count)
	}
}

// freshMediaStore поднимает схему с нуля и отдает хранилище метаданных медиа.
func freshMediaStore(t *testing.T) *store.MediaStore {
	t.Helper()

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

	return store.NewMediaStore(openDB(t))
}

// TestMediaStoreUpdateUploadedOnce - UPDATE без условия
// uploaded = false позволял второму PUT перезаписать метаданные первого.
func TestMediaStoreUpdateUploadedOnce(t *testing.T) {
	repo := freshMediaStore(t)
	ctx := context.Background()

	pending := media.Meta{
		ContentURI: "mxc://test.local/pend1",
		MediaID:    "pend1",
		ServerName: "test.local",
		Uploader:   "@alice:test.local",
		CreatedAt:  time.Now().UTC(),
	}

	if err := repo.InsertPending(ctx, pending); err != nil {
		t.Fatalf("insert pending: %v", err)
	}

	first := pending
	first.ContentType = "text/plain"
	first.SizeBytes = 5
	first.StorageKey = "test.local/pend1-a"
	first.Uploaded = true

	if err := repo.UpdateUploaded(ctx, first); err != nil {
		t.Fatalf("первый update: %v", err)
	}

	second := first
	second.ContentType = "image/png"
	second.StorageKey = "test.local/pend1-b"

	if err := repo.UpdateUploaded(ctx, second); !errors.Is(err, media.ErrAlreadyUploaded) {
		t.Fatalf("второй update: ожидали ErrAlreadyUploaded, получили %v", err)
	}

	got, err := repo.ByID(ctx, "test.local", "pend1")
	if err != nil {
		t.Fatalf("by id: %v", err)
	}

	if got.StorageKey != first.StorageKey || got.ContentType != first.ContentType {
		t.Fatalf("метаданные первого PUT перезаписаны: %+v", got)
	}

	missing := first
	missing.MediaID = "nope"

	if err := repo.UpdateUploaded(ctx, missing); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("несуществующий id: ожидали ErrNotFound, получили %v", err)
	}
}

// TestMediaStorePendingStats - предел и очистка незаполненных placeholder.
func TestMediaStorePendingStats(t *testing.T) {
	repo := freshMediaStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i, age := range []time.Duration{0, time.Hour, 30 * time.Hour} {
		id := fmt.Sprintf("p%d", i)
		meta := media.Meta{
			ContentURI: "mxc://test.local/" + id,
			MediaID:    id,
			ServerName: "test.local",
			Uploader:   "@alice:test.local",
			CreatedAt:  now.Add(-age),
		}

		if err := repo.InsertPending(ctx, meta); err != nil {
			t.Fatalf("insert pending %s: %v", id, err)
		}
	}

	// Повтор id - ошибка, а не молчаливый DO NOTHING.
	if err := repo.InsertPending(ctx, media.Meta{
		ContentURI: "mxc://test.local/p0", MediaID: "p0", ServerName: "test.local",
		Uploader: "@bob:test.local", CreatedAt: now,
	}); err == nil {
		t.Fatal("повторный placeholder должен давать ошибку")
	}

	since := now.Add(-24 * time.Hour)

	count, oldest, err := repo.PendingStats(ctx, "@alice:test.local", since)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if count != 2 || !oldest.Equal(now.Add(-time.Hour)) {
		t.Fatalf("stats: count=%d oldest=%v", count, oldest)
	}

	if count, _, err := repo.PendingStats(ctx, "@bob:test.local", since); err != nil || count != 0 {
		t.Fatalf("stats bob: %d %v", count, err)
	}

	if err := repo.DeleteExpiredPending(ctx, "@alice:test.local", since); err != nil {
		t.Fatalf("delete expired: %v", err)
	}

	if _, err := repo.ByID(ctx, "test.local", "p2"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("просроченный placeholder не удален: %v", err)
	}

	if _, err := repo.ByID(ctx, "test.local", "p1"); err != nil {
		t.Fatalf("живой placeholder удален: %v", err)
	}
}
