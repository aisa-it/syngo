package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aisa-it/syngo/internal/api"
	"github.com/aisa-it/syngo/internal/config"
	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// syncCall - одна сборка /sync; ошибка - сбой сборки (не латентность).
type syncCall func(ctx context.Context) error

// scenarioResult - метрики одного сценария.
type scenarioResult struct {
	p50, p95, p99 float64
	rps           float64
	queries       float64
	samples       int
	errors        int
	firstErr      error
}

// runScenario гоняет конкурентные вызовы и считает метрики. Ошибка вызова не
// останавливает клиента молча: она считается и попадает в результат, а замер
// с ошибкой в перцентили не входит.
func runScenario(ctx context.Context, call syncCall, clients, iterations int) scenarioResult {
	var (
		mu       sync.Mutex
		all      []time.Duration
		queries  int64
		errCount int
		firstErr error
		wg       sync.WaitGroup
	)

	startAll := time.Now()

	for range clients {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range iterations {
				var callQueries int64

				callCtx := context.WithValue(ctx, counterKey{}, &callQueries)
				started := time.Now()
				err := call(callCtx)
				elapsed := time.Since(started)

				mu.Lock()
				if err != nil {
					errCount++
					if firstErr == nil {
						firstErr = err
					}
				} else {
					all = append(all, elapsed)
					queries += callQueries
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	elapsed := time.Since(startAll)

	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	sqlPerSync := 0.0
	if len(all) > 0 {
		sqlPerSync = float64(queries) / float64(len(all))
	}

	return scenarioResult{
		p50:      percentile(all, 50),
		p95:      percentile(all, 95),
		p99:      percentile(all, 99),
		rps:      float64(len(all)) / elapsed.Seconds(),
		queries:  sqlPerSync,
		samples:  len(all),
		errors:   errCount,
		firstErr: firstErr,
	}
}

// percentile считает перцентиль по отсортированному срезу (мс).
func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}

	index := int(p / 100 * float64(len(sorted)))
	if index >= len(sorted) {
		index = len(sorted) - 1
	}

	return float64(sorted[index].Microseconds()) / 1000.0
}

// httpSync гоняет настоящий обработчик GET /sync в процессе (без сети): токен,
// middleware, сборка ответа и JSON те же, что у syngod.
type httpSync struct {
	handler http.Handler
	token   string
	// lastBytes - размер последнего ответа /sync: пропуск тихих комнат виден по
	// объему пустого incremental.
	lastBytes atomic.Int64
	// filter - inline-фильтр запроса ("" - без фильтра).
	filter string
}

// benchDeviceID - устройство измеряемого пользователя для HTTP-сценариев.
const benchDeviceID = "SYNCLOAD"

// newBenchServer - сервер syngod с настройками стенда (без лимитов, свой секрет токенов).
func newBenchServer(db *store.DB, serverName string, perRoomLimit int) *api.Server {
	cfg := config.Default()
	cfg.Matrix.ServerName = serverName
	cfg.RateLimit = config.RateLimit{} // стенд меряет /sync, а не лимитер
	cfg.Sync.TokenSecret = "syncload-token-secret"
	cfg.Sync.PerRoomLimit = perRoomLimit

	return api.New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newHTTPSync заводит устройство и токен измеряемого пользователя для server.
func newHTTPSync(ctx context.Context, db *store.DB, server *api.Server, prof profile) (*httpSync, error) {
	token := "syncload_" + prof.ServerName
	user := benchUserID(prof.ServerName)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO devices (user_id, device_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		user, benchDeviceID); err != nil {
		return nil, fmt.Errorf("устройство: %w", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO access_tokens (token_hash, user_id, device_id) VALUES ($1, $2, $3)
		 ON CONFLICT (token_hash) DO NOTHING`,
		core.HashToken(token), user, benchDeviceID); err != nil {
		return nil, fmt.Errorf("токен: %w", err)
	}

	runner := &httpSync{handler: server.Handler(), token: token}
	if prof.LazyMembers {
		runner.filter = `{"room":{"state":{"lazy_load_members":true}}}`
	}

	// Прогрев вне замера: первый /sync после генерации разбирает синтетику,
	// записанную в обход API (push-действия), - это разовая работа,
	// а не стоимость сценария.
	if _, err := runner.do(ctx, ""); err != nil {
		return nil, fmt.Errorf("прогрев: %w", err)
	}

	return runner, nil
}

// do выполняет GET /sync и возвращает next_batch.
func (h *httpSync) do(ctx context.Context, since string) (string, error) {
	path := "/_matrix/client/v3/sync?timeout=0"
	if h.filter != "" {
		path += "&filter=" + url.QueryEscape(h.filter)
	}

	if since != "" {
		path += "&since=" + url.QueryEscape(since)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+h.token)

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return "", fmt.Errorf("GET /sync: %d %.200s", rec.Code, rec.Body.String())
	}

	h.lastBytes.Store(int64(rec.Body.Len()))

	var resp struct {
		NextBatch string `json:"next_batch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return "", fmt.Errorf("GET /sync: разбор ответа: %w", err)
	}

	if resp.NextBatch == "" {
		return "", fmt.Errorf("GET /sync: нет next_batch в ответе %.200s", rec.Body.String())
	}

	return resp.NextBatch, nil
}

func (h *httpSync) sync(ctx context.Context, since string) error {
	_, err := h.do(ctx, since)

	return err
}

// currentToken - next_batch на текущую позицию (initial sync, вне замера).
func (h *httpSync) currentToken(ctx context.Context) (string, error) {
	return h.do(ctx, "")
}
