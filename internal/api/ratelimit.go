package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/ratelimit"
)

// ErrCodeLimitExceeded — код Matrix для превышения лимита (429).
const ErrCodeLimitExceeded = "M_LIMIT_EXCEEDED"

// limitKind — класс запроса, для которого применяется свой лимит.
type limitKind string

const (
	limitLoginIP      limitKind = "login_ip"
	limitLoginAccount limitKind = "login_account"
	limitRegisterIP   limitKind = "register_ip"
	limitSend         limitKind = "send"
	limitMediaUpload  limitKind = "media_upload"
	limitSync         limitKind = "sync"
)

// limiters — набор лимитеров, собранный из конфигурации при старте.
type limiters map[limitKind]*ratelimit.Limiter

// toLimiterConfig переводит настройку из конфига в параметры лимитера;
// выключенный лимит превращается в нулевой Config (ratelimit его игнорирует).
func toLimiterConfig(action config.RateLimitAction) ratelimit.Config {
	if !action.Enabled {
		return ratelimit.Config{}
	}

	return ratelimit.Config{PerSecond: action.PerSecond, Burst: action.Burst}
}

// newLimiters создаёт лимитеры по конфигу. Выключенные лимиты не создаются.
func newLimiters(cfg config.RateLimit) limiters {
	out := make(limiters)

	for kind, action := range map[limitKind]config.RateLimitAction{
		limitLoginIP:      cfg.LoginIP,
		limitLoginAccount: cfg.LoginAccount,
		limitRegisterIP:   cfg.RegisterIP,
		limitSend:         cfg.Send,
		limitMediaUpload:  cfg.MediaUpload,
		limitSync:         cfg.Sync,
	} {
		if limiterCfg := toLimiterConfig(action); limiterCfg.Enabled() {
			out[kind] = ratelimit.New(limiterCfg)
		}
	}

	return out
}

// limitErrorBody — тело 429 в формате Matrix (поле retry_after_ms, как в спеке).
type limitErrorBody struct {
	Code         string `json:"errcode"`
	Message      string `json:"error"`
	RetryAfterMS int64  `json:"retry_after_ms"`
}

// rateLimit возвращает middleware, который применяет лимит kind по ключу keyFn.
// Если лимит выключен или keyFn вернул false, запрос пропускается без учёта.
func (s *Server) rateLimit(kind limitKind, keyFn func(*http.Request) (string, bool)) func(http.Handler) http.Handler {
	limiter := s.limiters[kind]

	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := keyFn(r)
			if !ok {
				next.ServeHTTP(w, r)

				return
			}

			decision := limiter.Allow(key)
			if decision.Allowed {
				next.ServeHTTP(w, r)

				return
			}

			s.writeLimitExceeded(w, r, kind, decision.RetryAfter)
		})
	}
}

// writeLimitExceeded отдаёт 429 + M_LIMIT_EXCEEDED и логирует отказ без PII
// (ключ лимита — IP или user id — в лог не попадает).
func (s *Server) writeLimitExceeded(w http.ResponseWriter, r *http.Request, kind limitKind, retryAfter time.Duration) {
	retryMS := retryAfter.Milliseconds()
	if retryMS < 1 {
		retryMS = 1
	}

	w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(float64(retryMS)/1000)), 10))

	s.log.WarnContext(r.Context(), "api: rate limited",
		"limiter", string(kind),
		"path", r.URL.Path,
		"method", r.Method,
		"retry_after_ms", retryMS,
	)

	w.Header().Set("Content-Type", "application/json")
	if s != nil {
		s.rateLimited.Add(1)
	}

	w.WriteHeader(http.StatusTooManyRequests)

	if err := json.NewEncoder(w).Encode(limitErrorBody{
		Code:         ErrCodeLimitExceeded,
		Message:      "Too many requests",
		RetryAfterMS: retryMS,
	}); err != nil {
		s.log.ErrorContext(r.Context(), "api: encode rate limit response", "error", err)
	}
}

// ipKey — ключ лимита по IP клиента (всегда доступен).
func ipKey(r *http.Request) (string, bool) {
	return clientIP(r), true
}

// userOrIPKey — ключ лимита по пользователю из сессии; если сессии нет
// (например, маршрут без requireAuth), лимит применяется по IP.
func userOrIPKey(r *http.Request) (string, bool) {
	if session, ok := sessionFrom(r.Context()); ok {
		return "user:" + session.UserID, true
	}

	return "ip:" + clientIP(r), true
}

// deviceOrIPKey — ключ лимита по устройству пользователя: каждое
// устройство ведет свой /sync, и телефон не должен расходовать лимит ноутбука.
// Без сессии — по IP.
func deviceOrIPKey(r *http.Request) string {
	if session, ok := sessionFrom(r.Context()); ok {
		return "device:" + session.UserID + "|" + session.DeviceID
	}

	return "ip:" + clientIP(r)
}

// syncRateLimit — лимит /sync по устройству. Цель — частый опрос
// без ожидания (timeout=0 в цикле): каждый такой запрос собирает ответ из БД.
//
// Long-poll (timeout > 0) не получает 429: клиент и так готов ждать до
// timeout, поэтому сверх лимита запрос выдерживается до освобождения ведра и
// затем выполняется. Element при частых событиях будит long-poll чаще лимита -
// он получит события чуть позже одной порцией, а не ошибку соединения. Если
// ждать пришлось бы дольше timeout, клиент ушел или сервер останавливается -
// 429 как обычно.
func (s *Server) syncRateLimit(next http.Handler) http.Handler {
	limiter := s.limiters[limitSync]
	if limiter == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := deviceOrIPKey(r)

		decision := limiter.Allow(key)
		if decision.Allowed {
			next.ServeHTTP(w, r)

			return
		}

		// При остановке сервера не ждем: long-poll /sync и так завершается сразу.
		if longPoll := s.syncLongPollTimeout(r); longPoll > 0 && decision.RetryAfter <= longPoll && !s.draining.Load() {
			if waitCtx(r.Context(), decision.RetryAfter) {
				if decision = limiter.Allow(key); decision.Allowed {
					next.ServeHTTP(w, r)

					return
				}
			}
		}

		s.writeLimitExceeded(w, r, limitSync, decision.RetryAfter)
	})
}

// syncLongPollTimeout — timeout long-poll запроса /sync с учетом
// sync.max_timeout_ms; 0 — запрос не ждет (параметр не задан или некорректен).
func (s *Server) syncLongPollTimeout(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("timeout")
	if raw == "" {
		return 0
	}

	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return 0
	}

	if limit := int64(s.cfg.Sync.MaxTimeoutMilliseconds); ms > limit {
		ms = limit
	}

	return time.Duration(ms) * time.Millisecond
}

// waitCtx ждет d; false — контекст запроса отменен раньше.
func waitCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
