package transport

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"

	"github.com/aisa-it/syngo/internal/ratelimit"
)

// Incoming — ограничения входящих федеративных запросов: размер тела и rate limit.
//
// До проверки подписи origin из X-Matrix ничем не подтвержден: лимит по нему
// позволял бы выбить 429 настоящему серверу. Поэтому до подписи лимит считается
// по IP клиента (Wrap), по origin - только после проверки подписи (AllowOrigin).
type Incoming struct {
	MaxBody int64
	// IPLimiter - лимит до проверки подписи, ключ - IP клиента.
	IPLimiter *ratelimit.Limiter
	// OriginLimiter - лимит после проверки подписи, ключ - проверенный origin.
	OriginLimiter *ratelimit.Limiter
	// ClientIP - IP клиента (в api - с учетом server.trusted_proxies); nil - адрес соединения.
	ClientIP func(*http.Request) string
}

// NewIncoming создаёт ограничитель входящих федеративных запросов.
// perSecond/burst — лимит на IP и на origin (0 — выключено), maxBody — предел тела в байтах.
func NewIncoming(perSecond float64, burst int, maxBody int64) *Incoming {
	in := &Incoming{MaxBody: maxBody}

	if perSecond > 0 && burst > 0 {
		in.IPLimiter = ratelimit.New(ratelimit.Config{PerSecond: perSecond, Burst: burst})
		in.OriginLimiter = ratelimit.New(ratelimit.Config{PerSecond: perSecond, Burst: burst})
	}

	return in
}

// Wrap возвращает middleware с ограничениями входящих запросов до проверки подписи.
//
// Порядок: размер тела (413) → rate limit по IP (429 с retry_after_ms).
func (in *Incoming) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if in.MaxBody > 0 {
			if r.ContentLength > in.MaxBody {
				writeFederationError(w, http.StatusRequestEntityTooLarge, "M_TOO_LARGE", "Request body is too large", 0)

				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, in.MaxBody)
		}

		if !allow(w, in.IPLimiter, in.clientIP(r)) {
			return
		}

		next.ServeHTTP(w, r)
	})
}

// AllowOrigin - лимит по origin после проверки подписи; при превышении пишет 429.
func (in *Incoming) AllowOrigin(w http.ResponseWriter, origin string) bool {
	return allow(w, in.OriginLimiter, origin)
}

func (in *Incoming) clientIP(r *http.Request) string {
	if in.ClientIP != nil {
		return in.ClientIP(r)
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// allow проверяет лимит и пишет 429 с Retry-After и retry_after_ms.
func allow(w http.ResponseWriter, limiter *ratelimit.Limiter, key string) bool {
	if limiter == nil {
		return true
	}

	decision := limiter.Allow(key)
	if decision.Allowed {
		return true
	}

	retryMS := decision.RetryAfter.Milliseconds()
	if retryMS < 1 {
		retryMS = 1
	}

	w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(float64(retryMS)/1000)), 10))
	writeFederationError(w, http.StatusTooManyRequests, "M_LIMIT_EXCEEDED", "Too many requests", retryMS)

	return false
}

// writeFederationError пишет JSON-ошибку в формате Matrix (без тел событий и PII).
func writeFederationError(w http.ResponseWriter, status int, code, message string, retryAfterMS int64) {
	body := map[string]any{"errcode": code, "error": message}
	if retryAfterMS > 0 {
		body["retry_after_ms"] = retryAfterMS
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// retry_after_ms раньше собирался, но не попадал в тело.
	_ = json.NewEncoder(w).Encode(body)
}
