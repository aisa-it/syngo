// Command syncload - нагрузочный стенд /sync (ADR-0005 §8).
//
// Генерирует профиль (пользователи × комнаты × события, состояние комнат и
// участники) прямо в схему сервера и измеряет два уровня:
//
//   - слой данных /sync (core.SyncDataLoader): сценарии initial, empty, delta;
//   - HTTP-обработчик /sync целиком (auth, account data, to-device, ключи, JSON)
//     в том же процессе, без сети: сценарии http-initial, http-empty, http-delta.
//
// Метрики: латентность p50/p95/p99, RPS, SQL-запросов на одну сборку, число
// замеров и ошибок, RSS процесса и железо (CPU, память, версии Go и PostgreSQL).
// Любая ошибка сборки /sync - ненулевой код выхода: при полном отказе раньше
// получался p99=0 и зеленый perf-ratchet.
//
// Примеры:
//
//	syncload -dsn "$PSQL_DSN" -scenario all -rooms 1000 -events-per-room 20
//	make bench-sync
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/dbguard"
	"github.com/aisa-it/syngo/internal/store"
)

// counterKey — ключ контекста, в который кладётся счётчик запросов одного вызова.
type counterKey struct{}

// queryCounter считает SQL-запросы через pgx-трейсер. Счётчик берётся из контекста,
// поэтому при конкурентных вызовах запросы не смешиваются между клиентами.
type queryCounter struct {
	total atomic.Int64
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.total.Add(1)

	if counter, ok := ctx.Value(counterKey{}).(*int64); ok {
		*counter++
	}

	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// profile — параметры генерации данных.
type profile struct {
	ServerName     string
	Users          int
	Rooms          int
	EventsPerRoom  int
	MembersPerRoom int
	// ForeignSenders - сообщения от других участников (непрочитанные).
	ForeignSenders bool
	// LazyMembers - HTTP /sync с фильтром lazy_load_members.
	LazyMembers bool
}

// envSyncloadDSN - DSN бенч-базы.
const envSyncloadDSN = "SYNGO_SYNCLOAD_DSN"

// checkBenchDB пускает генератор только в базу, помеченную как тестовая/бенч,
// если оператор явно не разрешил другую.
func checkBenchDB(dsn string, allowAny bool) error {
	if allowAny {
		return nil
	}

	ok, name, err := dbguard.IsTestDatabase(dsn, "test", "bench", "perf")
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("база %q не похожа на бенч-базу (имя должно содержать test, bench или perf): "+
			"syncload пишет синтетические данные; -allow-any-db, если это осознанно", name)
	}

	return nil
}

// benchUser — пользователь, от имени которого собираем данные.
const benchLocalpart = "bench"

func main() {
	if err := run(); err != nil {
		log.Fatal("syncload: ", err)
	}
}

// dataScenarios и httpScenarios - порядок прогона при -scenario all.
var (
	dataScenarios = []string{"initial", "empty", "delta"}
	httpScenarios = []string{"http-initial", "http-empty", "http-delta"}
)

// selectScenarios разворачивает -scenario в список сценариев.
func selectScenarios(name string) ([]string, error) {
	switch name {
	case "all":
		return append(append([]string{}, dataScenarios...), httpScenarios...), nil
	case "data":
		return dataScenarios, nil
	case "http":
		return httpScenarios, nil
	}

	for _, known := range append(append([]string{}, dataScenarios...), httpScenarios...) {
		if name == known {
			return []string{name}, nil
		}
	}

	return nil, fmt.Errorf("неизвестный сценарий %q", name)
}

// run — тело команды: ошибки возвращаются, чтобы defer db.Close срабатывал.
func run() error {
	// Своя переменная, а не SYNGOD_DATABASE_DSN: syncload пишет бенч-данные и
	// сдвигает общий stream, на боевой базе это порча данных.
	dsn := flag.String("dsn", os.Getenv(envSyncloadDSN), "DSN PostgreSQL (по умолчанию $"+envSyncloadDSN+")")
	anyDB := flag.Bool("allow-any-db", false, "разрешить базу без test/bench/perf в имени")
	scenario := flag.String("scenario", "all",
		"сценарий: initial | empty | delta | http-initial | http-empty | http-delta | data | http | all")
	users := flag.Int("users", 10, "сколько пользователей в профиле")
	rooms := flag.Int("rooms", 1000, "сколько комнат у измеряемого пользователя")
	eventsPerRoom := flag.Int("events-per-room", 20, "сколько событий в комнате")
	membersPerRoom := flag.Int("members-per-room", 5, "участников в комнате (включая измеряемого, не больше users+1)")
	lazy := flag.Bool("lazy-members", false, "HTTP-сценарии с фильтром lazy_load_members")
	foreign := flag.Bool("foreign-senders", false,
		"сообщения от других участников: непрочитанные и счетчики unread_notifications")
	clients := flag.Int("clients", 10, "конкурентных клиентов /sync")
	iterations := flag.Int("iterations", 30, "итераций на клиента")
	perRoomLimit := flag.Int("per-room-limit", 20, "лимит событий на комнату в одной порции")
	outDir := flag.String("out-dir", "bench", "куда писать отчёт")
	jsonOut := flag.String("json", "", "куда писать машинночитаемые метрики (для perf-ratchet в CI)")
	requireEmpty := flag.Bool("require-empty", false, "отказ, если в базе уже есть комнаты (perf-ratchet: данные как у baseline)")
	flag.Parse()

	if *dsn == "" {
		return errors.New("нужен -dsn или " + envSyncloadDSN)
	}

	if err := checkBenchDB(*dsn, *anyDB); err != nil {
		return err
	}

	scenarios, err := selectScenarios(*scenario)
	if err != nil {
		return err
	}

	ctx := context.Background()

	counter := &queryCounter{}

	db, err := store.OpenWithOptions(ctx, store.Options{DSN: *dsn, MaxConns: 16, Tracer: counter})
	if err != nil {
		return fmt.Errorf("подключение: %w", err)
	}

	defer db.Close()

	// Число запросов зависит от формы данных: на базе после прошлого прогона (дельты
	// сверх per-room-limit) initial sync делает лишний батч-запрос состояния. Сравнение
	// с baseline - только на чистой схеме, как в CI.
	if *requireEmpty {
		var rooms int64
		if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM rooms`).Scan(&rooms); err != nil {
			return fmt.Errorf("проверка пустой базы: %w", err)
		}

		if rooms > 0 {
			return fmt.Errorf("в базе уже %d комнат: perf-ratchet сравнивает только на чистой схеме "+
				"(migrate down/up или новая база)", rooms)
		}
	}

	prof := profile{
		ServerName: "bench.local", Users: *users, Rooms: *rooms,
		EventsPerRoom: *eventsPerRoom, MembersPerRoom: min(*membersPerRoom, *users+1),
		ForeignSenders: *foreign,
		LazyMembers:    *lazy,
	}

	if err := generate(ctx, db, prof); err != nil {
		return fmt.Errorf("генерация данных: %w", err)
	}

	syncStore := store.NewSyncStore(db)
	loader := core.NewSyncDataLoader(syncStore)

	position, err := syncStore.StreamPosition(ctx)
	if err != nil {
		return fmt.Errorf("позиция потока: %w", err)
	}

	userID := benchUserID(prof.ServerName)
	hw := detectHardware(ctx, db)

	var httpRunner *httpSync

	metrics := map[string]scenarioMetrics{}

	var report strings.Builder

	fmt.Fprintf(&report, "# Стенд /sync: профиль %d комнат, %d событий на комнату, %d участников, %d клиентов\n\n",
		prof.Rooms, prof.EventsPerRoom, prof.MembersPerRoom, *clients)
	fmt.Fprintf(&report, "- дата: %s\n- пользователь: %s\n- лимит событий на комнату: %d\n- итераций на клиента: %d\n",
		time.Now().UTC().Format(time.RFC3339), userID, *perRoomLimit, *iterations)
	fmt.Fprintf(&report, "- железо: %s\n\n", hw)
	fmt.Fprintf(&report, "| Сценарий | p50, мс | p95, мс | p99, мс | RPS | SQL-запросов на sync | замеров | ошибок |\n")
	fmt.Fprintf(&report, "|---|---|---|---|---|---|---|---|\n")

	var failed []string

	for _, name := range scenarios {
		var call syncCall

		switch name {
		case "initial", "empty", "delta":
			since := position
			if name == "initial" {
				since = 0
			}

			if name == "delta" {
				if err := insertDelta(ctx, db, prof, 10); err != nil {
					return fmt.Errorf("дельта: %w", err)
				}
			}

			call = func(callCtx context.Context) error {
				_, err := loader.Load(callCtx, userID, since, *perRoomLimit)

				return err
			}
		default:
			if httpRunner == nil {
				server := newBenchServer(db, prof.ServerName, *perRoomLimit)
				if httpRunner, err = newHTTPSync(ctx, db, server, prof); err != nil {
					return fmt.Errorf("HTTP /sync: %w", err)
				}
			}

			since := ""
			if name != "http-initial" {
				// Токен на текущую позицию: empty - пустая дельта, delta - 10 новых
				// событий после него.
				if since, err = httpRunner.currentToken(ctx); err != nil {
					return fmt.Errorf("HTTP /sync, токен: %w", err)
				}
			}

			if name == "http-delta" {
				if err := insertDelta(ctx, db, prof, 10); err != nil {
					return fmt.Errorf("дельта: %w", err)
				}
			}

			call = func(callCtx context.Context) error { return httpRunner.sync(callCtx, since) }
		}

		result := runScenario(ctx, call, *clients, *iterations)

		metrics[name] = scenarioMetrics{
			P50MS: result.p50, P95MS: result.p95, P99MS: result.p99,
			RPS: result.rps, SQLPerSync: result.queries, Samples: result.samples, Errors: result.errors,
		}

		fmt.Fprintf(&report, "| %s | %.1f | %.1f | %.1f | %.1f | %.1f | %d | %d |\n",
			name, result.p50, result.p95, result.p99, result.rps, result.queries, result.samples, result.errors)

		fmt.Printf("%-12s p50=%6.1fмс p95=%6.1fмс p99=%6.1fмс rps=%6.1f sql/sync=%.1f samples=%d errors=%d\n",
			name, result.p50, result.p95, result.p99, result.rps, result.queries, result.samples, result.errors)

		if httpRunner != nil && strings.HasPrefix(name, "http-") {
			fmt.Printf("%-12s ответ %d байт\n", name, httpRunner.lastBytes.Load())
		}

		if result.errors > 0 || result.samples == 0 {
			failed = append(failed, fmt.Sprintf("%s: %d ошибок, %d замеров (первая: %v)",
				name, result.errors, result.samples, result.firstErr))
		}
	}

	report.WriteString("\nЦелевой SLO (ADR-0005 §2): incremental p99 ≤ 150 мс, initial p99 ≤ 1.5 с, " +
		"SQL-запросов на sync ≤ 8 и не зависит от числа комнат.\n")

	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return fmt.Errorf("каталог отчёта: %w", err)
	}

	reportPath := filepath.Join(*outDir, fmt.Sprintf("sync-%d-rooms.md", prof.Rooms))
	if err := os.WriteFile(reportPath, []byte(report.String()), 0o600); err != nil {
		return fmt.Errorf("запись отчёта: %w", err)
	}

	if *jsonOut != "" {
		payload, err := json.MarshalIndent(map[string]any{
			"profile": map[string]any{
				"rooms": prof.Rooms, "events_per_room": prof.EventsPerRoom, "clients": *clients,
				"users": prof.Users, "members_per_room": prof.MembersPerRoom,
				"iterations": *iterations, "per_room_limit": *perRoomLimit,
				"foreign_senders": *foreign, "lazy_members": *lazy,
			},
			"hardware":  hw,
			"scenarios": metrics,
			"rss_kb":    rssKB(),
			"generated": time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		if err != nil {
			return fmt.Errorf("json-метрики: %w", err)
		}

		if err := os.WriteFile(*jsonOut, append(payload, '\n'), 0o600); err != nil {
			return fmt.Errorf("запись json-метрик: %w", err)
		}
	}

	fmt.Printf("отчёт: %s\nRSS процесса: %s\n", reportPath, rss())

	if len(failed) > 0 {
		return fmt.Errorf("сборка /sync с ошибками:\n  %s", strings.Join(failed, "\n  "))
	}

	return nil
}

// benchUserID - полный Matrix user_id измеряемого пользователя.
func benchUserID(serverName string) string {
	return "@" + benchLocalpart + ":" + serverName
}

// scenarioMetrics — метрики сценария для perf-ratchet (CI).
type scenarioMetrics struct {
	P50MS      float64 `json:"p50_ms"`
	P95MS      float64 `json:"p95_ms"`
	P99MS      float64 `json:"p99_ms"`
	RPS        float64 `json:"rps"`
	SQLPerSync float64 `json:"sql_per_sync"`
	Samples    int     `json:"samples"`
	Errors     int     `json:"errors"`
}
