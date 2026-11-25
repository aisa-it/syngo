package importevents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Record — событие из дампа Synapse (kind=event).
//
// Дамп syngo-synapse-export кладет исходный PDU строкой в поле json (как его
// хранит event_json.json Synapse), тело и ссылки берутся оттуда. Старый формат
// (удаленный export-events.sql, synapse-fixture) - content/prev_events/auth_events
// отдельными полями - читается по-прежнему.
type Record struct {
	Kind       string          `json:"kind"`
	RoomID     string          `json:"room_id"`
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	Sender     string          `json:"sender"`
	StateKey   *string         `json:"state_key"`
	Content    json.RawMessage `json:"content"`
	OriginTS   int64           `json:"origin_server_ts"`
	Depth      int64           `json:"depth"`
	PrevEvents EventRefs       `json:"prev_events"`
	AuthEvents EventRefs       `json:"auth_events"`
	Stream     *int64          `json:"stream_ordering"`
	Outlier    bool            `json:"outlier"`
	// JSON - исходный PDU (event_json.json Synapse) строкой.
	JSON string `json:"json"`
	// RejectedReason - событие отклонено Synapse (rejections), SoftFailed - не
	// прошло проверку по текущему состоянию: такие события, как и outlier, не
	// входят в timeline и состояние.
	RejectedReason *string `json:"rejected_reason"`
	SoftFailed     bool    `json:"soft_failed"`
}

// Строки kind=state - текущее состояние комнаты (current_state_events Synapse):
// room_id, type, state_key, event_id. Строки одной комнаты идут подряд.

// Manifest - последняя строка дампа syngo-synapse-export (kind=manifest): сколько
// событий выгружено. Без нее оборванная выгрузка неотличима от полной.
type Manifest struct {
	Kind       string `json:"kind"`
	Events     int64  `json:"events"`
	MinStream  int64  `json:"min_stream"`
	MaxStream  int64  `json:"max_stream"`
	NullStream int64  `json:"null_stream"`
	Rooms      int64  `json:"rooms"`
	StateRows  int64  `json:"state_rows"`
	ExportedAt string `json:"exported_at"`
}

// EventRefs - prev_events/auth_events. В комнатах v1 и v2 ссылка - пара
// [event_id, {"sha256": ...}], в v3+ - строка; храним только event_id
// (раньше события v1/v2 целиком уходили в bad).
type EventRefs []string

// UnmarshalJSON принимает оба формата ссылок.
func (r *EventRefs) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*r = nil

		return nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("ссылки на события: %w", err)
	}

	refs := make(EventRefs, 0, len(items))

	for _, item := range items {
		var id string

		switch {
		case len(item) > 0 && item[0] == '"':
			if err := json.Unmarshal(item, &id); err != nil {
				return fmt.Errorf("ссылка на событие: %w", err)
			}
		case len(item) > 0 && item[0] == '[':
			var pair []json.RawMessage
			if err := json.Unmarshal(item, &pair); err != nil || len(pair) == 0 {
				return fmt.Errorf("ссылка-пара на событие: %s", item)
			}

			if err := json.Unmarshal(pair[0], &id); err != nil {
				return fmt.Errorf("ссылка-пара на событие: %w", err)
			}
		default:
			return fmt.Errorf("ссылка на событие: неожиданный формат %s", item)
		}

		refs = append(refs, id)
	}

	*r = refs

	return nil
}

// pduBody - поля исходного PDU, которые нужны импорту.
type pduBody struct {
	Content    json.RawMessage `json:"content"`
	PrevEvents EventRefs       `json:"prev_events"`
	AuthEvents EventRefs       `json:"auth_events"`
	Origin     string          `json:"origin"`
	Redacts    string          `json:"redacts"`
	Signatures json.RawMessage `json:"signatures"`
	Hashes     json.RawMessage `json:"hashes"`
}

// Причины, по которым событие не входит в timeline (Event.Excluded).
const (
	ExcludedRejected   = "rejected"
	ExcludedSoftFailed = "soft_failed"
	ExcludedOutlier    = "outlier"
)

// StateRow - строка текущего состояния комнаты источника.
type StateRow struct {
	RoomID   string
	Type     string
	StateKey string
	EventID  string
}

// StateWriter - необязательная возможность писателя: заменить состояние
// комнаты состоянием источника; возвращает строки, чьих событий нет.
type StateWriter interface {
	ReplaceRoomState(ctx context.Context, roomID string, rows []StateRow) ([]StateRow, error)
}

// RedactionApplier - необязательная возможность писателя: применить
// записанные redaction после импорта (цель может идти позже redaction).
type RedactionApplier interface {
	ApplyPendingRedactions(ctx context.Context) (int64, error)
}

// RoomVersioner - необязательная возможность писателя: версии комнат для
// проверки event_id по reference hash.
type RoomVersioner interface {
	RoomVersions(ctx context.Context) (map[string]string, error)
}

// BatchWriter — запись пачки событий. Реализация обязана быть идемпотентной по event_id.
type BatchWriter interface {
	// InsertEvents пишет пачку и возвращает число реально вставленных строк (конфликты не считаются).
	InsertEvents(ctx context.Context, events []Event) (int64, error)
	// ExistingRooms возвращает множество известных комнат (для проверки целостности).
	ExistingRooms(ctx context.Context) (map[string]struct{}, error)
}

// SequenceSyncer — необязательная возможность писателя: подтянуть глобальную
// последовательность stream_ordering после импорта (см. store.SyncStreamOrdering).
type SequenceSyncer interface {
	SyncStreamOrdering(ctx context.Context) error
}

// ReceiptApplier - необязательная возможность писателя: после событий проставить
// позиции отметкам прочтения, импортированным раньше них (данные пользователей
// по runbook идут до событий), и поднять до них сводку непрочитанного.
type ReceiptApplier interface {
	ApplyImportedReceipts(ctx context.Context) (int64, error)
}

// RowErrorClassifier — необязательная возможность писателя: отличить ошибку
// данных одной строки (ограничение, кодировка) от отказа БД. Если пачка упала
// на ошибке данных, импорт повторяет ее по одному событию и откладывает только
// плохие строки, а не роняет весь импорт.
type RowErrorClassifier interface {
	IsRowError(err error) bool
}

// Event — событие в терминах хранилища.
type Event struct {
	EventID        string
	RoomID         string
	Type           string
	Sender         string
	StateKey       *string
	Content        []byte
	PrevEvents     []string
	AuthEvents     []string
	Depth          int64
	OriginServerTS int64
	Origin         string
	// StreamOrdering - stream_ordering источника; 0 - не задан (назначит БД).
	// Отрицательный - событие из backfill Synapse, сохраняется как есть.
	StreamOrdering int64
	// PDUJSON - исходный PDU текстом; Signatures, Hashes - из него.
	PDUJSON    string
	Signatures []byte
	Hashes     []byte
	// Redacts - цель m.room.redaction (top-level redacts до v11, content.redacts в v11).
	Redacts string
	// Excluded - причина, по которой событие не входит в timeline и состояние
	// (ExcludedRejected, ...); пусто - обычное событие.
	Excluded       string
	RejectedReason *string
}

// Options — параметры импорта.
type Options struct {
	// BatchSize — сколько событий накапливать перед вставкой (по умолчанию 1000).
	BatchSize int
	// ProgressEvery — как часто печатать прогресс (0 — каждые 50 000 событий).
	ProgressEvery int
	// ProgressWriter — куда писать прогресс (по умолчанию os.Stderr).
	ProgressWriter io.Writer
	// ResumeAfterLine - строки дампа с номером <= ResumeAfterLine уже записаны
	// прошлым запуском: они только считаются (для сверки с манифестом), но не
	// пишутся (продолжение с чекпойнта).
	ResumeAfterLine int64
	// Checkpoint вызывается после каждой записанной пачки с номером строки, до
	// которой включительно дамп записан. Ошибка останавливает импорт.
	Checkpoint func(line int64) error
	// BadLines - куда копировать строки дампа, которые не удалось импортировать
	// (карантин для разбора, по строке JSONL).
	BadLines io.Writer
	// OnBad вызывается для каждой неимпортированной строки (syngo-import ведет
	// по ним свой отчет).
	OnBad func(line int64, msg string)
}

// Report — итоги импорта.
type Report struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Read       int64
	Inserted   int64
	Skipped    int64 // уже были в БД
	Bad        int64 // неизвестная комната/битая строка
	Resumed    int64 // события до чекпойнта (записаны прошлым запуском)
	// Excluded - события вне timeline по причинам (rejected, soft_failed, outlier).
	Excluded map[string]int64
	// IDMismatch - event_id не совпал с reference hash исходного PDU.
	IDMismatch int64
	// StateRooms, StateRows - комнат и строк состояния из current_state_events;
	// StateMissing - строк, чье событие не импортировано.
	StateRooms   int64
	StateRows    int64
	StateMissing int64
	// Redacted - целей, удаленных импортированными redaction.
	Redacted int64
	// Receipts - отметок прочтения, получивших позицию события.
	Receipts int64
	ByType   map[string]int64
	Errors   []string
	RSSBytes int64
	// Manifest - манифест дампа, если он был в конце файла.
	Manifest *Manifest
}

// Duration возвращает затраченное время.
func (r Report) Duration() time.Duration {
	return r.FinishedAt.Sub(r.StartedAt)
}

// Importer переносит историю событий пачками.
type Importer struct {
	writer BatchWriter
	opts   Options
	rss    func() int64
}

// New создаёт импортёр.
func New(writer BatchWriter, opts Options) *Importer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}

	if opts.ProgressEvery == 0 {
		opts.ProgressEvery = 50_000 // значение по умолчанию
	}

	if opts.ProgressEvery < 0 {
		opts.ProgressEvery = 0 // явное «выключить»
	}

	if opts.ProgressWriter == nil {
		opts.ProgressWriter = os.Stderr
	}

	return &Importer{writer: writer, opts: opts, rss: readRSS}
}

// pending - событие пачки вместе с исходной строкой (для карантина).
type pending struct {
	event Event
	line  int64
	raw   []byte
}

// Session - один проход импорта по строкам дампа. Import ведет сессию сам;
// syngo-import подает в нее строки kind=event из общего дампа.
type Session struct {
	im       *Importer
	report   Report
	rooms    map[string]struct{}
	versions map[string]string
	batch    []pending
	line     int64
	events   int64 // строк kind=event, включая пропущенные по чекпойнту
	states   int64 // строк kind=state, включая пропущенные по чекпойнту
	withPDU  bool  // в дампе есть исходные PDU (формат syngo-synapse-export)
	finished bool

	// Состояние текущей комнаты: строки kind=state одной комнаты идут подряд.
	stateRoom string
	stateRows []StateRow
	stateLast int64 // номер последней строки состояния текущей комнаты
	stateDone map[string]struct{}
}

// Begin начинает сессию импорта.
func (im *Importer) Begin() *Session {
	return &Session{
		im:     im,
		report: Report{StartedAt: time.Now().UTC(), ByType: map[string]int64{}, Excluded: map[string]int64{}},
		batch:  make([]pending, 0, im.opts.BatchSize),
	}
}

// Import читает JSONL потоком и пишет события пачками.
//
// Память ограничена размером пачки: дамп не загружается целиком.
func (im *Importer) Import(ctx context.Context, r io.Reader) (Report, error) {
	session := im.Begin()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20) // строка дампа с телом события — крупная

	var line int64

	for scanner.Scan() {
		line++

		if err := session.Line(ctx, line, scanner.Bytes()); err != nil {
			return session.report, err
		}
	}

	if err := scanner.Err(); err != nil {
		return session.report, fmt.Errorf("import events: чтение дампа: %w", err)
	}

	return session.Finish(ctx)
}

// Line обрабатывает одну строку дампа. Ошибка - только отказ записи (БД,
// чекпойнт); плохая строка попадает в отчет (Bad), импорт продолжается.
func (s *Session) Line(ctx context.Context, line int64, data []byte) error {
	s.line = line

	raw := strings.TrimSpace(string(data))
	if raw == "" || strings.HasPrefix(raw, "--") || strings.HasPrefix(raw, "#") {
		return nil
	}

	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		s.bad(line, raw, fmt.Sprintf("строка %d: %v", line, err))

		return nil
	}

	switch rec.Kind {
	case "", "event":
	case "state":
		return s.stateLine(ctx, line, raw, &rec)
	case "manifest":
		var manifest Manifest
		if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
			s.bad(line, raw, fmt.Sprintf("строка %d: манифест: %v", line, err))

			return nil
		}

		s.report.Manifest = &manifest

		return nil
	default:
		return nil
	}

	s.events++
	s.report.Read++

	if rec.JSON != "" {
		s.withPDU = true
	}

	if line <= s.im.opts.ResumeAfterLine {
		s.report.Resumed++

		return nil
	}

	if rec.EventID == "" || rec.RoomID == "" || rec.Sender == "" {
		s.bad(line, raw, fmt.Sprintf("строка %d: нет event_id/room_id/sender", line))

		return nil
	}

	if err := s.loadRooms(ctx); err != nil {
		return err
	}

	if _, ok := s.rooms[rec.RoomID]; !ok {
		s.bad(line, raw, fmt.Sprintf("строка %d: комната %s не импортирована", line, rec.RoomID))

		return nil
	}

	ev, err := toEvent(rec)
	if err != nil {
		s.bad(line, raw, fmt.Sprintf("строка %d: %s: %v", line, rec.EventID, err))

		return nil
	}

	if msg := checkEventID(rec, s.versions[rec.RoomID]); msg != "" {
		s.report.IDMismatch++
		s.report.Errors = appendLimited(s.report.Errors, fmt.Sprintf("строка %d: %s", line, msg))
	}

	if ev.Excluded != "" {
		s.report.Excluded[ev.Excluded]++
	} else {
		s.report.ByType[rec.Type]++
	}

	s.batch = append(s.batch, pending{event: ev, line: line, raw: []byte(raw)})

	if len(s.batch) >= s.im.opts.BatchSize {
		if err := s.Flush(ctx); err != nil {
			return err
		}
	}

	if s.im.opts.ProgressEvery > 0 && s.report.Read%int64(s.im.opts.ProgressEvery) == 0 {
		s.im.progress(s.report)
	}

	return nil
}

// loadRooms читает комнаты и их версии при первом событии: syngo-import пишет
// комнаты в том же проходе раньше событий.
func (s *Session) loadRooms(ctx context.Context) error {
	if s.rooms != nil {
		return nil
	}

	rooms, err := s.im.writer.ExistingRooms(ctx)
	if err != nil {
		return fmt.Errorf("import events: загрузить комнаты: %w", err)
	}

	if versioner, ok := s.im.writer.(RoomVersioner); ok {
		versions, err := versioner.RoomVersions(ctx)
		if err != nil {
			return fmt.Errorf("import events: версии комнат: %w", err)
		}

		s.versions = versions
	}

	s.rooms = rooms

	return nil
}

// stateLine копит состояние комнаты; на смене комнаты пишет накопленное.
func (s *Session) stateLine(ctx context.Context, line int64, raw string, rec *Record) error {
	s.states++

	if line <= s.im.opts.ResumeAfterLine {
		return nil
	}

	if rec.RoomID == "" || rec.Type == "" || rec.StateKey == nil || rec.EventID == "" {
		s.bad(line, raw, fmt.Sprintf("строка %d: state без room_id/type/state_key/event_id", line))

		return nil
	}

	if rec.RoomID != s.stateRoom {
		if err := s.flushState(ctx); err != nil {
			return err
		}

		if _, seen := s.stateDone[rec.RoomID]; seen {
			// Повторная пачка той же комнаты затерла бы первую: дамп собран не по порядку.
			s.bad(line, raw, fmt.Sprintf("строка %d: состояние комнаты %s не подряд", line, rec.RoomID))

			return nil
		}

		s.stateRoom = rec.RoomID
	}

	s.stateRows = append(s.stateRows, StateRow{RoomID: rec.RoomID, Type: rec.Type, StateKey: *rec.StateKey, EventID: rec.EventID})
	s.stateLast = line

	return nil
}

// flushState заменяет состояние текущей комнаты строками источника.
func (s *Session) flushState(ctx context.Context) error {
	if s.stateRoom == "" {
		return nil
	}

	room, rows := s.stateRoom, s.stateRows
	s.stateRoom, s.stateRows = "", nil

	if s.stateDone == nil {
		s.stateDone = map[string]struct{}{}
	}

	s.stateDone[room] = struct{}{}

	// События состояния должны быть в БД раньше ссылок на них. Чекпойнт - только
	// после записи состояния: текущая строка (начало следующей комнаты) еще не записана.
	if err := s.flushBatch(ctx); err != nil {
		return err
	}

	writer, ok := s.im.writer.(StateWriter)
	if !ok {
		s.bad(s.stateLast, "", fmt.Sprintf("строка %d: писатель не поддерживает kind=state", s.stateLast))

		return nil
	}

	missing, err := writer.ReplaceRoomState(ctx, room, rows)
	if err != nil {
		return fmt.Errorf("import events: состояние комнаты %s: %w", room, err)
	}

	s.report.StateRooms++
	s.report.StateRows += int64(len(rows) - len(missing))

	for _, m := range missing {
		s.report.StateMissing++
		s.bad(s.stateLast, "", fmt.Sprintf("состояние %s (%s, %q): событие %s не импортировано (отклонено или потеряно)",
			room, m.Type, m.StateKey, m.EventID))
	}

	return s.checkpointAt(s.stateLast)
}

// Flush пишет накопленную пачку. syngo-import вызывает его перед строками
// других kind: membership ссылается на события (FK).
func (s *Session) Flush(ctx context.Context) error {
	if err := s.flushBatch(ctx); err != nil {
		return err
	}

	return s.checkpoint()
}

func (s *Session) flushBatch(ctx context.Context) error {
	if len(s.batch) == 0 {
		return nil
	}

	events := make([]Event, len(s.batch))
	for i := range s.batch {
		events[i] = s.batch[i].event
	}

	inserted, err := s.im.writer.InsertEvents(ctx, events)
	if err != nil {
		classifier, ok := s.im.writer.(RowErrorClassifier)
		if !ok || !classifier.IsRowError(err) {
			return fmt.Errorf("import events: пачка до строки %d: %w", s.line, err)
		}

		if err := s.insertOneByOne(ctx, classifier); err != nil {
			return err
		}
	} else {
		s.report.Inserted += inserted
		s.report.Skipped += int64(len(events)) - inserted
	}

	s.batch = s.batch[:0]

	return nil
}

// insertOneByOne повторяет упавшую пачку по одному событию: плохие строки
// уходят в отчет и карантин, остальные записываются.
func (s *Session) insertOneByOne(ctx context.Context, classifier RowErrorClassifier) error {
	for i := range s.batch {
		item := &s.batch[i]

		inserted, err := s.im.writer.InsertEvents(ctx, []Event{item.event})
		if err != nil {
			if !classifier.IsRowError(err) {
				return fmt.Errorf("import events: строка %d: %w", item.line, err)
			}

			s.report.ByType[item.event.Type]--
			s.bad(item.line, string(item.raw), fmt.Sprintf("строка %d: %s: %v", item.line, item.event.EventID, err))

			continue
		}

		s.report.Inserted += inserted
		s.report.Skipped += 1 - inserted
	}

	return nil
}

func (s *Session) checkpoint() error {
	// Пока копится состояние комнаты, записано только то, что до него.
	if s.stateRoom != "" {
		return nil
	}

	return s.checkpointAt(s.line)
}

func (s *Session) checkpointAt(line int64) error {
	if s.im.opts.Checkpoint == nil || line <= s.im.opts.ResumeAfterLine {
		return nil
	}

	if err := s.im.opts.Checkpoint(line); err != nil {
		return fmt.Errorf("import events: чекпойнт строки %d: %w", line, err)
	}

	return nil
}

// ErrIncompleteDump - число событий в дампе не совпало с манифестом выгрузки
// либо манифеста нет: выгрузка оборвалась.
var ErrIncompleteDump = errors.New("import events: дамп неполный")

// Finish дописывает пачку, подтягивает sequence и сверяет дамп с манифестом.
func (s *Session) Finish(ctx context.Context) (Report, error) {
	if s.finished {
		return s.report, nil
	}

	s.finished = true

	if err := s.flushState(ctx); err != nil {
		return s.report, err
	}

	if err := s.Flush(ctx); err != nil {
		return s.report, err
	}

	// redaction применяются после всех событий: цель может идти в дампе позже.
	if applier, ok := s.im.writer.(RedactionApplier); ok {
		redacted, err := applier.ApplyPendingRedactions(ctx)
		if err != nil {
			return s.report, fmt.Errorf("import events: применение redaction: %w", err)
		}

		s.report.Redacted = redacted
	}

	// После импорта истории последовательность могла отстать от вставленных значений:
	// иначе новый локальный /send получит stream_ordering меньше импортированных.
	if syncer, ok := s.im.writer.(SequenceSyncer); ok {
		if err := syncer.SyncStreamOrdering(ctx); err != nil {
			return s.report, fmt.Errorf("import events: синхронизация sequence: %w", err)
		}
	}

	if applier, ok := s.im.writer.(ReceiptApplier); ok {
		positioned, err := applier.ApplyImportedReceipts(ctx)
		if err != nil {
			return s.report, fmt.Errorf("import events: отметки прочтения: %w", err)
		}

		s.report.Receipts = positioned
	}

	s.report.FinishedAt = time.Now().UTC()
	s.report.RSSBytes = s.im.rss()

	switch manifest := s.report.Manifest; {
	case manifest != nil && manifest.Events != s.events:
		return s.report, fmt.Errorf("%w: в манифесте %d событий, в дампе %d", ErrIncompleteDump, manifest.Events, s.events)
	case manifest != nil && manifest.StateRows != s.states:
		return s.report, fmt.Errorf("%w: в манифесте %d строк состояния, в дампе %d", ErrIncompleteDump, manifest.StateRows, s.states)
	case manifest == nil && s.withPDU:
		return s.report, fmt.Errorf("%w: нет строки kind=manifest в конце дампа syngo-synapse-export", ErrIncompleteDump)
	}

	return s.report, nil
}

func (s *Session) bad(line int64, raw, msg string) {
	s.report.Bad++
	s.report.Errors = appendLimited(s.report.Errors, msg)

	if s.im.opts.OnBad != nil {
		s.im.opts.OnBad(line, msg)
	}

	if s.im.opts.BadLines != nil && raw != "" {
		_, _ = fmt.Fprintln(s.im.opts.BadLines, raw)
	}
}

// progress печатает строку прогресса с ETA.
func (im *Importer) progress(report Report) {
	elapsed := time.Since(report.StartedAt)
	rate := float64(report.Read) / elapsed.Seconds()

	fmt.Fprintf(im.opts.ProgressWriter, "events: read=%d inserted=%d skipped=%d bad=%d rate=%.0f/s elapsed=%s\n",
		report.Read, report.Inserted, report.Skipped, report.Bad, rate, elapsed.Round(time.Second))
}

// toEvent переводит запись дампа в событие хранилища.
func toEvent(rec Record) (Event, error) {
	stream := int64(0)
	if rec.Stream != nil {
		stream = *rec.Stream
	}

	content, prev, auth := []byte(rec.Content), []string(rec.PrevEvents), []string(rec.AuthEvents)

	var pdu pduBody

	if rec.JSON != "" {
		if err := json.Unmarshal([]byte(rec.JSON), &pdu); err != nil {
			return Event{}, fmt.Errorf("исходный PDU: %w", err)
		}

		content, prev, auth = pdu.Content, pdu.PrevEvents, pdu.AuthEvents
	}

	if len(content) == 0 || string(content) == "null" {
		return Event{}, errors.New("нет content (в Synapse нет строки event_json)")
	}

	content, err := SanitizeContent(content)
	if err != nil {
		return Event{}, err
	}

	origin := pdu.Origin
	if origin == "" {
		origin = originOf(rec.Sender)
	}

	ev := Event{
		EventID:        rec.EventID,
		RoomID:         rec.RoomID,
		Type:           rec.Type,
		Sender:         rec.Sender,
		StateKey:       rec.StateKey,
		Content:        content,
		PrevEvents:     prev,
		AuthEvents:     auth,
		Depth:          rec.Depth,
		OriginServerTS: rec.OriginTS,
		Origin:         origin,
		StreamOrdering: stream,
		PDUJSON:        rec.JSON,
		Signatures:     nonNullJSON(pdu.Signatures),
		Hashes:         nonNullJSON(pdu.Hashes),
		RejectedReason: rec.RejectedReason,
	}

	if rec.Type == redactionType {
		ev.Redacts = pdu.Redacts
		if ev.Redacts == "" {
			ev.Redacts = contentRedacts(content)
		}
	}

	switch {
	case rec.RejectedReason != nil:
		ev.Excluded = ExcludedRejected
	case rec.SoftFailed:
		ev.Excluded = ExcludedSoftFailed
	case rec.Outlier:
		ev.Excluded = ExcludedOutlier
	}

	return ev, nil
}

const redactionType = "m.room.redaction"

func contentRedacts(content []byte) string {
	var body struct {
		Redacts string `json:"redacts"`
	}

	if json.Unmarshal(content, &body) != nil {
		return ""
	}

	return body.Redacts
}

func nonNullJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	return raw
}

// originOf возвращает домен отправителя (origin события), с фолбэком на "imported".
func originOf(sender string) string {
	if _, domain, found := strings.Cut(sender, ":"); found && domain != "" {
		return domain
	}

	return "imported"
}

func appendLimited(list []string, msg string) []string {
	const maxErrors = 20

	if len(list) < maxErrors {
		return append(list, msg)
	}

	return list
}

// readRSS читает текущий RSS процесса (Linux /proc/self/status) для отчёта.
func readRSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}

		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}

		return kib * 1024
	}

	return 0
}

// Text отдаёт отчёт в текстовом виде.
func (r Report) Text() string {
	var b strings.Builder

	fmt.Fprintf(&b, "import events report\n")
	fmt.Fprintf(&b, "started:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "finished: %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "duration: %s\n", r.Duration().Round(time.Millisecond))
	fmt.Fprintf(&b, "read=%d inserted=%d skipped=%d bad=%d resumed=%d rss=%.1f MiB\n",
		r.Read, r.Inserted, r.Skipped, r.Bad, r.Resumed, float64(r.RSSBytes)/(1<<20))
	fmt.Fprintf(&b, "excluded: rejected=%d soft_failed=%d outlier=%d; event_id mismatch=%d; redacted=%d\n",
		r.Excluded[ExcludedRejected], r.Excluded[ExcludedSoftFailed], r.Excluded[ExcludedOutlier], r.IDMismatch, r.Redacted)
	fmt.Fprintf(&b, "state: rooms=%d rows=%d missing=%d\n", r.StateRooms, r.StateRows, r.StateMissing)
	fmt.Fprintf(&b, "receipts: positioned=%d\n", r.Receipts)

	if r.Manifest != nil {
		fmt.Fprintf(&b, "manifest: events=%d state_rows=%d min_stream=%d max_stream=%d rooms=%d exported_at=%s\n",
			r.Manifest.Events, r.Manifest.StateRows, r.Manifest.MinStream, r.Manifest.MaxStream, r.Manifest.Rooms, r.Manifest.ExportedAt)
	}

	types := make([]string, 0, len(r.ByType))
	for t := range r.ByType {
		types = append(types, t)
	}

	sort.Strings(types)

	for _, t := range types {
		fmt.Fprintf(&b, "type %s: %d\n", t, r.ByType[t])
	}

	for _, e := range r.Errors {
		fmt.Fprintf(&b, "error: %s\n", e)
	}

	return b.String()
}
