package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/media"
)

// MediaStore — метаданные медиа (таблица media, миграция 0002). Байты — в media.ObjectStore.
type MediaStore struct {
	db *DB
}

var _ media.Repo = (*MediaStore)(nil)

// NewMediaStore создаёт хранилище метаданных медиа.
func NewMediaStore(db *DB) *MediaStore {
	return &MediaStore{db: db}
}

// Insert сохраняет метаданные медиа.
func (s *MediaStore) Insert(ctx context.Context, meta media.Meta) error {
	const query = `
		INSERT INTO media (content_uri, media_id, server_name, uploader, content_type,
		                   size_bytes, sha256, file_name, storage_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	_, err := s.db.pool.Exec(ctx, query,
		meta.ContentURI, meta.MediaID, meta.ServerName, meta.Uploader, meta.ContentType,
		meta.SizeBytes, meta.SHA256, nullString(meta.FileName), meta.StorageKey,
	)
	if err != nil {
		return fmt.Errorf("store: insert media: %w", err)
	}

	return nil
}

var _ media.RemoteRepo = (*MediaStore)(nil)

// InsertRemote сохраняет кеш медиа другого сервера (is_remote).
// Запись уже есть - медиа скачал параллельный запрос, не ошибка.
func (s *MediaStore) InsertRemote(ctx context.Context, meta media.Meta) error {
	const query = `
		INSERT INTO media (content_uri, media_id, server_name, content_type,
		                   size_bytes, sha256, file_name, storage_key, is_remote)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
		ON CONFLICT DO NOTHING`

	if _, err := s.db.pool.Exec(ctx, query,
		meta.ContentURI, meta.MediaID, meta.ServerName, meta.ContentType,
		meta.SizeBytes, meta.SHA256, nullString(meta.FileName), meta.StorageKey,
	); err != nil {
		return fmt.Errorf("store: insert remote media: %w", err)
	}

	return nil
}

// UpsertImported сохраняет метаданные импортированного медиа: если запись уже есть
// (например, из PoC-импорта без sha256 или из прошлого прогона), обновляет sha256,
// размер, storage_key, имя файла и карантин.
//
// Размер - тот, что реально записан в хранилище (импорт передает его, а не
// media_length дампа): Content-Length скачивания берется отсюда, и при
// расхождении с дампом скачивание обрывалось. Карантин источника
// (quarantined_by) переносится и блокирует медиа (blocked); повторный прогон
// повторяет состояние источника - снятый в Synapse карантин снимается.
func (s *MediaStore) UpsertImported(ctx context.Context, meta media.Meta) error {
	const query = `
		INSERT INTO media (content_uri, media_id, server_name, uploader, content_type,
		                   size_bytes, sha256, file_name, storage_key, created_at,
		                   quarantined_by, blocked)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, coalesce($10, now()), $11::text, $11::text IS NOT NULL)
		ON CONFLICT (content_uri) DO UPDATE SET
			sha256      = coalesce(EXCLUDED.sha256, media.sha256),
			size_bytes  = CASE WHEN EXCLUDED.size_bytes > 0 THEN EXCLUDED.size_bytes ELSE media.size_bytes END,
			storage_key = EXCLUDED.storage_key,
			file_name   = coalesce(EXCLUDED.file_name, media.file_name),
			content_type = CASE WHEN EXCLUDED.content_type <> '' THEN EXCLUDED.content_type ELSE media.content_type END,
			quarantined_by = EXCLUDED.quarantined_by,
			blocked     = EXCLUDED.blocked`

	var createdAt any
	if !meta.CreatedAt.IsZero() {
		createdAt = meta.CreatedAt
	}

	if _, err := s.db.pool.Exec(ctx, query,
		meta.ContentURI, meta.MediaID, meta.ServerName, nullString(meta.Uploader), meta.ContentType,
		meta.SizeBytes, meta.SHA256, nullString(meta.FileName), meta.StorageKey, createdAt,
		nullString(meta.QuarantinedBy),
	); err != nil {
		return fmt.Errorf("store: upsert imported media %s: %w", meta.ContentURI, err)
	}

	return nil
}

// Imported возвращает метаданные медиа по content_uri вместе с заблокированными
// (карантин): повторный импорт по ним решает, копировать ли байты.
func (s *MediaStore) Imported(ctx context.Context, contentURI string) (media.Meta, error) {
	const query = `
		SELECT content_uri, media_id, server_name, coalesce(uploader, ''), content_type,
		       size_bytes, sha256, coalesce(file_name, ''), storage_key, uploaded, created_at,
		       coalesce(quarantined_by, '')
		FROM media
		WHERE content_uri = $1`

	var meta media.Meta

	err := s.db.pool.QueryRow(ctx, query, contentURI).Scan(
		&meta.ContentURI, &meta.MediaID, &meta.ServerName, &meta.Uploader, &meta.ContentType,
		&meta.SizeBytes, &meta.SHA256, &meta.FileName, &meta.StorageKey, &meta.Uploaded, &meta.CreatedAt,
		&meta.QuarantinedBy,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return media.Meta{}, media.ErrNotFound
		}

		return media.Meta{}, fmt.Errorf("store: imported media: %w", err)
	}

	return meta, nil
}

// ByID ищет метаданные по (server_name, media_id).
func (s *MediaStore) ByID(ctx context.Context, serverName, mediaID string) (media.Meta, error) {
	const query = `
		SELECT content_uri, media_id, server_name, coalesce(uploader, ''), content_type,
		       size_bytes, sha256, coalesce(file_name, ''), storage_key, uploaded, created_at
		FROM media
		WHERE server_name = $1 AND media_id = $2 AND blocked = false`

	var meta media.Meta

	err := s.db.pool.QueryRow(ctx, query, serverName, mediaID).Scan(
		&meta.ContentURI, &meta.MediaID, &meta.ServerName, &meta.Uploader, &meta.ContentType,
		&meta.SizeBytes, &meta.SHA256, &meta.FileName, &meta.StorageKey, &meta.Uploaded, &meta.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return media.Meta{}, media.ErrNotFound
		}

		return media.Meta{}, fmt.Errorf("store: media by id: %w", err)
	}

	return meta, nil
}

// TouchAccess обновляет last_accessed_at; ошибки не критичны для скачивания.
// С включенным накопителем скачивание не пишет в БД: отметка уходит
// пачкой фоновым сбросом, повторы одного медиа за интервал схлопываются. Метку
// пока ничто не читает (очистки медиа по давности нет).
func (s *MediaStore) TouchAccess(ctx context.Context, contentURI string) error {
	if activity := s.db.activity.Load(); activity != nil {
		activity.TouchMedia(contentURI)

		return nil
	}

	const query = `UPDATE media SET last_accessed_at = now() WHERE content_uri = $1`

	if _, err := s.db.pool.Exec(ctx, query, contentURI); err != nil {
		return fmt.Errorf("store: touch media access: %w", err)
	}

	return nil
}

// nullString возвращает NULL для пустой строки: file_name необязателен.
func nullString(s string) any {
	if s == "" {
		return nil
	}

	return s
}

// InsertPending создает placeholder медиа (MSC2246). media_id серверный,
// поэтому конфликт - ошибка, а не молчаливый DO NOTHING.
func (s *MediaStore) InsertPending(ctx context.Context, meta media.Meta) error {
	_, err := s.db.pool.Exec(ctx, `
		INSERT INTO media (content_uri, media_id, server_name, uploader, content_type,
		                   size_bytes, file_name, storage_key, uploaded, created_at)
		VALUES ($1,$2,$3,$4,'',0,'','',false,$5)`,
		meta.ContentURI, meta.MediaID, meta.ServerName, meta.Uploader, meta.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: insert pending media: %w", err)
	}

	return nil
}

// UpdateUploaded записывает метаданные после загрузки байтов. Условие
// uploaded = false: из двух параллельных PUT побеждает один, второй получает
// ErrAlreadyUploaded и не смешивает свои метаданные с чужими байтами.
func (s *MediaStore) UpdateUploaded(ctx context.Context, meta media.Meta) error {
	tag, err := s.db.pool.Exec(ctx, `
		UPDATE media SET content_type = $3, size_bytes = $4, sha256 = $5,
		       file_name = $6, storage_key = $7, uploaded = true
		WHERE server_name = $1 AND media_id = $2 AND uploaded = false`,
		meta.ServerName, meta.MediaID, meta.ContentType, meta.SizeBytes,
		meta.SHA256, meta.FileName, meta.StorageKey)
	if err != nil {
		return fmt.Errorf("store: update uploaded media: %w", err)
	}

	if tag.RowsAffected() > 0 {
		return nil
	}

	var exists bool
	if err := s.db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM media WHERE server_name = $1 AND media_id = $2)`,
		meta.ServerName, meta.MediaID).Scan(&exists); err != nil {
		return fmt.Errorf("store: check uploaded media: %w", err)
	}

	if exists {
		return media.ErrAlreadyUploaded
	}

	return media.ErrNotFound
}

// PendingStats считает незаполненные placeholder пользователя, созданные не
// раньше since (предел max_pending_media_uploads).
func (s *MediaStore) PendingStats(ctx context.Context, uploader string, since time.Time) (int, time.Time, error) {
	var (
		count  int
		oldest *time.Time
	)

	err := s.db.pool.QueryRow(ctx, `
		SELECT count(*), min(created_at)
		FROM media
		WHERE uploader = $1 AND uploaded = false AND created_at >= $2`,
		uploader, since).Scan(&count, &oldest)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("store: pending media stats: %w", err)
	}

	if oldest == nil {
		return count, time.Time{}, nil
	}

	return count, *oldest, nil
}

// DeleteExpiredPending удаляет просроченные незаполненные placeholder пользователя.
func (s *MediaStore) DeleteExpiredPending(ctx context.Context, uploader string, before time.Time) error {
	if _, err := s.db.pool.Exec(ctx, `
		DELETE FROM media
		WHERE uploader = $1 AND uploaded = false AND created_at < $2`,
		uploader, before); err != nil {
		return fmt.Errorf("store: delete expired pending media: %w", err)
	}

	return nil
}
