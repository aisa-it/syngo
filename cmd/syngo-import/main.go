// Command syngo-import импортирует JSONL-дамп Synapse в схему SYNGO (ADR-0006, PoC).
//
// Формат дампа — по строке на объект: user, room, event, membership, media.
// Экспортёр для Synapse лежит в tools/synapse-export/export.sql.
//
//	syngo-import -dsn "$PSQL_DSN" -file synapse-dump.jsonl
//	syngo-import -dsn "$PSQL_DSN" -file dump.jsonl -dry-run   # посчитать строки без записи
//
// Команда идемпотентна: повторный импорт того же дампа не создаёт дублей. Дамп
// читается потоком; битая строка попадает в отчет, импорт идет дальше; код
// возврата ненулевой, если в отчете есть ошибки. События (kind=event)
// пишутся тем же импортером пачками, что и в syngo-import-events, включая
// подтягивание sequence stream_ordering.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/importevents"
	"github.com/aisa-it/syngo/internal/store"
)

// dumpRecord — одна строка дампа. Поля общие для всех kind, лишние игнорируются.
// line — номер строки в файле (для отчёта об ошибках, в JSON не участвует).
type dumpRecord struct {
	Kind        string                 `json:"kind"`
	UserID      string                 `json:"user_id"`
	Localpart   string                 `json:"localpart"`
	Password    string                 `json:"password_hash"`
	Display     string                 `json:"display_name"`
	Deactivated bool                   `json:"deactivated"`
	RoomID      string                 `json:"room_id"`
	RoomVersion string                 `json:"room_version"`
	Creator     string                 `json:"creator"`
	IsPublic    bool                   `json:"is_public"`
	EventID     string                 `json:"event_id"`
	Type        string                 `json:"type"`
	Sender      string                 `json:"sender"`
	StateKey    *string                `json:"state_key"`
	Content     json.RawMessage        `json:"content"`
	OriginTS    int64                  `json:"origin_server_ts"`
	Depth       int64                  `json:"depth"`
	PrevEvents  importevents.EventRefs `json:"prev_events,omitempty"`
	Stream      *int64                 `json:"stream_ordering"`
	Membership  string                 `json:"membership"`
	Forgotten   bool                   `json:"forgotten"`
	Alias       string                 `json:"alias"`
	MediaID     string                 `json:"media_id"`
	MediaType   string                 `json:"media_type"`
	MediaLen    int64                  `json:"media_length"`
	UploadName  string                 `json:"upload_name"`
	CreatedAt   int64                  `json:"created_at_ms"`
	// Quarantined, URLCache - карантин и превью ссылки медиа.
	Quarantined *string `json:"quarantined_by"`
	URLCache    *string `json:"url_cache"`

	// Пользователь: профиль и флаги Synapse.
	AvatarURL    string  `json:"avatar_url"`
	Admin        bool    `json:"admin"`
	Locked       bool    `json:"locked"`
	Suspended    bool    `json:"suspended"`
	ShadowBanned bool    `json:"shadow_banned"`
	AppserviceID *string `json:"appservice_id"`

	// Устройства, токены, E2EE, бэкап ключей, настройки клиента (userdata.go).
	DeviceID        string          `json:"device_id"`
	LastSeenMS      *int64          `json:"last_seen_ms"`
	LastSeenIP      *string         `json:"last_seen_ip"`
	Token           string          `json:"token"`
	RefreshToken    *string         `json:"refresh_token"`
	ValidUntilMS    *int64          `json:"valid_until_ms"`
	LastValidatedMS *int64          `json:"last_validated_ms"`
	KeyJSON         json.RawMessage `json:"key_json"`
	Algorithm       string          `json:"algorithm"`
	KeyID           string          `json:"key_id"`
	Used            bool            `json:"used"`
	KeyType         string          `json:"key_type"`
	TargetUserID    string          `json:"target_user_id"`
	TargetDeviceID  string          `json:"target_device_id"`
	Signature       string          `json:"signature"`
	Version         string          `json:"version"`
	AuthData        json.RawMessage `json:"auth_data"`
	Deleted         bool            `json:"deleted"`
	Etag            string          `json:"etag"`
	SessionID       string          `json:"session_id"`
	FirstMsgIndex   int64           `json:"first_message_index"`
	ForwardedCount  int64           `json:"forwarded_count"`
	IsVerified      bool            `json:"is_verified"`
	SessionData     json.RawMessage `json:"session_data"`
	Scope           string          `json:"scope"`
	RuleKind        string          `json:"rule_kind"`
	RuleName        string          `json:"rule_name"`
	Priority        *int            `json:"priority"`
	DefaultActions  bool            `json:"default_actions"`
	Enabled         *bool           `json:"enabled"`
	Conditions      json.RawMessage `json:"conditions"`
	Actions         json.RawMessage `json:"actions"`
	AppID           string          `json:"app_id"`
	Pushkey         string          `json:"pushkey"`
	PusherKind      string          `json:"pusher_kind"`
	AppDisplayName  string          `json:"app_display_name"`
	DeviceDisplay   string          `json:"device_display_name"`
	ProfileTag      string          `json:"profile_tag"`
	Lang            string          `json:"lang"`
	Data            json.RawMessage `json:"data"`
	FilterID        string          `json:"filter_id"`
	Filter          json.RawMessage `json:"filter"`
	Message         json.RawMessage `json:"message"`
	SourceStreamID  *int64          `json:"stream_id"`
	Stats           map[string]int  `json:"stats"`
	// AuthProvider, ExternalID - привязка учетки SSO (user_external_ids).
	AuthProvider string `json:"auth_provider"`
	ExternalID   string `json:"external_id"`
	// ReceiptType, ThreadID, TS - отметка прочтения (receipts_linearized).
	ReceiptType string `json:"receipt_type"`
	ThreadID    string `json:"thread_id"`
	TS          int64  `json:"ts"`

	// line — номер строки дампа (для отчёта об ошибках), в JSON не участвует.
	line int
}

// writeReport пишет отчёт импорта в JSON (файл пригоден для приемки миграции).
func writeReport(path string, report importReport) error {
	if path == "" {
		return nil
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("отчёт: %w", err)
	}

	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("запись отчёта %s: %w", path, err)
	}

	fmt.Printf("отчёт: %s\n", path)

	return nil
}

// counters — итоги импорта для отчёта.
type counters struct {
	Users int `json:"users"`
	Rooms int `json:"rooms"`
	// RoomVersions - комнаты по версиям (проверка источника до переключения).
	RoomVersions map[string]int `json:"room_versions,omitempty"`
	Aliases      int            `json:"aliases"`
	Events       int            `json:"events"`
	Memberships  int            `json:"memberships"`
	Media        int            `json:"media"`
	Skipped      int            `json:"skipped"`
	// Updated - повторный импорт обновил существующую строку.
	Updated int `json:"updated"`
	// Данные пользователей: вставлено строк каждого вида.
	Devices        int `json:"devices"`
	AccessTokens   int `json:"access_tokens"`
	DeviceKeys     int `json:"device_keys"`
	OneTimeKeys    int `json:"one_time_keys"`
	FallbackKeys   int `json:"fallback_keys"`
	CrossSigning   int `json:"cross_signing_keys"`
	Signatures     int `json:"signatures"`
	RoomKeyVersion int `json:"room_keys_versions"`
	RoomKeys       int `json:"room_keys"`
	AccountData    int `json:"account_data"`
	PushRules      int `json:"push_rules"`
	Pushers        int `json:"pushers"`
	Filters        int `json:"filters"`
	ToDevice       int `json:"to_device"`
	// ExternalIDs - привязки учеток SSO.
	ExternalIDs int `json:"external_ids"`
	// Receipts - отметки прочтения m.read и m.read.private.
	Receipts int `json:"receipts"`
	// LockedAsDeactivated - заблокированные (locked) в Synapse перенесены деактивированными:
	// блокировки в SYNGO нет, а оставлять их активными нельзя.
	LockedAsDeactivated int `json:"locked_as_deactivated"`
	// Suspended, ShadowBanned - флаги без аналога в SYNGO: перенесены как обычные (см. отчет).
	Suspended    int `json:"suspended_unsupported"`
	ShadowBanned int `json:"shadow_banned_unsupported"`
}

// importError — ошибка на конкретной строке дампа (импорт продолжается).
type importError struct {
	Line  int    `json:"line"`
	Kind  string `json:"kind"`
	Error string `json:"error"`
}

// importReport — отчёт импорта: счётчики, ошибки и время (пригоден для приемки миграции).
type importReport struct {
	StartedAt  string        `json:"started_at"`
	FinishedAt string        `json:"finished_at"`
	DurationMS int64         `json:"duration_ms"`
	Counts     counters      `json:"counts"`
	Errors     []importError `json:"errors"`
	// Verify - сверка с источником по пользователям (флаг -verify).
	Verify *verifyReport `json:"verify,omitempty"`
}

// main — тонкая обёртка: ошибки возвращает run, чтобы defer закрыл пул.
func main() {
	if err := run(); err != nil {
		log.Fatal("syngo-import: ", err)
	}
}

func run() error {
	dsn := flag.String("dsn", os.Getenv("SYNGOD_DATABASE_DSN"), "DSN PostgreSQL")
	file := flag.String("file", "", "JSONL-дамп Synapse")
	dryRun := flag.Bool("dry-run", false, "только прочитать дамп, ничего не писать")
	limit := flag.Int("limit", 0, "импортировать не больше N записей (0 — все)")
	serverName := flag.String("server-name", "", "ожидаемое имя сервера: строки других серверов пропускаются")
	report := flag.String("report", "import-report.json", "файл отчёта об импорте")
	batch := flag.Int("batch", 500, "размер батча обработки (для прогресса в логе)")
	verify := flag.Bool("verify", false, "после импорта сверить счетчики по пользователям с источником (user_stats)")
	verifyOnly := flag.Bool("verify-only", false, "только сверка с источником, без импорта")
	retention := flag.Duration("redaction-retention", 7*24*time.Hour,
		"сколько хранить оригинал удаленных событий (как syngo-import-events; 0 - не хранить)")
	flag.Parse()

	if *dsn == "" && !*dryRun {
		return errors.New("нужен -dsn или SYNGOD_DATABASE_DSN")
	}

	if *file == "" {
		return errors.New("нужен -file с JSONL-дампом")
	}

	if *dryRun {
		var counts counters

		total, lineErrors, err := scanDump(*file, *limit, func(record *dumpRecord, _ []byte) error {
			countRecord(&counts, record, *serverName)

			return nil
		})
		if err != nil {
			return err
		}

		fmt.Printf("dry-run: %d записей — users=%d rooms=%d aliases=%d events=%d memberships=%d media=%d, пропущено=%d, битых строк=%d\n",
			total, counts.Users, counts.Rooms, counts.Aliases, counts.Events,
			counts.Memberships, counts.Media, counts.Skipped, len(lineErrors))
		(&importer{counts: counts}).printUserData()
		fmt.Printf("версии комнат: %s\n", counts.roomVersionsText())

		if len(lineErrors) > 0 {
			return fmt.Errorf("в дампе %d битых строк", len(lineErrors))
		}

		if bad := counts.unsupportedRoomVersions(); len(bad) > 0 {
			return fmt.Errorf("в дампе комнаты неподдерживаемых версий %s: они не перенесутся", strings.Join(bad, ", "))
		}

		return nil
	}

	db, err := store.Open(context.Background(), *dsn, 8, 0)
	if err != nil {
		return fmt.Errorf("подключение: %w", err)
	}

	defer db.Close()

	// Оригинал цели redaction сохраняется так же, как в syngo-import-events: иначе
	// обратная выгрузка и повторный импорт теряли бы его.
	db.SetRedactionRetention(*retention)

	importer := newImporter(db, *serverName, *batch)

	started := time.Now()

	if *verifyOnly {
		err = importer.collectStats(*file, *limit)
	} else {
		err = importer.importFile(context.Background(), *file, *limit)
	}

	reportData := importReport{
		StartedAt: started.UTC().Format(time.RFC3339),
		Counts:    importer.counts,
		Errors:    importer.errors,
	}

	if err == nil && (*verify || *verifyOnly) {
		reportData.Verify, err = verifyUsers(context.Background(), db, importer.userStats)
	}

	reportData.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	reportData.DurationMS = time.Since(started).Milliseconds()

	if writeErr := writeReport(*report, reportData); writeErr != nil {
		return writeErr
	}

	if err != nil {
		return err
	}

	if reportData.Verify != nil {
		reportData.Verify.print()

		if len(reportData.Verify.Mismatches) > 0 {
			return fmt.Errorf("сверка: расхождений %d (см. %s)", len(reportData.Verify.Mismatches), *report)
		}
	}

	// Раньше код возврата был 0 при любом числе ошибок: сбой импорта не останавливал
	// runbook.
	if len(importer.errors) > 0 {
		return fmt.Errorf("импорт завершён с %d ошибками (см. %s)", len(importer.errors), *report)
	}

	return nil
}

// scanDump читает JSONL потоком и вызывает fn для каждой записи; пустые строки и
// комментарии пропускаются. Битая строка не прерывает чтение (README обещает
// продолжение): она возвращается в списке ошибок строк. Ошибка fn прерывает чтение.
func scanDump(path string, limit int, fn func(record *dumpRecord, raw []byte) error) (int, []importError, error) {
	file, err := os.Open(path) //nolint:gosec // путь задаёт оператор миграции
	if err != nil {
		return 0, nil, fmt.Errorf("открыть дамп: %w", err)
	}

	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var (
		total      int
		lineErrors []importError
	)

	for line := 1; scanner.Scan(); line++ {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}

		var record dumpRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			lineErrors = append(lineErrors, importError{Line: line, Error: err.Error()})

			continue
		}

		if record.Kind == "" {
			lineErrors = append(lineErrors, importError{Line: line, Error: "нет поля kind"})

			continue
		}

		record.line = line
		total++

		if err := fn(&record, []byte(raw)); err != nil {
			return total, lineErrors, err
		}

		if limit > 0 && total >= limit {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return total, lineErrors, fmt.Errorf("чтение дампа: %w", err)
	}

	return total, lineErrors, nil
}

// countRecord считает записи для dry-run (без обращения к БД).
func countRecord(counts *counters, record *dumpRecord, serverName string) {
	// События фильтр сервера не проходят: их отбирает комната (как при импорте).
	if record.Kind != "event" && serverName != "" && !belongsToServer(record, serverName) {
		counts.Skipped++

		return
	}

	switch record.Kind {
	case "user":
		counts.Users++
	case "room":
		counts.Rooms++

		version := record.RoomVersion
		if version == "" {
			version = "10"
		}

		counts.countRoomVersion(version)
	case "room_alias":
		counts.Aliases++
	case "event":
		counts.Events++
	case "membership":
		counts.Memberships++
	case "media":
		if isURLCache(record) {
			counts.Skipped++
		} else {
			counts.Media++
		}
	case "user_stats":
	default:
		if counter := counts.userDataCounter(record.Kind); counter != nil {
			*counter++
		} else {
			counts.Skipped++
		}
	}
}

// roomVersionSupported - версия комнаты, правила которой у нас есть: числовые 1-12
// (v1-v9 переносятся историей, v10-v12 - полностью).
func roomVersionSupported(version string) bool {
	number, err := strconv.Atoi(version)

	return err == nil && number >= 1 && number <= core.MaxRoomVersion
}

// countRoomVersion считает комнату по версии.
func (c *counters) countRoomVersion(version string) {
	if c.RoomVersions == nil {
		c.RoomVersions = map[string]int{}
	}

	c.RoomVersions[version]++
}

// roomVersionsText - «10=3 12=2» в порядке версий.
func (c *counters) roomVersionsText() string {
	if len(c.RoomVersions) == 0 {
		return "-"
	}

	versions := make([]string, 0, len(c.RoomVersions))
	for version := range c.RoomVersions {
		versions = append(versions, version)
	}

	sort.Slice(versions, func(i, j int) bool {
		a, errA := strconv.Atoi(versions[i])
		b, errB := strconv.Atoi(versions[j])

		if errA == nil && errB == nil {
			return a < b
		}

		return errA == nil || errB != nil && versions[i] < versions[j]
	})

	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, fmt.Sprintf("%s=%d", version, c.RoomVersions[version]))
	}

	return strings.Join(parts, " ")
}

// unsupportedRoomVersions - версии из дампа, комнаты которых не перенесутся.
func (c *counters) unsupportedRoomVersions() []string {
	var out []string

	for version, rooms := range c.RoomVersions {
		if !roomVersionSupported(version) {
			out = append(out, fmt.Sprintf("%q (%d)", version, rooms))
		}
	}

	sort.Strings(out)

	return out
}

// isURLCache - медиа превью ссылки (url_cache Synapse): не переносится.
func isURLCache(record *dumpRecord) bool {
	return record.URLCache != nil && *record.URLCache != ""
}

// quarantinedBy - карантин медиа для вставки (NULL - нет карантина).
func quarantinedBy(record *dumpRecord) any {
	if record.Quarantined == nil || *record.Quarantined == "" {
		return nil
	}

	return *record.Quarantined
}

// belongsToServer проверяет, что сущность относится к нашему серверу. room_id
// комнаты v12 домена не содержит (хеш m.room.create): такая комната своя по
// создателю, а не по суффиксу room_id.
func belongsToServer(record *dumpRecord, serverName string) bool {
	suffix := ":" + serverName

	for _, value := range []string{record.UserID, record.RoomID, record.Sender, record.Creator} {
		if value == "" || value == record.RoomID && auth.IsCreateHashRoomID(value) {
			continue
		}

		if !strings.HasSuffix(value, suffix) {
			return false
		}
	}

	return true
}

// importer пишет записи дампа в схему SYNGO.
type importer struct {
	db         *store.DB
	serverName string
	batchSize  int
	counts     counters
	errors     []importError
	// events - импорт событий пачками (общий с syngo-import-events): раньше здесь
	// была своя вставка по строке без подтягивания sequence, и после импорта по
	// README новые сообщения не попадали в инкрементальный /sync.
	events     *importevents.Session
	eventsSeen bool
	// userStats - строки user_stats дампа для сверки (-verify).
	userStats []dumpRecord
	// Порядковые номера одинаковых to_device в старом дампе без stream_id.
	legacyInboxOccurrences map[string]int64
}

func newImporter(db *store.DB, serverName string, batchSize int) *importer {
	if batchSize <= 0 {
		batchSize = 500
	}

	im := &importer{db: db, serverName: serverName, batchSize: batchSize}
	im.events = importevents.New(importevents.NewStoreWriter(db), importevents.Options{
		BatchSize: batchSize, ProgressEvery: -1,
		OnBad: func(line int64, msg string) {
			im.errors = append(im.errors, importError{Line: int(line), Kind: "event", Error: msg})
		},
	}).Begin()

	return im
}

// importFile импортирует дамп потоком в порядке файла: пользователи → комнаты →
// алиасы → события → членство → медиа. Ошибка на строке не останавливает импорт:
// она попадает в отчёт, а процесс идёт дальше; ошибка
// записи событий (отказ БД) - останавливает.
func (im *importer) importFile(ctx context.Context, path string, limit int) error {
	processed := 0
	im.legacyInboxOccurrences = nil

	_, lineErrors, err := scanDump(path, limit, func(record *dumpRecord, raw []byte) error {
		processed++

		if err := im.importRecord(ctx, record, raw); err != nil {
			return err
		}

		if processed%im.batchSize == 0 {
			fmt.Printf("прогресс: %d записей, ошибок %d\n", processed, len(im.errors))
		}

		return nil
	})

	im.errors = append(im.errors, lineErrors...)

	if err != nil {
		return err
	}

	if err := im.finishEvents(ctx); err != nil {
		return err
	}

	if err := im.finishUserData(ctx); err != nil {
		im.errors = append(im.errors, importError{Kind: "room_keys_current", Error: err.Error()})
	}

	im.printUserData()

	return nil
}

// collectStats читает из дампа только user_stats (-verify-only).
func (im *importer) collectStats(path string, limit int) error {
	_, lineErrors, err := scanDump(path, limit, func(record *dumpRecord, _ []byte) error {
		if record.Kind == "user_stats" {
			im.userStats = append(im.userStats, *record)
		}

		return nil
	})

	im.errors = append(im.errors, lineErrors...)

	return err
}

// importRecord пишет одну запись. Возвращает только ошибку, после которой
// продолжать нельзя; ошибки строки уходят в отчет.
func (im *importer) importRecord(ctx context.Context, record *dumpRecord, raw []byte) error {
	if record.Kind == "event" || record.Kind == "state" || record.Kind == "manifest" {
		im.eventsSeen = true

		return im.events.Line(ctx, int64(record.line), raw)
	}

	// Членство ссылается на события (FK): накопленная пачка пишется раньше.
	if err := im.events.Flush(ctx); err != nil {
		return err
	}

	if im.serverName != "" && !belongsToServer(record, im.serverName) {
		im.counts.Skipped++

		return nil
	}

	var err error

	switch record.Kind {
	case "user":
		err = im.importUser(ctx, record)
	case "room":
		err = im.importRoom(ctx, record)
	case "room_alias":
		err = im.importAlias(ctx, record)
	case "membership":
		err = im.importMembership(ctx, record)
	case "media":
		err = im.importMedia(ctx, record)
	case "user_stats":
		im.userStats = append(im.userStats, *record)

		return nil
	default:
		handled, userErr := im.importUserData(ctx, record)
		if !handled {
			im.counts.Skipped++

			return nil
		}

		err = userErr
	}

	if err != nil {
		im.errors = append(im.errors, importError{Line: record.line, Kind: record.Kind, Error: err.Error()})
	}

	return nil
}

// finishEvents дописывает события, подтягивает sequence и материализует room_state.
func (im *importer) finishEvents(ctx context.Context) error {
	report, err := im.events.Finish(ctx)
	if err != nil {
		return err
	}

	im.counts.Events += int(report.Inserted)
	im.counts.Skipped += int(report.Skipped)

	// event_id, не совпавший с reference hash PDU, - ошибка, как в syngo-import-events:
	// так видно неверные правила redaction и event_id версии комнаты.
	if report.IDMismatch > 0 {
		im.errors = append(im.errors, importError{Kind: "event", Error: fmt.Sprintf(
			"event_id не совпал с reference hash PDU: %d (%s)", report.IDMismatch, strings.Join(report.Errors, "; "))})
	}

	// Состояние из kind=state (current_state_events) точнее эвристики.
	if im.eventsSeen && report.StateRooms == 0 {
		if _, err := im.db.RecomputeRoomState(ctx, 100); err != nil {
			return fmt.Errorf("материализация room_state: %w", err)
		}
	}

	fmt.Printf("импорт: users=%d rooms=%d aliases=%d events=%d memberships=%d media=%d, пропущено=%d, ошибок=%d\n",
		im.counts.Users, im.counts.Rooms, im.counts.Aliases, im.counts.Events,
		im.counts.Memberships, im.counts.Media, im.counts.Skipped, len(im.errors))
	fmt.Printf("версии комнат: %s\n", im.counts.roomVersionsText())

	return nil
}

// importAlias переносит алиас комнаты.
func (im *importer) importAlias(ctx context.Context, record *dumpRecord) error {
	if record.Alias == "" || record.RoomID == "" {
		return fmt.Errorf("room_alias без alias/room_id")
	}

	const query = `
		INSERT INTO room_aliases (room_alias, room_id, creator)
		VALUES ($1, $2, $3)
		ON CONFLICT (room_alias) DO NOTHING`

	tag, err := im.db.Pool().Exec(ctx, query, record.Alias, record.RoomID, record.Creator)
	if err != nil {
		return err
	}

	if tag.RowsAffected() > 0 {
		im.counts.Aliases++
	} else {
		im.counts.Skipped++
	}

	return nil
}

// importUser вставляет пользователя с профилем и флагами. Повторный импорт обновляет
// профиль и флаги из источника (Synapse - источник истины до переключения); хеш
// пароля, уже переведенный в argon2id входом в SYNGO, не затирается.
func (im *importer) importUser(ctx context.Context, record *dumpRecord) error {
	localpart := record.Localpart
	if localpart == "" {
		localpart = strings.SplitN(strings.TrimPrefix(record.UserID, "@"), ":", 2)[0]
	}

	// Блокировки (locked) в SYNGO нет: заблокированный не должен снова стать активным.
	deactivated := record.Deactivated || record.Locked
	if record.Locked && !record.Deactivated {
		im.counts.LockedAsDeactivated++
	}

	if record.Suspended {
		im.counts.Suspended++
	}

	if record.ShadowBanned {
		im.counts.ShadowBanned++
	}

	const query = `
		INSERT INTO users (user_id, localpart, password_hash, display_name, avatar_url, is_admin, deactivated)
		VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7)
		ON CONFLICT (user_id) DO UPDATE
		SET password_hash = CASE WHEN users.password_hash LIKE '$argon2id$%' THEN users.password_hash
		                         ELSE EXCLUDED.password_hash END,
		    display_name = EXCLUDED.display_name, avatar_url = EXCLUDED.avatar_url,
		    is_admin = EXCLUDED.is_admin, deactivated = EXCLUDED.deactivated
		RETURNING (xmax = 0)`

	var inserted bool

	if err := im.db.Pool().QueryRow(ctx, query,
		record.UserID, localpart, record.Password, record.Display, record.AvatarURL,
		record.Admin, deactivated).Scan(&inserted); err != nil {
		return err
	}

	if inserted {
		im.counts.Users++
	} else {
		im.counts.Updated++
	}

	return nil
}

// importRoom вставляет комнату.
func (im *importer) importRoom(ctx context.Context, record *dumpRecord) error {
	version := record.RoomVersion
	if version == "" {
		version = "10"
	}

	im.counts.countRoomVersion(version)

	// Версия, правил которой у нас нет (нестабильные org.matrix.*, v13+), не
	// переносится: комната с ней ломала бы чтение (room_version не число) или
	// жила бы по чужим правилам. Ошибка строки и ошибки ее событий дают ненулевой
	// код возврата до окна переключения.
	if !roomVersionSupported(version) {
		return fmt.Errorf("комната %s: версия %q не поддерживается (поддерживаются 1-%d), комната и ее события не переносятся",
			record.RoomID, version, core.MaxRoomVersion)
	}

	const query = `
		INSERT INTO rooms (room_id, room_version, creator, is_public)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (room_id) DO NOTHING`

	tag, err := im.db.Pool().Exec(ctx, query, record.RoomID, version, record.Creator, record.IsPublic)
	if err != nil {
		return err
	}

	if tag.RowsAffected() > 0 {
		im.counts.Rooms++
	} else {
		im.counts.Skipped++
	}

	return nil
}

// importMembership материализует членство (история уже в событиях).
func (im *importer) importMembership(ctx context.Context, record *dumpRecord) error {
	const query = `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership, forgotten)
		VALUES ($1, $2, $3, $3, $4, $5)
		ON CONFLICT (room_id, user_id) DO UPDATE
		SET event_id = EXCLUDED.event_id, membership = EXCLUDED.membership,
		    forgotten = EXCLUDED.forgotten`

	if _, err := im.db.Pool().Exec(ctx, query,
		record.EventID, record.RoomID, record.UserID, record.Membership, record.Forgotten); err != nil {
		return err
	}

	im.counts.Memberships++

	return nil
}

// importMedia переносит метаданные медиа. Байты копируются отдельно (rsync/media_store),
// поэтому storage_key вычисляется по раскладке Synapse: local_content/<2>/<2>/<rest>.
// Превью ссылок (url_cache) пропускаются, карантин переносится блокировкой:
// иначе медиа из карантина после syngo-import-media снова отдавалось бы.
func (im *importer) importMedia(ctx context.Context, record *dumpRecord) error {
	if isURLCache(record) {
		im.counts.Skipped++

		return nil
	}

	if len(record.MediaID) < 5 {
		return fmt.Errorf("media_id %q слишком короткий", record.MediaID)
	}

	storageKey := fmt.Sprintf("local_content/%s/%s/%s",
		record.MediaID[0:2], record.MediaID[2:4], record.MediaID[4:])

	contentURI := "mxc://" + im.serverName + "/" + record.MediaID

	const query = `
		INSERT INTO media (content_uri, media_id, server_name, uploader, content_type,
		                   size_bytes, file_name, storage_key, created_at, quarantined_by, blocked)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, to_timestamp($9::double precision / 1000),
		        $10::text, $10::text IS NOT NULL)
		ON CONFLICT (content_uri) DO NOTHING`

	tag, err := im.db.Pool().Exec(ctx, query,
		contentURI, record.MediaID, im.serverName, record.UserID, record.MediaType,
		record.MediaLen, record.UploadName, storageKey, record.CreatedAt, quarantinedBy(record))
	if err != nil {
		return err
	}

	if tag.RowsAffected() > 0 {
		im.counts.Media++
	} else {
		im.counts.Skipped++
	}

	return nil
}
