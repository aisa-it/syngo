// Package media реализует медиа-репозиторий Matrix: метаданные в БД, байты — в
// объектном хранилище (локальный каталог для dev, S3/MinIO — отдельной реализацией
// media.ObjectStore).
//
// Правила:
//   - content_uri = mxc://<server_name>/<media_id>, media_id — случайный, не производный
//     от имени файла (никаких путей пользователя в хранилище);
//   - в БД хранится sha256 и размер, байты — по storage_key;
//   - скачивание по (server_name, media_id): аутентифицированные пути v1 отдают все,
//     неаутентифицированные legacy-пути - только медиа до freeze,
//     загрузка - только с валидным access token.
package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
)

// Ошибки медиа-слоя. Транспорт отображает их в коды Matrix.
var (
	// ErrNotFound — метаданных нет (или media_id не наш).
	ErrNotFound = errors.New("media: not found")
	// ErrTooLarge — загрузка превысила лимит.
	ErrTooLarge = errors.New("media: too large")
	// ErrUnsupportedType — пустой/некорректный Content-Type.
	ErrUnsupportedType = errors.New("media: unsupported content type")
	// ErrNotConfigured — хранилище не сконфигурировано.
	ErrNotConfigured = errors.New("media: store is not configured")
	// ErrInvalidMediaID - media_id вне алфавита серверных id (400 M_INVALID_PARAM).
	ErrInvalidMediaID = errors.New("media: invalid media id")
	// ErrForbidden - placeholder создан другим пользователем (403 M_FORBIDDEN).
	ErrForbidden = errors.New("media: forbidden")
	// ErrPendingLimit - достигнут предел незаполненных placeholder (429 M_LIMIT_EXCEEDED).
	ErrPendingLimit = errors.New("media: too many pending uploads")
)

// PendingLimitError - отказ /create по пределу placeholder; RetryAfter - когда
// истечет самый старый из них (Synapse max_pending_media_uploads).
type PendingLimitError struct {
	RetryAfter time.Duration
}

func (e *PendingLimitError) Error() string { return ErrPendingLimit.Error() }

// Is позволяет проверять ошибку через errors.Is(err, ErrPendingLimit).
func (e *PendingLimitError) Is(target error) bool { return target == ErrPendingLimit }

// Значения по умолчанию для асинхронной загрузки - как в Synapse.
const (
	// DefaultMaxPendingUploads - max_pending_media_uploads.
	DefaultMaxPendingUploads = 5
	// DefaultUnusedExpiration - unused_expiration_time.
	DefaultUnusedExpiration = 24 * time.Hour
	// DefaultDownloadWait - ожидание async-загрузки без timeout_ms (20 с).
	DefaultDownloadWait = 20 * time.Second
	// MaxDownloadWait - верхняя граница timeout_ms (60 с).
	MaxDownloadWait = 60 * time.Second
)

// pendingPollInterval - период опроса placeholder при ожидании загрузки (как в Synapse, 0,5 с).
const pendingPollInterval = 500 * time.Millisecond

// Options - настройки медиа-репозитория сверх лимита размера.
type Options struct {
	// MaxPendingUploads - предел незаполненных placeholder на пользователя (<= 0 - без предела).
	MaxPendingUploads int
	// UnusedExpiration - срок, за который placeholder должен быть заполнен.
	UnusedExpiration time.Duration
	// AuthenticatedMedia - legacy-пути отдают только медиа, созданные до LegacyFreeze
	// (Synapse enable_authenticated_media). false - legacy отдает все.
	AuthenticatedMedia bool
	// LegacyFreeze - момент, с которого новые медиа доступны только по токену
	// (freeze из MSC3916). Нулевое значение при AuthenticatedMedia - legacy не отдает ничего.
	LegacyFreeze time.Time
}

// ReadOptions - как читать медиа при скачивании.
type ReadOptions struct {
	// MaxWait - сколько ждать заполнения placeholder (timeout_ms); 0 - не ждать.
	MaxWait time.Duration
	// Legacy - запрос пришел на неаутентифицированный /_matrix/media/v3.
	Legacy bool
}

// validMediaID - алфавит media_id: серверные id (base64url) и id Synapse
// (буквы); точки и слеши недопустимы - id становится частью ключа хранилища.
func validMediaID(id string) bool {
	if id == "" || len(id) > 255 {
		return false
	}

	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}

// Meta — метаданные медиа (строка таблицы media).
type Meta struct {
	ContentURI     string
	MediaID        string
	ServerName     string
	Uploader       string
	ContentType    string
	SizeBytes      int64
	SHA256         []byte
	FileName       string
	StorageKey     string
	Uploaded       bool
	CreatedAt      time.Time
	LastAccessedAt *time.Time
	// QuarantinedBy - кто поместил медиа в карантин (перенос из Synapse);
	// непустое значение блокирует скачивание и миниатюры.
	QuarantinedBy string
	// IsRemote - кеш медиа другого сервера.
	IsRemote bool
}

// Repo — хранилище метаданных (реализуется internal/store).
type Repo interface {
	// Insert сохраняет метаданные; конфликт по content_uri — ошибка.
	Insert(ctx context.Context, meta Meta) error
	// ByID возвращает метаданные по (server_name, media_id); ErrNotFound — если нет.
	ByID(ctx context.Context, serverName, mediaID string) (Meta, error)
	// TouchAccess обновляет last_accessed_at (best-effort, ошибки не критичны).
	TouchAccess(ctx context.Context, contentURI string) error
	// InsertPending создает placeholder (uploaded=false) под серверный media_id.
	InsertPending(ctx context.Context, meta Meta) error
	// UpdateUploaded записывает метаданные после асинхронной загрузки только для
	// незаполненного placeholder: уже загруженный - ErrAlreadyUploaded, нет строки - ErrNotFound.
	UpdateUploaded(ctx context.Context, meta Meta) error
	// PendingStats - число незаполненных placeholder пользователя, созданных не
	// раньше since, и время создания самого старого из них.
	PendingStats(ctx context.Context, uploader string, since time.Time) (int, time.Time, error)
	// DeleteExpiredPending удаляет незаполненные placeholder пользователя, созданные раньше before.
	DeleteExpiredPending(ctx context.Context, uploader string, before time.Time) error
}

// ObjectStore — хранилище байтов.
type ObjectStore interface {
	// Put сохраняет объект по ключу и возвращает число записанных байт.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Get открывает объект; ErrNotFound — если объекта нет.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete удаляет объект (best-effort при откате неудачной загрузки).
	Delete(ctx context.Context, key string) error
}

// Service — операции медиа-репозитория.
type Service struct {
	repo      Repo
	objects   ObjectStore
	server    string
	maxUpload int64
	opts      Options
	now       func() time.Time
	// remote скачивает медиа других серверов (nil - их медиа не отдается).
	remote  RemoteFetcher
	fetches remoteFetches
}

// NewService собирает сервис. maxUpload <= 0 означает «без лимита» (не рекомендуется).
func NewService(repo Repo, objects ObjectStore, serverName string, maxUpload int64) *Service {
	return &Service{
		repo:      repo,
		objects:   objects,
		server:    serverName,
		maxUpload: maxUpload,
		opts: Options{
			MaxPendingUploads:  DefaultMaxPendingUploads,
			UnusedExpiration:   DefaultUnusedExpiration,
			AuthenticatedMedia: true,
		},
		now: time.Now,
	}
}

// Configure задает настройки async-загрузки и доступа legacy-путей.
// Нулевой UnusedExpiration заменяется значением по умолчанию.
func (s *Service) Configure(opts Options) {
	if opts.UnusedExpiration <= 0 {
		opts.UnusedExpiration = DefaultUnusedExpiration
	}

	s.opts = opts
}

// MaxUploadBytes возвращает лимит загрузки (для /config).
func (s *Service) MaxUploadBytes() int64 {
	return s.maxUpload
}

// Upload читает поток, считает sha256 и размер, кладёт байты в объектное хранилище
// и метаданные в БД. Возвращает content_uri (mxc://…).
//
// Лимит проверяется и здесь (по факту прочитанного), и в транспорте (MaxBytesReader),
// чтобы одинаково работать при любом источнике вызова.
func (s *Service) Upload(ctx context.Context, uploader, contentType, fileName string, r io.Reader) (string, error) {
	if s.repo == nil || s.objects == nil {
		return "", ErrNotConfigured
	}

	contentType = strings.TrimSpace(contentType)
	if contentType == "" || strings.ContainsAny(contentType, "\r\n") {
		return "", ErrUnsupportedType
	}

	mediaID, err := newMediaID()
	if err != nil {
		return "", fmt.Errorf("media: generate id: %w", err)
	}

	storageKey := path.Join(s.server, mediaID)
	hasher := sha256.New()

	var written int64
	if s.maxUpload > 0 {
		limited := io.LimitReader(r, s.maxUpload+1)

		written, err = s.objects.Put(ctx, storageKey, io.TeeReader(limited, hasher))
		if err != nil {
			return "", fmt.Errorf("media: put object: %w", err)
		}

		if written > s.maxUpload {
			_ = s.objects.Delete(ctx, storageKey)

			return "", ErrTooLarge
		}
	} else {
		written, err = s.objects.Put(ctx, storageKey, io.TeeReader(r, hasher))
		if err != nil {
			return "", fmt.Errorf("media: put object: %w", err)
		}
	}

	contentURI := "mxc://" + s.server + "/" + mediaID
	meta := Meta{
		ContentURI:  contentURI,
		MediaID:     mediaID,
		ServerName:  s.server,
		Uploader:    uploader,
		ContentType: contentType,
		SizeBytes:   written,
		SHA256:      hasher.Sum(nil),
		FileName:    sanitizeFileName(fileName),
		StorageKey:  storageKey,
		CreatedAt:   s.now().UTC(),
	}

	if err := s.repo.Insert(ctx, meta); err != nil {
		_ = s.objects.Delete(ctx, storageKey) // откат: не оставляем сироту в хранилище

		return "", fmt.Errorf("media: insert metadata: %w", err)
	}

	return contentURI, nil
}

// Download отдает метаданные и поток байтов без ожидания async-загрузки
// (внутренние вызовы и импорт). HTTP-слой использует DownloadWith.
func (s *Service) Download(ctx context.Context, serverName, mediaID string) (Meta, io.ReadCloser, error) {
	return s.DownloadWith(ctx, serverName, mediaID, ReadOptions{})
}

// DownloadWith отдает метаданные и поток байтов. Медиа другого сервера берется
// из кеша или скачивается у владельца.
func (s *Service) DownloadWith(ctx context.Context, serverName, mediaID string, opts ReadOptions) (Meta, io.ReadCloser, error) {
	meta, err := s.resolve(ctx, serverName, mediaID, opts)
	if err != nil {
		return Meta{}, nil, err
	}

	body, err := s.objects.Get(ctx, meta.StorageKey)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Meta{}, nil, ErrNotFound
		}

		return Meta{}, nil, fmt.Errorf("media: get object: %w", err)
	}

	if err := s.repo.TouchAccess(ctx, meta.ContentURI); err != nil {
		// не критично: аналитика доступа не должна ломать скачивание
		_ = err
	}

	return meta, body, nil
}

// resolve находит загруженное медиа: проверяет доступ legacy-пути и при
// необходимости ждет заполнения placeholder до opts.MaxWait (MSC2246, как
// get_local_media_info в Synapse).
func (s *Service) resolve(ctx context.Context, serverName, mediaID string, opts ReadOptions) (Meta, error) {
	if s.repo == nil || s.objects == nil {
		return Meta{}, ErrNotConfigured
	}

	if serverName != s.server {
		return s.resolveRemote(ctx, serverName, mediaID, opts)
	}

	deadline := s.now().Add(opts.MaxWait)

	for {
		meta, err := s.repo.ByID(ctx, serverName, mediaID)
		if err != nil {
			return Meta{}, err
		}

		// Legacy-путь не раскрывает даже существование медиа после freeze.
		if opts.Legacy && !s.legacyAllowed(meta) {
			return Meta{}, ErrNotFound
		}

		// Legacy-строки (до миграции 0021) считаем загруженными, если есть storage_key.
		if meta.Uploaded || meta.StorageKey != "" {
			return meta, nil
		}

		now := s.now()
		if s.pendingExpired(meta, now) {
			return Meta{}, ErrNotFound
		}

		if !now.Before(deadline) {
			return Meta{}, ErrNotYetUploaded
		}

		select {
		case <-ctx.Done():
			return Meta{}, ctx.Err()
		case <-time.After(pendingPollInterval):
		}
	}
}

// legacyAllowed - можно ли отдать медиа через неаутентифицированный legacy-путь.
func (s *Service) legacyAllowed(meta Meta) bool {
	if !s.opts.AuthenticatedMedia {
		return true
	}

	return meta.CreatedAt.Before(s.opts.LegacyFreeze)
}

// pendingExpired - placeholder не заполнен за unused_expiration_time.
func (s *Service) pendingExpired(meta Meta, now time.Time) bool {
	return meta.CreatedAt.Add(s.opts.UnusedExpiration).Before(now)
}

// newMediaID генерирует 32-символьный url-safe идентификатор (24 байта энтропии).
func newMediaID() (string, error) {
	buf := make([]byte, 18) // 18 байт -> 24 символа base64url без паддинга
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// sanitizeFileName убирает путь и управляющие символы: имя файла — атрибут для
// Content-Disposition, а не часть ключа хранилища.
func sanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}

		return r
	}, name)

	if name == "." || name == "/" {
		return ""
	}

	if len(name) > 200 {
		name = name[:200]
	}

	return name
}

// SHA256Hex — вспомогательное представление хеша для логов/тестов.
func SHA256Hex(sum []byte) string {
	return hex.EncodeToString(sum)
}

// ErrNotYetUploaded — placeholder создан, байты ещё не загружены (504 M_NOT_YET_UPLOADED).
var ErrNotYetUploaded = errors.New("media: not yet uploaded")

// ErrAlreadyUploaded — попытка повторно загрузить тот же media_id (409).
var ErrAlreadyUploaded = errors.New("media: already uploaded")

// CreatePending создает placeholder с серверным media_id (POST /_matrix/media/v1/create,
// MSC2246). Возвращает content_uri и момент, после которого незаполненный
// placeholder перестает действовать (unused_expires_at).
func (s *Service) CreatePending(ctx context.Context, uploader string) (string, time.Time, error) {
	if s.repo == nil {
		return "", time.Time{}, ErrNotConfigured
	}

	now := s.now().UTC()
	since := now.Add(-s.opts.UnusedExpiration)

	// Просроченные placeholder пользователя больше не нужны: чистим при каждом
	// create, чтобы таблица не копила мусор (лениво, без фонового воркера).
	if err := s.repo.DeleteExpiredPending(ctx, uploader, since); err != nil {
		return "", time.Time{}, fmt.Errorf("media: delete expired pending: %w", err)
	}

	if s.opts.MaxPendingUploads > 0 {
		count, oldest, err := s.repo.PendingStats(ctx, uploader, since)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("media: pending stats: %w", err)
		}

		if count >= s.opts.MaxPendingUploads {
			return "", time.Time{}, &PendingLimitError{RetryAfter: oldest.Add(s.opts.UnusedExpiration).Sub(now)}
		}
	}

	mediaID, err := newMediaID()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("media: generate id: %w", err)
	}

	meta := Meta{
		ContentURI: "mxc://" + s.server + "/" + mediaID,
		MediaID:    mediaID,
		ServerName: s.server,
		Uploader:   uploader,
		CreatedAt:  now,
	}

	if err := s.repo.InsertPending(ctx, meta); err != nil {
		return "", time.Time{}, err
	}

	return meta.ContentURI, now.Add(s.opts.UnusedExpiration), nil
}

// UploadTo загружает байты в существующий placeholder (PUT /_matrix/media/v3/upload/{server}/{id}).
// Порядок проверок как в Synapse verify_can_upload: нет id - ErrNotFound, чужой -
// ErrForbidden, уже загружен - ErrAlreadyUploaded, просрочен - ErrNotFound.
func (s *Service) UploadTo(ctx context.Context, userID, mediaID, contentType, fileName string, r io.Reader, maxBytes int64) (Meta, error) {
	if s.repo == nil || s.objects == nil {
		return Meta{}, ErrNotConfigured
	}

	// id становится частью ключа хранилища: "." или "a/b" иначе ломали каталог
	// сервера.
	if !validMediaID(mediaID) {
		return Meta{}, ErrInvalidMediaID
	}

	meta, err := s.repo.ByID(ctx, s.server, mediaID)
	if err != nil {
		return Meta{}, err
	}

	if userID == "" || meta.Uploader != userID {
		return Meta{}, ErrForbidden
	}

	if meta.Uploaded || meta.StorageKey != "" {
		return Meta{}, ErrAlreadyUploaded
	}

	if s.pendingExpired(meta, s.now()) {
		return Meta{}, ErrNotFound
	}

	contentType = strings.TrimSpace(contentType)
	if contentType == "" || strings.ContainsAny(contentType, "\r\n") {
		contentType = "application/octet-stream"
	}

	// Свой ключ на каждую попытку: два параллельных PUT не перезаписывают байты
	// друг друга, а условный UpdateUploaded оставляет ровно одного победителя.
	suffix, err := newMediaID()
	if err != nil {
		return Meta{}, fmt.Errorf("media: generate storage key: %w", err)
	}

	storageKey := path.Join(s.server, mediaID+"-"+suffix)
	hasher := sha256.New()

	reader := r
	if maxBytes > 0 {
		reader = io.LimitReader(r, maxBytes+1)
	}

	written, err := s.objects.Put(ctx, storageKey, io.TeeReader(reader, hasher))
	if err != nil {
		return Meta{}, fmt.Errorf("media: put object: %w", err)
	}

	if maxBytes > 0 && written > maxBytes {
		_ = s.objects.Delete(ctx, storageKey)

		return Meta{}, ErrTooLarge
	}

	meta.ContentType = contentType
	meta.SizeBytes = written
	meta.SHA256 = hasher.Sum(nil)
	meta.FileName = sanitizeFileName(fileName)
	meta.StorageKey = storageKey
	meta.Uploaded = true

	if err := s.repo.UpdateUploaded(ctx, meta); err != nil {
		_ = s.objects.Delete(ctx, storageKey)

		if errors.Is(err, ErrAlreadyUploaded) || errors.Is(err, ErrNotFound) {
			return Meta{}, err
		}

		return Meta{}, fmt.Errorf("media: update uploaded: %w", err)
	}

	return meta, nil
}
