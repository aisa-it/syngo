package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Коды ошибок Matrix, которые сервер отдаёт уже на этапе каркаса.
const (
	ErrCodeUnknown      = "M_UNKNOWN"
	ErrCodeBadJSON      = "M_BAD_JSON"
	ErrCodeNotJSON      = "M_NOT_JSON"
	ErrCodeUnrecognized = "M_UNRECOGNIZED"
	// ErrCodeNotFound — запрошенная сущность отсутствует.
	ErrCodeNotFound = "M_NOT_FOUND"
	// ErrCodeUnknownPos — токен since/next_batch не разбирается (M_UNKNOWN_POS).
	ErrCodeUnknownPos = "M_UNKNOWN_POS"
	// ErrCodeUnauthorized — федеративная подпись не принята (spec: M_UNAUTHORIZED).
	ErrCodeUnauthorized = "M_UNAUTHORIZED"
	// ErrCodeInvalidParam — параметр запроса семантически некорректен.
	ErrCodeInvalidParam = "M_INVALID_PARAM"
	// ErrCodeBadAlias - алиас из m.room.canonical_alias не существует или указывает
	// на другую комнату.
	ErrCodeBadAlias = "M_BAD_ALIAS"
	// ErrCodeMissingParam — не хватает обязательного параметра.
	ErrCodeMissingParam = "M_MISSING_PARAM"
	// ErrCodeInvalidSignature — подпись не проверяется (кросс-подписи).
	ErrCodeInvalidSignature = "M_INVALID_SIGNATURE"
	// ErrCodeUnsupportedRoomVersion — версия комнаты вне нашего скоупа.
	ErrCodeUnsupportedRoomVersion = "M_UNSUPPORTED_ROOM_VERSION"
)

// matrixError — тело ошибки в формате Matrix Client-Server API.
type matrixError struct {
	Code    string `json:"errcode"`
	Message string `json:"error"`
}

// writeMatrixError пишет JSON-ошибку Matrix с указанным HTTP-кодом.
func writeMatrixError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, matrixError{Code: code, Message: message})
}

// writeJSON сериализует значение и отправляет его с заданным статусом.
//
// Content-Type - ровно "application/json", как у Synapse: JSON всегда UTF-8 (RFC 8259),
// а Complement сравнивает заголовок целиком и на "; charset=utf-8" валил проверки
// /login, /register и др..
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("api: encode response", "error", err)
	}
}

// isMatrixPath сообщает, относится ли путь к пространству Matrix API.
func isMatrixPath(path string) bool {
	return strings.HasPrefix(path, "/_matrix/")
}

// logRequests логирует каждый запрос структурно и не шумит на пробах.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/health") {
			level = slog.LevelDebug
		}

		// Шаблон маршрута вместо пути: в пути user id, room id,
		// media id; query (access_token) не пишется никогда.
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("route", routePattern(r)),
			slog.Int("status", ww.Status()),
			slog.Int("bytes", ww.BytesWritten()),
			slog.Duration("duration", time.Since(start)),
			slog.String("request_id", middleware.GetReqID(r.Context())),
			// IP клиента: за доверенным прокси - из X-Forwarded-For.
			slog.String("remote_addr", clientIP(r)),
		}

		// Запрос пришел через прокси - адрес соединения отдельным полем.
		if peer := remoteHost(r.RemoteAddr); peer != clientIP(r) {
			attrs = append(attrs, slog.String("proxy_addr", peer))
		}

		s.log.LogAttrs(r.Context(), level, "http request", attrs...)
	})
}

// routePattern - шаблон маршрута chi ("/_matrix/client/v3/profile/{userId}");
// для несуществующих маршрутов - "unmatched", сам путь не логируется.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if pattern := rc.RoutePattern(); pattern != "" {
			return pattern
		}
	}

	return "unmatched"
}

// recoverer перехватывает панику в обработчике, логирует её и отдаёт
// M_UNKNOWN вместо разрыва соединения.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		defer func() {
			if rec := recover(); rec != nil {
				s.log.ErrorContext(ctx, "api: panic in handler",
					"panic", rec,
					"route", routePattern(r),
					"request_id", middleware.GetReqID(ctx),
				)

				writeMatrixError(w, http.StatusInternalServerError, ErrCodeUnknown, "Internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}
