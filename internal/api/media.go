package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/media"
)

// ErrCodeTooLarge — файл больше лимита медиа-репозитория (spec: 413 M_TOO_LARGE).
const ErrCodeTooLarge = "M_TOO_LARGE"

// ErrCodeNotYetUploaded - placeholder async-загрузки еще пуст (MSC2246).
const ErrCodeNotYetUploaded = "M_NOT_YET_UPLOADED"

// ErrCodeCannotOverwriteMedia - в placeholder уже загружены байты (MSC2246).
const ErrCodeCannotOverwriteMedia = "M_CANNOT_OVERWRITE_MEDIA"

// limitMediaPending - отказ /create по пределу незаполненных placeholder
// (Synapse max_pending_media_uploads); в лог и метрику идет как отдельный лимит.
const limitMediaPending limitKind = "media_pending"

// registerMediaRoutes подключает медиа-репозиторий.
//
//   - /_matrix/client/v1/media/* - основной путь CS API v1.11 (MSC3916): все
//     эндпоинты только с access token;
//   - POST /_matrix/media/v1/create + PUT /_matrix/media/v3/upload/{server}/{id} -
//     асинхронная загрузка (MSC2246) с серверным media_id;
//   - /_matrix/media/v3/* - легаси: upload, config, preview_url с токеном, download
//     и thumbnail без токена, но только для медиа до freeze (как в Synapse
//     enable_authenticated_media).
func (s *Server) registerMediaRoutes(r chi.Router) {
	// rc_media_create в Synapse: лимит по пользователю на upload и create.
	uploadLimit := s.rateLimit(limitMediaUpload, userOrIPKey)

	r.Route("/_matrix/client/v1/media", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/download/{serverName}/{mediaId}", s.handleMediaDownload)
		r.Get("/download/{serverName}/{mediaId}/{fileName}", s.handleMediaDownload)
		r.Get("/thumbnail/{serverName}/{mediaId}", s.handleMediaThumbnail)
		r.Get("/config", s.handleMediaConfig)
		r.Get("/preview_url", s.handleMediaPreviewURL)
		// Загрузки в спеке нет под v1; алиас сохранен для совместимости.
		r.With(uploadLimit).Post("/upload", s.handleMediaUpload)
	})

	r.With(s.requireAuth, uploadLimit).Post("/_matrix/media/v1/create", s.handleMediaCreate)

	r.Route("/_matrix/media/v3", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.With(uploadLimit).Post("/upload", s.handleMediaUpload)
			r.Put("/upload/{serverName}/{mediaId}", s.handleMediaUploadTo)
			// Synapse требует токен для config и preview_url и на легаси-пути.
			r.Get("/config", s.handleMediaConfig)
			r.Get("/preview_url", s.handleMediaPreviewURL)
		})

		r.Get("/download/{serverName}/{mediaId}", s.handleLegacyMediaDownload)
		r.Get("/download/{serverName}/{mediaId}/{fileName}", s.handleLegacyMediaDownload)
		r.Get("/thumbnail/{serverName}/{mediaId}", s.handleLegacyMediaThumbnail)
	})
}

// mediaOptions переводит конфиг в настройки медиа-сервиса.
func mediaOptions(cfg config.Media) media.Options {
	return media.Options{
		MaxPendingUploads:  cfg.MaxPendingUploads,
		UnusedExpiration:   cfg.UnusedExpiration.Std(),
		AuthenticatedMedia: cfg.EnableAuthenticatedMedia,
		LegacyFreeze:       cfg.LegacyFreezeTime(),
	}
}

// mediaUploadBody ограничивает тело загрузки: Content-Length больше лимита - сразу
// 413, без Content-Length - MaxBytesReader (лимит + 1 байт, чтобы превышение
// распознал сервис). Возвращает false, если ответ уже записан.
func (s *Server) mediaUploadBody(w http.ResponseWriter, r *http.Request) (io.Reader, bool) {
	limit := s.media.MaxUploadBytes()
	if limit <= 0 {
		return r.Body, true
	}

	if r.ContentLength > limit {
		writeMatrixError(w, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "Upload is too large")

		return nil, false
	}

	return http.MaxBytesReader(w, r.Body, limit+1), true
}

// mediaContentType - Content-Type загрузки; без заголовка, как в Synapse,
// application/octet-stream.
func mediaContentType(r *http.Request) string {
	if contentType := strings.TrimSpace(r.Header.Get("Content-Type")); contentType != "" {
		return contentType
	}

	return "application/octet-stream"
}

// handleMediaUpload принимает файл и возвращает content_uri.
func (s *Server) handleMediaUpload(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Media repository is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	defer func() { _ = r.Body.Close() }()

	body, ok := s.mediaUploadBody(w, r)
	if !ok {
		return
	}

	fileName := r.URL.Query().Get("filename")

	contentURI, err := s.media.Upload(r.Context(), session.UserID, mediaContentType(r), fileName, body)
	if err != nil {
		var maxErr *http.MaxBytesError

		switch {
		case errors.Is(err, media.ErrTooLarge), errors.As(err, &maxErr):
			writeMatrixError(w, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "Upload is too large")
		case errors.Is(err, media.ErrUnsupportedType):
			writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Invalid Content-Type")
		default:
			s.log.ErrorContext(r.Context(), "api: media upload failed", "error", err)
			writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to store media")
		}

		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"content_uri": contentURI})
}

// handleMediaDownload - GET /_matrix/client/v1/media/download (с токеном).
func (s *Server) handleMediaDownload(w http.ResponseWriter, r *http.Request) {
	s.serveMediaDownload(w, r, false)
}

// handleLegacyMediaDownload - GET /_matrix/media/v3/download (без токена, до freeze).
func (s *Server) handleLegacyMediaDownload(w http.ResponseWriter, r *http.Request) {
	s.serveMediaDownload(w, r, true)
}

// serveMediaDownload отдает байты медиа.
func (s *Server) serveMediaDownload(w http.ResponseWriter, r *http.Request, legacy bool) {
	if s.media == nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Media repository is not configured")

		return
	}

	wait, ok := mediaWait(w, r)
	if !ok {
		return
	}

	serverName := pathParamValue(r, "serverName")
	mediaID := pathParamValue(r, "mediaId")

	meta, body, err := s.media.DownloadWith(r.Context(), serverName, mediaID,
		media.ReadOptions{MaxWait: wait, Legacy: legacy})
	if err != nil {
		s.writeMediaReadError(w, r, err)

		return
	}

	defer func() { _ = body.Close() }()

	// Имя из пути download/{server}/{id}/{fileName} переопределяет сохраненное (как в Synapse).
	fileName := meta.FileName
	if name := pathParamValue(r, "fileName"); name != "" {
		fileName = name
	}

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(meta.SizeBytes, 10))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Disposition", contentDisposition(meta.ContentType, fileName))
	setMediaSecurityHeaders(w)

	w.WriteHeader(http.StatusOK)

	// Тело уже проверено лимитом при загрузке — отдаём потоком.
	if _, err := io.Copy(w, body); err != nil {
		s.log.WarnContext(r.Context(), "api: media stream interrupted", "error", err)
	}
}

// mediaWait разбирает timeout_ms (MSC2246): сколько ждать заполнения placeholder.
// По умолчанию 20 с, не больше 60 с - как в Synapse.
func mediaWait(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("timeout_ms")
	if raw == "" {
		return media.DefaultDownloadWait, true
	}

	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms < 0 {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "timeout_ms must be a non-negative integer")

		return 0, false
	}

	if ms >= media.MaxDownloadWait.Milliseconds() {
		return media.MaxDownloadWait, true
	}

	return time.Duration(ms) * time.Millisecond, true
}

// writeMediaReadError отображает ошибки чтения медиа в коды Matrix.
func (s *Server) writeMediaReadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, media.ErrNotYetUploaded):
		writeMatrixError(w, http.StatusGatewayTimeout, ErrCodeNotYetUploaded, "Media has not been uploaded yet")
	case errors.Is(err, media.ErrNotFound):
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Media not found")
	case errors.Is(err, media.ErrRemoteUnavailable):
		// Как в Synapse: сервер-владелец не отдал медиа - 502.
		s.log.WarnContext(r.Context(), "api: remote media fetch failed", "error", err)
		writeMatrixError(w, http.StatusBadGateway, ErrCodeUnknown, "Failed to fetch remote media")
	case errors.Is(err, media.ErrBadThumbnailParams):
		writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid thumbnail parameters")
	case errors.Is(err, media.ErrUnsupportedImage):
		// Спека: 400 - сервер не может сделать миниатюру этого содержимого.
		writeMatrixError(w, http.StatusBadRequest, ErrCodeUnknown, "Cannot generate a thumbnail for this media")
	case errors.Is(err, media.ErrTooLarge):
		writeMatrixError(w, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "Media is too large to thumbnail")
	case errors.Is(err, context.Canceled):
		// Клиент ушел, пока ждали async-загрузку: отвечать некому.
	default:
		s.log.ErrorContext(r.Context(), "api: media read failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to read media")
	}
}

// setMediaSecurityHeaders - заголовки отдаваемого медиа как в Synapse.
// Загруженный HTML/SVG иначе исполнился бы в origin homeserver (хранимый XSS):
// запрещаем активный контент и sniffing. CORP cross-origin нужен
// веб-клиенту на другом origin, если тот включает COEP.
func setMediaSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", mediaCSP)
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

// handleMediaConfig отдаёт лимиты медиа (клиент показывает до загрузки).
// Без лимита m.upload.size не отдаем: поле необязательное, а 0 клиент понял бы
// как нулевой лимит.
func (s *Server) handleMediaConfig(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{}

	if s.media != nil {
		if limit := s.media.MaxUploadBytes(); limit > 0 {
			resp["m.upload.size"] = limit
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleMediaPreviewURL - превью ссылок не генерируем (исходящие запросы и защита
// от SSRF для них не реализованы): пустой OpenGraph-объект, клиент просто не
// показывает превью.
func (s *Server) handleMediaPreviewURL(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" {
		writeMatrixError(w, http.StatusBadRequest, ErrCodeMissingParam, "url query parameter is required")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{})
}

// mediaCSP — политика для отдаваемых медиа (как в Synapse): любой активный
// контент в загруженном файле не должен исполняться на нашем origin.
const mediaCSP = "sandbox; default-src 'none'; script-src 'none'; plugin-types application/pdf; " +
	"style-src 'unsafe-inline'; media-src 'self'; object-src 'self'"

// inlineMediaTypes - типы, которые безопасно отдавать inline (INLINE_CONTENT_TYPES
// Synapse); все остальное (text/html, image/svg+xml, PDF и прочее) уходит только
// как attachment.
var inlineMediaTypes = map[string]struct{}{
	"text/css":            {},
	"text/plain":          {},
	"text/csv":            {},
	"application/json":    {},
	"application/ld+json": {},
	"image/jpeg":          {},
	"image/gif":           {},
	"image/png":           {},
	"image/apng":          {},
	"image/webp":          {},
	"image/avif":          {},
	"video/mp4":           {},
	"video/webm":          {},
	"video/ogg":           {},
	"video/quicktime":     {},
	"audio/mp4":           {},
	"audio/webm":          {},
	"audio/aac":           {},
	"audio/mpeg":          {},
	"audio/ogg":           {},
	"audio/wave":          {},
	"audio/wav":           {},
	"audio/x-wav":         {},
	"audio/x-pn-wav":      {},
	"audio/flac":          {},
	"audio/x-flac":        {},
}

// mediaTypeOf отбрасывает параметры Content-Type (charset и т.п.).
func mediaTypeOf(contentType string) string {
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		return strings.ToLower(strings.TrimSpace(contentType[:idx]))
	}

	return strings.ToLower(strings.TrimSpace(contentType))
}

// contentDisposition собирает заголовок с именем файла, не ломаясь на не-ASCII.
// Типы вне белого списка отдаются как attachment: браузер не должен отображать
// загруженный пользователем HTML/SVG в нашем origin.
func contentDisposition(contentType, fileName string) string {
	disposition := "attachment"

	if _, ok := inlineMediaTypes[mediaTypeOf(contentType)]; ok {
		disposition = "inline"
	}

	if strings.TrimSpace(fileName) == "" {
		return disposition
	}

	if disp := mime.FormatMediaType(disposition, map[string]string{"filename": fileName}); disp != "" {
		return disp
	}

	return disposition
}

// handleMediaThumbnail - GET /_matrix/client/v1/media/thumbnail (с токеном).
func (s *Server) handleMediaThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveMediaThumbnail(w, r, false)
}

// handleLegacyMediaThumbnail - GET /_matrix/media/v3/thumbnail (без токена, до freeze).
func (s *Server) handleLegacyMediaThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveMediaThumbnail(w, r, true)
}

// serveMediaThumbnail отдает миниатюру: method=scale|crop, размеры приводятся к
// фиксированному набору.
func (s *Server) serveMediaThumbnail(w http.ResponseWriter, r *http.Request, legacy bool) {
	if s.media == nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Media repository is not configured")

		return
	}

	wait, ok := mediaWait(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()

	width, err := strconv.Atoi(query.Get("width"))
	if err != nil {
		width = 0
	}

	height, err := strconv.Atoi(query.Get("height"))
	if err != nil {
		height = 0
	}

	params := media.ThumbnailParams{Width: width, Height: height, Method: query.Get("method")}

	thumb, err := s.media.Thumbnail(r.Context(), pathParamValue(r, "serverName"), pathParamValue(r, "mediaId"),
		params, media.ReadOptions{MaxWait: wait, Legacy: legacy})
	if err != nil {
		s.writeMediaReadError(w, r, err)

		return
	}

	w.Header().Set("Content-Type", thumb.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(thumb.Data)))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Disposition", "inline")
	setMediaSecurityHeaders(w)
	_, _ = w.Write(thumb.Data) //nolint:gosec // G705: миниатюра - PNG/JPEG, Content-Type задан сервером
}

// handleMediaCreate - POST /_matrix/media/v1/create (MSC2246): placeholder с
// серверным media_id и сроком заполнения unused_expires_at.
func (s *Server) handleMediaCreate(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Media repository is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	contentURI, expiresAt, err := s.media.CreatePending(r.Context(), session.UserID)
	if err != nil {
		var limitErr *media.PendingLimitError
		if errors.As(err, &limitErr) {
			s.writeLimitExceeded(w, r, limitMediaPending, limitErr.RetryAfter)

			return
		}

		s.log.ErrorContext(r.Context(), "api: create pending media failed", "error", err)
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to create media")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"content_uri":       contentURI,
		"unused_expires_at": expiresAt.UnixMilli(),
	})
}

// handleMediaUploadTo - PUT /_matrix/media/v3/upload/{serverName}/{mediaId} (MSC2246).
func (s *Server) handleMediaUploadTo(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Media repository is not configured")

		return
	}

	session, ok := sessionFrom(r.Context())
	if !ok {
		writeMatrixError(w, http.StatusUnauthorized, ErrCodeMissingToken, "Missing access token")

		return
	}

	// Чужой сервер - 404, как в Synapse: загружать можно только в свои placeholder.
	if pathParamValue(r, "serverName") != s.cfg.Matrix.ServerName {
		writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Non-local server name specified")

		return
	}

	defer func() { _ = r.Body.Close() }()

	body, ok := s.mediaUploadBody(w, r)
	if !ok {
		return
	}

	_, err := s.media.UploadTo(r.Context(), session.UserID, pathParamValue(r, "mediaId"), mediaContentType(r),
		r.URL.Query().Get("filename"), body, s.media.MaxUploadBytes())
	if err != nil {
		var maxErr *http.MaxBytesError

		switch {
		case errors.Is(err, media.ErrInvalidMediaID):
			writeMatrixError(w, http.StatusBadRequest, ErrCodeInvalidParam, "Invalid media id")
		case errors.Is(err, media.ErrAlreadyUploaded):
			writeMatrixError(w, http.StatusConflict, ErrCodeCannotOverwriteMedia, "Media ID already has content")
		case errors.Is(err, media.ErrForbidden):
			writeMatrixError(w, http.StatusForbidden, ErrCodeForbidden, "You are not permitted to upload to this media ID")
		case errors.Is(err, media.ErrTooLarge), errors.As(err, &maxErr):
			writeMatrixError(w, http.StatusRequestEntityTooLarge, ErrCodeTooLarge, "Upload is too large")
		case errors.Is(err, media.ErrNotFound):
			writeMatrixError(w, http.StatusNotFound, ErrCodeNotFound, "Unknown or expired media ID")
		default:
			s.log.ErrorContext(r.Context(), "api: upload to media failed", "error", err)
			writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Failed to upload")
		}

		return
	}

	// Спека: успешный PUT отвечает пустым объектом.
	writeJSON(w, http.StatusOK, map[string]any{})
}
