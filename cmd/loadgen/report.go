package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/benchenv"
)

// reportFormat - версия формата отчета (растет при несовместимых изменениях).
const reportFormat = 1

// targetInfo - сервер под нагрузкой.
type targetInfo struct {
	BaseURL      string   `json:"base_url"`
	Kind         string   `json:"kind"`
	ServerHeader string   `json:"server_header,omitempty"`
	Versions     []string `json:"versions,omitempty"`
	Build        string   `json:"build,omitempty"`
}

// summary - итог окна замера.
type summary struct {
	totals

	WindowSeconds float64 `json:"window_seconds"`
	Actions       int64   `json:"actions"`
	ActionsRPS    float64 `json:"actions_rps"`
	// ExpectedActionsRPS - частота действий, которую задает профиль.
	ExpectedActionsRPS float64 `json:"expected_actions_rps"`
	ActiveUsers        int     `json:"active_users"`
	Rooms              int     `json:"rooms"`
	Memberships        int     `json:"memberships"`
}

// check - проверка прогона. Kind gate - влияет на код выхода; slo - предложенный
// ориентир (docs/load-testing.md), на код выхода влияет только с -strict-slo.
type check struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
	Limit float64 `json:"limit"`
	// Op - как сравнивается: "<=" или ">=".
	Op string `json:"op"`
	OK bool   `json:"ok"`
}

// report - отчет прогона (JSON) и источник markdown-сводки.
type report struct {
	Tool      string            `json:"tool"`
	Format    int               `json:"format"`
	RunID     string            `json:"run_id"`
	Generated string            `json:"generated"`
	Commit    string            `json:"loadgen_commit"`
	Target    targetInfo        `json:"target"`
	Hardware  benchenv.Hardware `json:"hardware"`
	Profile   Profile           `json:"profile"`
	// Config - параметры стенда, переданные -label (rate_limit, db_max_conns, ...).
	Config         map[string]string         `json:"config"`
	Setup          setupStats                `json:"setup"`
	SetupEndpoints map[string]endpointReport `json:"setup_endpoints"`
	Summary        summary                   `json:"summary"`
	Delivery       deliveryReport            `json:"delivery"`
	Endpoints      map[string]endpointReport `json:"endpoints"`
	Counters       map[string]int64          `json:"counters"`
	Server         serverReport              `json:"server"`
	Process        processReport             `json:"process"`
	Postgres       postgresReport            `json:"postgres"`
	Checks         []check                   `json:"checks"`
	Samples        []sample                  `json:"samples"`
}

// thresholds - пороги проверок.
type thresholds struct {
	maxErrorRate   float64
	minDelivery    float64
	sloDeliveryP95 time.Duration
	sloDeliveryP99 time.Duration
	sloSendP95     time.Duration
}

// buildChecks - проверки по отчету.
func buildChecks(r *report, th thresholds) []check {
	le := func(name, kind string, v, limit float64) check {
		return check{Name: name, Kind: kind, Value: round3(v), Limit: limit, Op: "<=", OK: v <= limit}
	}
	ge := func(name, kind string, v, limit float64) check {
		return check{Name: name, Kind: kind, Value: round3(v), Limit: limit, Op: ">=", OK: v >= limit}
	}

	ms := func(d time.Duration) float64 { return float64(d.Milliseconds()) }

	send := r.Endpoints["send_text"]
	if enc := r.Endpoints["send_encrypted"]; enc.Count > send.Count {
		send = enc
	}

	return []check{
		le("error_rate (без 429)", "gate", r.Summary.ErrorRate, th.maxErrorRate),
		ge("delivery.completeness", "gate", r.Delivery.Completeness, th.minDelivery),
		ge("delivery.messages", "gate", float64(r.Delivery.Messages), 1),
		le("delivery.from_ack.p95_ms", "slo", r.Delivery.FromAck.P95MS, ms(th.sloDeliveryP95)),
		le("delivery.from_ack.p99_ms", "slo", r.Delivery.FromAck.P99MS, ms(th.sloDeliveryP99)),
		le("send.p95_ms", "slo", send.P95MS, ms(th.sloSendP95)),
	}
}

// failed - проверки, которые валят прогон.
func failed(checks []check, strictSLO bool) []check {
	var out []check

	for _, c := range checks {
		if !c.OK && (c.Kind == "gate" || strictSLO) {
			out = append(out, c)
		}
	}

	return out
}

// writeReport пишет report.json и report.md в dir.
func writeReport(dir string, r *report) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // каталог отчетов задает оператор (-out-dir)
		return "", "", fmt.Errorf("отчет: %w", err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("отчет: %w", err)
	}

	jsonPath = filepath.Join(dir, "report.json")
	if err := os.WriteFile(jsonPath, append(data, '\n'), 0o600); err != nil { //nolint:gosec // путь - каталог -out-dir
		return "", "", fmt.Errorf("отчет: %w", err)
	}

	var md strings.Builder

	renderMarkdown(&md, r)

	mdPath = filepath.Join(dir, "report.md")
	if err := os.WriteFile(mdPath, []byte(md.String()), 0o600); err != nil { //nolint:gosec // путь - каталог -out-dir
		return "", "", fmt.Errorf("отчет: %w", err)
	}

	return jsonPath, mdPath, nil
}

func fmtMS(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}

	return fmt.Sprintf("%.1f", v)
}

// renderMarkdown - сводка для человека; полные данные - в report.json.
func renderMarkdown(w io.Writer, r *report) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	p("# Нагрузочный прогон: профиль %s против %s\n\n", r.Profile.Name, r.Target.Kind)
	p("Сгенерировано `cmd/loadgen`, %s. Прогон `%s`, коммит генератора `%s`.\n", r.Generated, r.RunID, r.Commit)
	p("Полные данные (ряд ресурсов, все коды) - `report.json` рядом.\n\n")

	p("## Стенд\n\n| Параметр | Значение |\n|---|---|\n")
	p("| Сервер | %s (%s) |\n", r.Target.BaseURL, r.Target.Kind)

	if r.Target.Build != "" {
		p("| Сборка сервера | %s |\n", r.Target.Build)
	}

	if r.Target.ServerHeader != "" {
		p("| Server | %s |\n", r.Target.ServerHeader)
	}

	p("| Железо | %s |\n", r.Hardware)

	for _, k := range sortedKeys(r.Config) {
		p("| %s | %s |\n", k, r.Config[k])
	}

	pr := r.Profile
	p("\n## Профиль\n\n| Параметр | Значение |\n|---|---|\n")
	p("| Пользователи / комнаты / комнат на пользователя | %d / %d / %d (Ципф %.1f, шифрованных %d%%) |\n",
		pr.Users, pr.Rooms, pr.RoomsPerUser, pr.RoomSkew, pr.EncryptedRoomsPercent)

	if pr.Mode == modeRPS {
		p("| Модель | открытая, %.0f действий/с, max_inflight %d |\n", pr.TargetRPS, pr.MaxInflight)
	} else {
		p("| Модель | закрытая, think time %s (≈%.0f действий/с) |\n", pr.ThinkTime.D(), pr.expectedActionRate())
	}

	p("| Ramp / warmup / замер / drain | %s / %s / %s / %s |\n", pr.Ramp.D(), pr.Warmup.D(), pr.Duration.D(), pr.Drain.D())
	p("| /sync | timeout %s, timeline %d, lazy_load_members %t |\n", pr.SyncTimeout.D(), pr.TimelineLimit, pr.LazyLoadMembers)

	weights := make([]string, 0, len(pr.Weights))
	for _, wt := range pr.sortedWeights() {
		weights = append(weights, fmt.Sprintf("%s %.0f", wt.name, wt.weight))
	}

	p("| Веса действий | %s |\n", strings.Join(weights, ", "))

	st := r.Setup
	p("\n## Подготовка\n\n")
	p("%.1f с: пользователей %d (зарегистрировано %d, вход %d, из кеша %d), комнат создано %d, переиспользовано %d, "+
		"входов %d, членств %d (самая большая комната %d, самая маленькая %d).\n",
		st.Seconds, st.Users, st.Registered, st.LoggedIn, st.Cached, st.RoomsCreated, st.RoomsReused,
		st.Joins, st.Memberships, st.LargestRoom, st.SmallestRoom)

	s := r.Summary
	p("\n## Итог окна замера\n\n| Метрика | Значение |\n|---|---|\n")
	p("| Окно | %.1f с |\n", s.WindowSeconds)
	p("| Действий | %d (%.1f/с, профиль %.1f/с) |\n", s.Actions, s.ActionsRPS, s.ExpectedActionsRPS)
	p("| Запросов | %d (%.1f/с) |\n", s.Requests, s.RPS)
	p("| Ошибок (не 2xx и сеть) | %d, из них 429: %d; доля без 429: %.4f |\n", s.Errors, s.RateLimited, s.ErrorRate)
	p("| Комнат / членств к концу | %d / %d |\n", s.Rooms, s.Memberships)

	for _, k := range sortedKeys(r.Counters) {
		if !strings.HasPrefix(k, "action") {
			p("| %s | %d |\n", k, r.Counters[k])
		}
	}

	d := r.Delivery
	p("\n## Доставка сообщений\n\n")
	p("От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).\n\n")
	p("| Метрика | p50 | p95 | p99 | max | замеров |\n|---|---|---|---|---|---|\n")
	p("| от ответа PUT, мс | %s | %s | %s | %s | %d |\n", fmtMS(d.FromAck.P50MS), fmtMS(d.FromAck.P95MS),
		fmtMS(d.FromAck.P99MS), fmtMS(d.FromAck.MaxMS), d.FromAck.Count)
	p("| от начала PUT, мс | %s | %s | %s | %s | %d |\n", fmtMS(d.FromSendStart.P50MS), fmtMS(d.FromSendStart.P95MS),
		fmtMS(d.FromSendStart.P99MS), fmtMS(d.FromSendStart.MaxMS), d.FromSendStart.Count)
	p("\nСообщений %d, ожидалось доставок %d, доставлено %d (полнота %.3f), раньше ответа отправителю %d, "+
		"сообщений с недоставкой %d.\n", d.Messages, d.Expected, d.Delivered, d.Completeness, d.BeforeAck, d.Undelivered)

	p("\n## Эндпоинты (клиент)\n\n")
	p("Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.\n\n")
	p("| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |\n|---|---|---|---|---|---|---|---|---|---|\n")

	for _, name := range sortedEndpoints(r.Endpoints) {
		ep := r.Endpoints[name]
		p("| %s | %d | %.2f | %s | %s | %s | %s | %d | %d | %s |\n", name, ep.Count, ep.RPS,
			fmtMS(ep.P50MS), fmtMS(ep.P95MS), fmtMS(ep.P99MS), fmtMS(ep.MaxMS), ep.Errors, ep.RateLimited, statusList(ep.Statuses))
	}

	renderServer(w, r)

	p("\n## Проверки\n\n| Проверка | Тип | Значение | Порог | Итог |\n|---|---|---|---|---|\n")

	for _, c := range r.Checks {
		verdict := "ok"
		if !c.OK {
			verdict = "НЕ ПРОЙДЕНА"
		}

		kind := "гейт"
		if c.Kind == "slo" {
			kind = "SLO (предложение)"
		}

		p("| %s | %s | %g | %s %g | %s |\n", c.Name, kind, c.Value, c.Op, c.Limit, verdict)
	}
}

// renderServer - метрики сервера, процесса и PostgreSQL.
func renderServer(w io.Writer, r *report) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	srv := r.Server
	p("\n## Сервер\n\n")

	switch {
	case srv.MetricsURL == "":
		p("/metrics не задан (-metrics-url).\n")
	case srv.Error != "":
		p("/metrics: %s\n", srv.Error)
	}

	if srv.Kind == "syngo" {
		p("| Метрика | Значение |\n|---|---|\n")
		p("| /sync без long-poll p50 / p95 / p99, мс | %s / %s / %s |\n", fmtMS(srv.SyncP50MS), fmtMS(srv.SyncP95MS), fmtMS(srv.SyncP99MS))
		p("| 429 (syngo_rate_limited_total) | %.0f |\n", srv.RateLimited)
		p("| Пул БД: выдач с ожиданием / ожидание, с / предел | %.0f / %.3f / %.0f |\n",
			srv.DBPoolEmptyAcquires, srv.DBPoolWaitSeconds, srv.DBPoolMax)
		p("| GC: циклов / паузы, с / куча в конце, МиБ | %.0f / %.3f / %.1f |\n", srv.GCCycles, srv.GCPauseSeconds, srv.HeapEndBytes/(1<<20))
		p("| Макс. long-poll / горутин | %.0f / %.0f |\n", srv.LongpollWaitersMax, srv.GoroutinesMax)
		p("| Коды ответов | %s |\n", floatList(srv.Status))
	}

	if srv.Kind == "synapse" {
		p("Метрики Synapse (synapse_http_server_*): коды ответов %s.\n", floatList(srv.Status))
	}

	if len(srv.Requests) > 0 {
		p("\nМаршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):\n\n")
		p("| Маршрут | Запросов | p95, мс | p99, мс |\n|---|---|---|---|\n")

		routes := make([]string, 0, len(srv.Requests))
		for k := range srv.Requests {
			routes = append(routes, k)
		}

		sort.Slice(routes, func(i, j int) bool { return srv.Requests[routes[i]] > srv.Requests[routes[j]] })

		for _, route := range routes[:min(len(routes), 15)] {
			p("| %s | %.0f | %s | %s |\n", route, srv.Requests[route], fmtMS(srv.RouteP95MS[route]), fmtMS(srv.RouteP99MS[route]))
		}
	}

	pc := r.Process
	p("\n## Процесс сервера\n\n")

	switch {
	case pc.PID == 0:
		p("PID не задан (-server-pid): RSS и CPU не сняты.\n")
	case pc.Error != "":
		p("PID %d: %s\n", pc.PID, pc.Error)
	default:
		p("PID %d: RSS %d -> %d кБ (макс. %d), CPU %.1f с за окно (в среднем %.0f%%, пик %.0f%% одного ядра), потоков до %d.\n",
			pc.PID, pc.RSSStartKB, pc.RSSEndKB, pc.RSSMaxKB, pc.CPUSeconds, pc.CPUAvgPercent, pc.CPUMaxPercent, pc.ThreadsMax)
	}

	pg := r.Postgres
	p("\n## PostgreSQL\n\n")

	switch {
	case pg.Version == "" && pg.Error == "":
		p("DSN не задан (-pg-dsn).\n")

		return
	case pg.Version == "":
		p("%s\n", pg.Error)

		return
	}

	p("%s. TPS %.1f, cache hit %.3f, соединений базы до %d (активных до %d), размер %.1f -> %.1f МиБ.\n\n",
		firstWords(pg.Version, 2), pg.TPS, pg.CacheHitRatio, pg.ConnsMax, pg.ActiveMax, pg.DBSizeStartMB, pg.DBSizeEndMB)

	counters := make([]string, 0, len(pg.Counters))
	for _, k := range pgDatabaseCounters {
		counters = append(counters, fmt.Sprintf("%s %d", k, pg.Counters[k]))
	}

	p("pg_stat_database за окно: %s.\n\n", strings.Join(counters, ", "))

	if pg.StatementsError != "" {
		p("pg_stat_statements недоступен: %s\n", pg.StatementsError)

		return
	}

	p("| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |\n|---|---|---|---|\n")

	for _, st := range pg.Statements[:min(len(pg.Statements), 10)] {
		p("| `%s` | %d | %.0f | %.2f |\n", truncate(strings.ReplaceAll(st.Query, "|", "\\|"), 120), st.Calls, st.TotalMS, st.MeanMS)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func statusList(m map[string]int64) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s:%d", k, m[k]))
	}

	return strings.Join(parts, " ")
}

func floatList(m map[string]float64) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s:%.0f", k, m[k]))
	}

	return strings.Join(parts, " ")
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)

	return strings.Join(f[:min(n, len(f))], " ")
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}

	return string([]rune(s)[:n]) + "…"
}
