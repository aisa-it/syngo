package media

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
)

// Медиа других серверов. Раньше любое mxc:// чужого сервера
// отдавало 404, и вложения собеседника с другого сервера не открывались.
// Теперь медиа скачивается у сервера-владельца (федеративный media API, для
// старых серверов - фолбэк на /_matrix/media/v3), кешируется в том же
// хранилище (media.is_remote) и отдается клиенту как свое. Лимит размера -
// max_upload_size, как в Synapse. Миниатюра строится из кешированного
// оригинала; оригинал больше лимита - миниатюра запрашивается у владельца.

// ErrRemoteUnavailable - сервер-владелец медиа недоступен или ответил ошибкой (502).
var ErrRemoteUnavailable = errors.New("media: remote server unavailable")

// RemoteFile - медиа, скачиваемое у сервера-владельца (тело закрывает вызывающий).
type RemoteFile struct {
	ContentType string
	FileName    string
	Body        io.ReadCloser
}

// RemoteFetcher скачивает медиа другого сервера. Отсутствие медиа - ErrNotFound.
type RemoteFetcher interface {
	FetchRemote(ctx context.Context, serverName, mediaID string) (RemoteFile, error)
	// FetchRemoteThumbnail - миниатюра у сервера-владельца (width/height/method).
	FetchRemoteThumbnail(ctx context.Context, serverName, mediaID string, params ThumbnailParams) (RemoteFile, error)
}

// RemoteRepo - вставка кешированного удаленного медиа (is_remote); повтор
// content_uri не ошибка: медиа уже скачал параллельный запрос.
type RemoteRepo interface {
	InsertRemote(ctx context.Context, meta Meta) error
}

// remoteFetches - скачивания в полете: параллельные запросы одного медиа ждут одно.
type remoteFetches struct {
	mu       sync.Mutex
	inFlight map[string]*remoteFetch
}

type remoteFetch struct {
	done chan struct{}
	err  error
}

// SetRemoteFetcher включает скачивание медиа других серверов (nil - выключить).
func (s *Service) SetRemoteFetcher(fetcher RemoteFetcher) {
	s.remote = fetcher
}

// validServerName - имя сервера годится в ключ хранилища.
func validServerName(name string) bool {
	return name != "" && len(name) <= 255 && !strings.ContainsAny(name, "/\\") &&
		!strings.Contains(name, "..") && strings.Trim(name, ".") != ""
}

// resolveRemote - кешированное медиа другого сервера или скачивание у владельца.
func (s *Service) resolveRemote(ctx context.Context, serverName, mediaID string, opts ReadOptions) (Meta, error) {
	if !validServerName(serverName) || !validMediaID(mediaID) {
		return Meta{}, ErrNotFound
	}

	meta, err := s.repo.ByID(ctx, serverName, mediaID)
	if err == nil && (meta.Uploaded || meta.StorageKey != "") {
		if opts.Legacy && !s.legacyAllowed(meta) {
			return Meta{}, ErrNotFound
		}

		return meta, nil
	}

	if err != nil && !errors.Is(err, ErrNotFound) {
		return Meta{}, err
	}

	// Legacy-путь после freeze не скачивает новое: оно все равно было бы недоступно.
	if s.remote == nil || (opts.Legacy && s.opts.AuthenticatedMedia) {
		return Meta{}, ErrNotFound
	}

	if err := s.fetchRemoteOnce(ctx, serverName, mediaID); err != nil {
		return Meta{}, err
	}

	return s.repo.ByID(ctx, serverName, mediaID)
}

// fetchRemoteOnce скачивает медиа; параллельные запросы того же медиа ждут первый.
func (s *Service) fetchRemoteOnce(ctx context.Context, serverName, mediaID string) error {
	key := serverName + "/" + mediaID

	s.fetches.mu.Lock()

	if s.fetches.inFlight == nil {
		s.fetches.inFlight = map[string]*remoteFetch{}
	}

	if call, ok := s.fetches.inFlight[key]; ok {
		s.fetches.mu.Unlock()

		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	call := &remoteFetch{done: make(chan struct{})}
	s.fetches.inFlight[key] = call
	s.fetches.mu.Unlock()

	// Скачивание не обрывается уходом первого клиента: его ждут остальные.
	call.err = s.fetchRemote(context.WithoutCancel(ctx), serverName, mediaID)

	s.fetches.mu.Lock()
	delete(s.fetches.inFlight, key)
	s.fetches.mu.Unlock()
	close(call.done)

	return call.err
}

// fetchRemote скачивает медиа у владельца и сохраняет в кеш.
func (s *Service) fetchRemote(ctx context.Context, serverName, mediaID string) error {
	file, err := s.remote.FetchRemote(ctx, serverName, mediaID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}

		return fmt.Errorf("%w: %w", ErrRemoteUnavailable, err)
	}

	defer func() { _ = file.Body.Close() }()

	storageKey := path.Join("remote", serverName, mediaID)
	hasher := sha256.New()

	reader := io.Reader(file.Body)
	if s.maxUpload > 0 {
		reader = io.LimitReader(file.Body, s.maxUpload+1)
	}

	written, err := s.objects.Put(ctx, storageKey, io.TeeReader(reader, hasher))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRemoteUnavailable, err)
	}

	if s.maxUpload > 0 && written > s.maxUpload {
		_ = s.objects.Delete(ctx, storageKey)

		return ErrTooLarge
	}

	contentType := strings.TrimSpace(file.ContentType)
	if contentType == "" || strings.ContainsAny(contentType, "\r\n") {
		contentType = "application/octet-stream"
	}

	meta := Meta{
		ContentURI: "mxc://" + serverName + "/" + mediaID, MediaID: mediaID, ServerName: serverName,
		ContentType: contentType, SizeBytes: written, SHA256: hasher.Sum(nil),
		FileName: sanitizeFileName(file.FileName), StorageKey: storageKey, Uploaded: true,
		CreatedAt: s.now().UTC(), IsRemote: true,
	}

	insert := s.repo.Insert
	if remote, ok := s.repo.(RemoteRepo); ok {
		insert = remote.InsertRemote
	}

	if err := insert(ctx, meta); err != nil {
		return fmt.Errorf("media: insert remote metadata: %w", err)
	}

	return nil
}

// remoteThumbnail - миниатюра у сервера-владельца (оригинал больше лимита).
func (s *Service) remoteThumbnail(ctx context.Context, serverName, mediaID string, params ThumbnailParams) (Thumb, error) {
	file, err := s.remote.FetchRemoteThumbnail(ctx, serverName, mediaID, params)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Thumb{}, ErrNotFound
		}

		return Thumb{}, fmt.Errorf("%w: %w", ErrRemoteUnavailable, err)
	}

	defer func() { _ = file.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(file.Body, maxRemoteThumbnailBytes+1))
	if err != nil {
		return Thumb{}, fmt.Errorf("%w: %w", ErrRemoteUnavailable, err)
	}

	if len(data) > maxRemoteThumbnailBytes {
		return Thumb{}, ErrTooLarge
	}

	contentType := mediaTypeOnly(file.ContentType)
	if !strings.HasPrefix(contentType, "image/") {
		return Thumb{}, ErrUnsupportedImage
	}

	return Thumb{ContentType: contentType, Data: data}, nil
}

// maxRemoteThumbnailBytes - предел миниатюры от сервера-владельца.
const maxRemoteThumbnailBytes = 10 << 20

// mediaTypeOnly - Content-Type без параметров.
func mediaTypeOnly(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}

	return strings.TrimSpace(strings.ToLower(contentType))
}
