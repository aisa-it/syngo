package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aisa-it/syngo/internal/build"
	"github.com/aisa-it/syngo/internal/core"
)

// healthResponse — тело GET /health (liveness) на служебном листенере. На
// публичном - только status: версия и коммит анонимному клиенту не нужны.
type healthResponse struct {
	Status        string  `json:"status"`
	Version       string  `json:"version,omitempty"`
	Commit        string  `json:"commit,omitempty"`
	BuiltAt       string  `json:"built_at,omitempty"`
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
}

// readyResponse — тело GET /health/ready (readiness).
type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// handleHealth - краткий liveness для публичного листенера.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// handleOpsHealth отвечает 200, пока процесс жив, с версией сборки (служебный
// листенер). Зависимости не проверяются — для этого есть readiness.
func (s *Server) handleOpsHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:        "ok",
		Version:       build.Version,
		Commit:        build.Commit,
		BuiltAt:       build.BuildTime,
		UptimeSeconds: time.Since(s.startedAt).Seconds(),
	})
}

// handleReady - readiness для публичного листенера: без текста ошибок
// (анонимный клиент видел ошибку подключения к БД).
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	s.writeReady(w, r, false)
}

// handleOpsReady - readiness с причинами отказа (служебный листенер).
func (s *Server) handleOpsReady(w http.ResponseWriter, r *http.Request) {
	s.writeReady(w, r, true)
}

// writeReady проверяет то, без чего инстанс не может обслуживать запросы:
// PostgreSQL и версию схемы. Очередь федерации в readiness не
// входит: один недоступный пир выводил из балансировки все инстансы; ее
// состояние - метрика syngo_federation_outbox_oldest_seconds и алерт.
func (s *Server) writeReady(w http.ResponseWriter, r *http.Request, detailed bool) {
	status, resp := s.readiness(r.Context(), detailed)

	writeJSON(w, status, resp)
}

// readiness - проверки готовности; общая для проб и метрики syngo_ready.
func (s *Server) readiness(parent context.Context, detailed bool) (int, readyResponse) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()

	status := http.StatusOK
	resp := readyResponse{Status: "ok", Checks: map[string]string{}}

	fail := func(check string, err error) {
		status = http.StatusServiceUnavailable
		resp.Status = "unavailable"
		resp.Checks[check] = "fail"

		if detailed {
			resp.Checks[check] = err.Error()
		}
	}

	// База и схема - core.HealthService.
	if dbErr, schemaErr := s.health.CheckDatabase(ctx, s.schemaVersion); dbErr != nil {
		fail("database", dbErr)
		s.log.WarnContext(ctx, "api: readiness: database", "error", dbErr)
	} else {
		resp.Checks["database"] = "ok"

		if s.schemaVersion > 0 {
			if schemaErr != nil {
				fail("schema", schemaErr)
				s.log.WarnContext(ctx, "api: readiness: schema", "error", schemaErr)
			} else {
				resp.Checks["schema"] = "ok"
			}
		}
	}

	// Без блокировки экземпляра (обрыв соединения с ней) инстанс не принимает
	// трафик: базу мог занять другой экземпляр.
	if s.instanceLockHeld != nil {
		if s.instanceLockHeld() {
			resp.Checks["instance_lock"] = "ok"
		} else {
			fail("instance_lock", errors.New("instance lock is not held"))
		}
	}

	if s.draining.Load() {
		fail("draining", errors.New("shutdown in progress"))
	}

	return status, resp
}

// SetInstanceLock подключает признак удержания блокировки экземпляра (main):
// readiness и метрика syngo_instance_lock_held.
func (s *Server) SetInstanceLock(held func() bool) {
	s.instanceLockHeld = held
}

// SetSchemaVersion задает версию схемы, которую readiness сверяет с БД
// (main: последняя встроенная миграция).
func (s *Server) SetSchemaVersion(want uint) {
	s.schemaVersion = want
}

// versionsResponse — тело GET /_matrix/client/versions.
type versionsResponse struct {
	Versions         []string        `json:"versions"`
	UnstableFeatures map[string]bool `json:"unstable_features"`
}

// capabilitiesResponse — тело GET /_matrix/client/v3/capabilities.
type capabilitiesResponse struct {
	Capabilities map[string]any `json:"capabilities"`
}

// handleCapabilities отдаёт возможности сервера (Element Web запрашивает при старте).
//
// Как в Synapse: только с токеном (401 без него) и с m.change_password - смена
// пароля доступна, если пароли проверяются по локальной базе
// (Complement TestServerCapabilities).
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	// Версия спеки фиксирована core.SpecVersion (v1.11): список не может быть пустым,
	// поэтому локальный фолбэк не нужен — раньше он был мёртвым присваиванием.
	writeJSON(w, http.StatusOK, capabilitiesResponse{Capabilities: map[string]any{
		"m.room_versions": map[string]any{
			"default":   "10",
			"available": map[string]string{"1": "stable", "2": "stable", "3": "stable", "4": "stable", "5": "stable", "6": "stable", "7": "stable", "8": "stable", "9": "stable", "10": "stable", "11": "stable", "12": "stable"},
		},
		"m.change_password": map[string]bool{"enabled": s.cfg.Auth.LocalPasswordsEnabled()},
	}})
}

// handleVersions отдаёт список поддерживаемых версий Client-Server API.
func (s *Server) handleVersions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, versionsResponse{
		Versions:         core.SupportedVersions(),
		UnstableFeatures: core.UnstableFeatures(),
	})
}
