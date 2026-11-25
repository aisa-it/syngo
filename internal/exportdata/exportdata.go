// Package exportdata выгружает данные SYNGO в JSONL-формат импортёра (ADR-0006).
//
// Обратный путь (exit path): дамп должен читаться нашими же импортёрами (syngo-import,
// syngo-import-events, syngo-import-media) и быть понятным человеку. Схему Synapse
// пакет не читает — источник только наши таблицы.
//
// Формат: по строке JSON на объект, поле kind: user | room | room_alias | event | state |
// manifest | membership | receipt | media - тот же, что у syngo-synapse-export: строка
// события несет исходный PDU (pdu_json) в поле json, отклоненные, soft-failed и outlier
// (excluded_events) выгружаются с теми же признаками, что в дампе Synapse, у удаленных
// событий content восстановлен из redactions.original_content (импорт снова применит
// redaction и сохранит оригинал), членство - с event_id, состояние - строками kind=state,
// в конце событий - манифест. Порядок событий — по stream_ordering (как в Synapse),
// включая отрицательные (backfill).
package exportdata

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options — параметры выгрузки.
type Options struct {
	// BatchSize — сколько событий читать за запрос (по умолчанию 1000).
	BatchSize int
	// MediaSourceDir — наш каталог объектного хранилища (media.directory); файлы копируются в MediaOutDir.
	MediaSourceDir string
	// MediaOutDir — куда складывать файлы в раскладке Synapse (local_content/<2>/<2>/<rest>).
	MediaOutDir string
	// AfterStream - выгрузить только события со stream_ordering больше (созданные
	// после cutover, для отката); строки состояния тогда не выгружаются.
	// Не задан (HasAfterStream=false) - все события.
	AfterStream    int64
	HasAfterStream bool
	// MediaSince - выгрузить только медиа, созданные не раньше (нулевое - все).
	MediaSince time.Time
}

// Report — итоги выгрузки.
type Report struct {
	Users       int64
	Rooms       int64
	Aliases     int64
	Memberships int64
	Events      int64
	// Excluded - из них отклоненных, soft-failed и outlier (excluded_events).
	Excluded int64
	// Restored - удаленных событий, у которых content восстановлен из redactions.
	Restored    int64
	StateRows   int64
	Receipts    int64
	Media       int64
	MediaCopied int64
	MediaFailed int64
	StartedAt   time.Time
	FinishedAt  time.Time
}

// Exporter выгружает данные из PostgreSQL в JSONL.
type Exporter struct {
	pool    *pgxpool.Pool
	opts    Options
	now     func() time.Time
	copyErr []string
}

// New создаёт выгрузчик.
func New(pool *pgxpool.Pool, opts Options) *Exporter {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}

	return &Exporter{pool: pool, opts: opts, now: time.Now}
}

// Run пишет дамп в w построчно и возвращает отчёт.
func (e *Exporter) Run(ctx context.Context, w io.Writer) (Report, error) {
	report := Report{StartedAt: e.now().UTC()}
	out := bufio.NewWriterSize(w, 1<<20)

	defer func() { _ = out.Flush() }()

	if err := e.writeUsers(ctx, out, &report); err != nil {
		return report, err
	}

	if err := e.writeRooms(ctx, out, &report); err != nil {
		return report, err
	}

	if err := e.writeAliases(ctx, out, &report); err != nil {
		return report, err
	}

	// События, состояние и манифест - раньше членства: у room_memberships FK на
	// events, импорт пишет строки в порядке файла.
	manifest, err := e.writeEvents(ctx, out, &report)
	if err != nil {
		return report, err
	}

	if !e.opts.HasAfterStream {
		if err := e.writeState(ctx, out, &report); err != nil {
			return report, err
		}
	}

	manifest.StateRows = report.StateRows
	manifest.ExportedAt = e.now().UTC().Format(time.RFC3339)

	if err := writeLine(out, manifest); err != nil {
		return report, err
	}

	if err := e.writeMemberships(ctx, out, &report); err != nil {
		return report, err
	}

	// Отметки прочтения - как kind=receipt из export.sql; в архиве
	// после cutover не нужны.
	if !e.opts.HasAfterStream {
		if err := e.writeReceipts(ctx, out, &report); err != nil {
			return report, err
		}
	}

	if err := e.writeMedia(ctx, out, &report); err != nil {
		return report, err
	}

	if err := out.Flush(); err != nil {
		return report, fmt.Errorf("export: flush: %w", err)
	}

	report.FinishedAt = e.now().UTC()

	return report, nil
}

// MediaErrors возвращает ошибки копирования медиа (для отчёта).
func (e *Exporter) MediaErrors() []string { return e.copyErr }

// writeLine сериализует объект одной строкой JSON.
func writeLine(out *bufio.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("export: marshal: %w", err)
	}

	if _, err := out.Write(data); err != nil {
		return fmt.Errorf("export: write: %w", err)
	}

	return out.WriteByte('\n')
}

// writeUsers выгружает пользователей (argon2id PHC-строка как есть).
func (e *Exporter) writeUsers(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `SELECT user_id, localpart, password_hash, display_name, deactivated
		FROM users ORDER BY user_id`)
	if err != nil {
		return fmt.Errorf("export users: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			userID, localpart, hash, display string
			deactivated                      bool
		)

		if err := rows.Scan(&userID, &localpart, &hash, &display, &deactivated); err != nil {
			return fmt.Errorf("export users scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "user", "user_id": userID, "localpart": localpart,
			"password_hash": hash, "display_name": display, "deactivated": deactivated,
		}); err != nil {
			return err
		}

		report.Users++
	}

	return rows.Err()
}

// writeRooms выгружает комнаты.
func (e *Exporter) writeRooms(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `SELECT room_id, room_version, creator, is_public FROM rooms ORDER BY room_id`)
	if err != nil {
		return fmt.Errorf("export rooms: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, version, creator string
			isPublic                 bool
		)

		if err := rows.Scan(&roomID, &version, &creator, &isPublic); err != nil {
			return fmt.Errorf("export rooms scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "room", "room_id": roomID, "room_version": version,
			"creator": creator, "is_public": isPublic,
		}); err != nil {
			return err
		}

		report.Rooms++
	}

	return rows.Err()
}

// writeAliases выгружает алиасы комнат (раньше не выгружались).
func (e *Exporter) writeAliases(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `SELECT room_alias, room_id, creator FROM room_aliases ORDER BY room_alias`)
	if err != nil {
		return fmt.Errorf("export aliases: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var alias, roomID, creator string

		if err := rows.Scan(&alias, &roomID, &creator); err != nil {
			return fmt.Errorf("export aliases scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "room_alias", "alias": alias, "room_id": roomID, "creator": creator,
		}); err != nil {
			return err
		}

		report.Aliases++
	}

	return rows.Err()
}

// writeMemberships выгружает текущее членство с event_id: без него обратный
// импорт давал ошибку FK по каждой строке.
func (e *Exporter) writeMemberships(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `
		SELECT DISTINCT ON (room_id, user_id) room_id, user_id, membership, forgotten, event_id, sender
		FROM room_memberships
		ORDER BY room_id, user_id, created_at DESC, event_id DESC`)
	if err != nil {
		return fmt.Errorf("export memberships: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, userID, membership, eventID, sender string
			forgotten                                   bool
		)

		if err := rows.Scan(&roomID, &userID, &membership, &forgotten, &eventID, &sender); err != nil {
			return fmt.Errorf("export memberships scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "membership", "room_id": roomID, "user_id": userID,
			"membership": membership, "forgotten": forgotten, "event_id": eventID, "sender": sender,
		}); err != nil {
			return err
		}

		report.Memberships++
	}

	return rows.Err()
}

// writeReceipts выгружает отметки прочтения m.read и m.read.private локальных
// пользователей в формате export.sql (kind=receipt).
func (e *Exporter) writeReceipts(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `
		SELECT r.room_id, r.user_id, r.receipt_type, r.thread_id, r.event_id, r.ts
		FROM receipts r
		JOIN users u ON u.user_id = r.user_id
		WHERE r.receipt_type IN ('m.read', 'm.read.private')
		ORDER BY r.room_id, r.user_id, r.receipt_type, r.thread_id`)
	if err != nil {
		return fmt.Errorf("export receipts: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, userID, receiptType, threadID, eventID string
			ts                                             int64
		)

		if err := rows.Scan(&roomID, &userID, &receiptType, &threadID, &eventID, &ts); err != nil {
			return fmt.Errorf("export receipts scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "receipt", "room_id": roomID, "user_id": userID, "receipt_type": receiptType,
			"thread_id": threadID, "event_id": eventID, "ts": ts,
		}); err != nil {
			return err
		}

		report.Receipts++
	}

	return rows.Err()
}

// eventLine — строка события в формате syngo-synapse-export: исходный PDU в json,
// метаданные и признаки исключения; content/prev/auth - для дампов без PDU
// (события, созданные ранними версиями synapse-go, PDU не хранят).
type eventLine struct {
	Kind           string          `json:"kind"`
	RoomID         string          `json:"room_id"`
	EventID        string          `json:"event_id"`
	Type           string          `json:"type"`
	Sender         string          `json:"sender"`
	StateKey       *string         `json:"state_key"`
	Content        json.RawMessage `json:"content,omitempty"`
	OriginTS       int64           `json:"origin_server_ts"`
	Depth          int64           `json:"depth"`
	PrevEvents     []string        `json:"prev_events,omitempty"`
	AuthEvents     []string        `json:"auth_events,omitempty"`
	StreamOrdering *int64          `json:"stream_ordering"`
	JSON           string          `json:"json,omitempty"`
	Outlier        bool            `json:"outlier,omitempty"`
	SoftFailed     bool            `json:"soft_failed,omitempty"`
	RejectedReason *string         `json:"rejected_reason,omitempty"`
}

// manifestLine - манифест выгрузки (как importevents.Manifest).
type manifestLine struct {
	Kind       string `json:"kind"`
	Events     int64  `json:"events"`
	MinStream  int64  `json:"min_stream"`
	MaxStream  int64  `json:"max_stream"`
	NullStream int64  `json:"null_stream"`
	Rooms      int64  `json:"rooms"`
	StateRows  int64  `json:"state_rows"`
	ExportedAt string `json:"exported_at"`
}

func (m *manifestLine) add(line *eventLine, rooms map[string]struct{}) {
	m.Events++
	rooms[line.RoomID] = struct{}{}
	m.Rooms = int64(len(rooms))

	if line.StreamOrdering == nil {
		m.NullStream++

		return
	}

	stream := *line.StreamOrdering
	if m.Events-m.NullStream == 1 || stream < m.MinStream {
		m.MinStream = stream
	}

	if m.Events-m.NullStream == 1 || stream > m.MaxStream {
		m.MaxStream = stream
	}
}

// writeEvents выгружает timeline (events) пачками по stream_ordering, затем
// excluded_events; возвращает манифест без числа строк состояния.
func (e *Exporter) writeEvents(ctx context.Context, out *bufio.Writer, report *Report) (manifestLine, error) {
	manifest := manifestLine{Kind: "manifest"}
	rooms := map[string]struct{}{}

	// Отрицательный stream_ordering - backfill Synapse: раньше выгрузка начиналась
	// с 0 и теряла такие события.
	last := int64(math.MinInt64)
	if e.opts.HasAfterStream {
		last = e.opts.AfterStream
	}

	for {
		events, err := e.eventBatch(ctx, last, report)
		if err != nil {
			return manifest, err
		}

		if len(events) == 0 {
			break
		}

		for i := range events {
			if err := writeLine(out, events[i]); err != nil {
				return manifest, err
			}

			manifest.add(&events[i], rooms)
			report.Events++
		}

		last = *events[len(events)-1].StreamOrdering
	}

	if err := e.writeExcluded(ctx, out, report, &manifest, rooms); err != nil {
		return manifest, err
	}

	return manifest, nil
}

// eventBatch читает очередную пачку событий после last.
func (e *Exporter) eventBatch(ctx context.Context, last int64, report *Report) ([]eventLine, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events,
		       e.depth, e.origin_server_ts, e.stream_ordering, coalesce(e.pdu_json, ''), rd.original_content
		FROM events e
		LEFT JOIN redactions rd ON rd.event_id = e.redacted_by AND rd.redacts = e.event_id
		WHERE e.stream_ordering > $1
		ORDER BY e.stream_ordering
		LIMIT $2`, last, e.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("export events: %w", err)
	}

	defer rows.Close()

	var events []eventLine

	for rows.Next() {
		var (
			line              eventLine
			content, original []byte
			stream            int64
		)

		line.Kind = "event"

		if err := rows.Scan(&line.EventID, &line.RoomID, &line.Type, &line.Sender, &line.StateKey, &content,
			&line.PrevEvents, &line.AuthEvents, &line.Depth, &line.OriginTS, &stream, &line.JSON, &original); err != nil {
			return nil, fmt.Errorf("export events scan: %w", err)
		}

		line.StreamOrdering = &stream
		line.Content = content

		// Удаленное событие: в events вырезанный content, оригинал - в redactions до
		// истечения retention. Выгружаем оригинал - импорт применит redaction заново и
		// сохранит его так же; иначе оригинал терялся бы при переезде.
		if len(original) > 0 && string(original) != "null" {
			line.Content = original

			if line.JSON != "" {
				pdu, err := replaceContent(line.JSON, original)
				if err != nil {
					return nil, fmt.Errorf("export events: %s: %w", line.EventID, err)
				}

				line.JSON = pdu
			}

			report.Restored++
		}

		events = append(events, line)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export events: %w", err)
	}

	return events, nil
}

// writeExcluded выгружает отклоненные, soft-failed и outlier с признаками дампа Synapse.
func (e *Exporter) writeExcluded(ctx context.Context, out *bufio.Writer, report *Report, manifest *manifestLine, rooms map[string]struct{}) error {
	lastID := ""

	for {
		rows, err := e.pool.Query(ctx, `
			SELECT event_id, room_id, type, sender, state_key, reason, coalesce(rejected_reason, reason),
			       depth, origin_server_ts, stream_ordering, coalesce(pdu_json, '')
			FROM excluded_events
			WHERE event_id > $1 AND (NOT $3 OR stream_ordering > $4)
			ORDER BY event_id
			LIMIT $2`, lastID, e.opts.BatchSize, e.opts.HasAfterStream, e.opts.AfterStream)
		if err != nil {
			return fmt.Errorf("export excluded events: %w", err)
		}

		var batch []eventLine

		for rows.Next() {
			var (
				line           eventLine
				reason, reject string
			)

			line.Kind = "event"

			if err := rows.Scan(&line.EventID, &line.RoomID, &line.Type, &line.Sender, &line.StateKey, &reason, &reject,
				&line.Depth, &line.OriginTS, &line.StreamOrdering, &line.JSON); err != nil {
				rows.Close()

				return fmt.Errorf("export excluded events scan: %w", err)
			}

			switch reason {
			case "rejected":
				line.RejectedReason = &reject
			case "soft_failed":
				line.SoftFailed = true
			default:
				line.Outlier = true
			}

			batch = append(batch, line)
		}

		rows.Close()

		if err := rows.Err(); err != nil {
			return fmt.Errorf("export excluded events: %w", err)
		}

		if len(batch) == 0 {
			return nil
		}

		for i := range batch {
			if err := writeLine(out, batch[i]); err != nil {
				return err
			}

			manifest.add(&batch[i], rooms)
			report.Events++
			report.Excluded++
		}

		lastID = batch[len(batch)-1].EventID
	}
}

// writeState выгружает текущее состояние комнат (room_state) строками kind=state,
// строки одной комнаты подряд - как current_state_events в дампе Synapse.
func (e *Exporter) writeState(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `
		SELECT room_id, type, state_key, event_id FROM room_state ORDER BY room_id, type, state_key`)
	if err != nil {
		return fmt.Errorf("export state: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var roomID, typ, stateKey, eventID string

		if err := rows.Scan(&roomID, &typ, &stateKey, &eventID); err != nil {
			return fmt.Errorf("export state scan: %w", err)
		}

		if err := writeLine(out, map[string]any{
			"kind": "state", "room_id": roomID, "type": typ, "state_key": stateKey, "event_id": eventID,
		}); err != nil {
			return err
		}

		report.StateRows++
	}

	return rows.Err()
}

// replaceContent подставляет content в исходный PDU.
func replaceContent(pdu string, content []byte) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(pdu), &object); err != nil {
		return "", fmt.Errorf("исходный PDU: %w", err)
	}

	object["content"] = content

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(object); err != nil {
		return "", fmt.Errorf("исходный PDU: %w", err)
	}

	return strings.TrimRight(buf.String(), "\n"), nil
}

// writeMedia выгружает метаданные медиа и копирует файлы в раскладку Synapse.
func (e *Exporter) writeMedia(ctx context.Context, out *bufio.Writer, report *Report) error {
	rows, err := e.pool.Query(ctx, `
		SELECT media_id, content_type, size_bytes, coalesce(file_name, ''), coalesce(uploader, ''),
		       storage_key, server_name, coalesce(extract(epoch FROM created_at) * 1000, 0)::bigint,
		       quarantined_by, sha256
		FROM media
		WHERE NOT is_remote AND uploaded AND ($1::timestamptz IS NULL OR created_at >= $1)
		ORDER BY media_id`, mediaSince(e.opts.MediaSince))
	if err != nil {
		return fmt.Errorf("export media: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			mediaID, contentType, fileName, uploader, storageKey, serverName string
			size, createdMS                                                  int64
			quarantinedBy                                                    *string
			sum                                                              []byte
		)

		if err := rows.Scan(&mediaID, &contentType, &size, &fileName, &uploader, &storageKey, &serverName, &createdMS,
			&quarantinedBy, &sum); err != nil {
			return fmt.Errorf("export media scan: %w", err)
		}

		// Карантин и sha256 - как в local_media_repository Synapse.
		line := map[string]any{
			"kind": "media", "media_id": mediaID, "media_type": contentType,
			"media_length": size, "upload_name": fileName, "user_id": uploader, "created_at_ms": createdMS,
			"quarantined_by": quarantinedBy,
		}

		if len(sum) > 0 {
			line["sha256"] = hex.EncodeToString(sum)
		}

		if err := writeLine(out, line); err != nil {
			return err
		}

		report.Media++

		if e.opts.MediaSourceDir == "" || e.opts.MediaOutDir == "" {
			continue
		}

		if err := e.copyMedia(mediaID, storageKey, serverName); err != nil {
			report.MediaFailed++

			if len(e.copyErr) < 20 {
				e.copyErr = append(e.copyErr, fmt.Sprintf("%s: %v", mediaID, err))
			}

			continue
		}

		report.MediaCopied++
	}

	return rows.Err()
}

// mediaSince - параметр фильтра медиа по времени создания (NULL - все).
func mediaSince(since time.Time) any {
	if since.IsZero() {
		return nil
	}

	return since
}

// copyMedia копирует файл из нашего хранилища в раскладку Synapse (local_content/ab/cd/rest).
//
// Ключи в нашем хранилище бывают двух видов: <server>/<mediaID> (загрузка через API)
// и local_content/ab/cd/rest (импорт из Synapse). Пробуем storage_key, затем <server>/<mediaID>.
func (e *Exporter) copyMedia(mediaID, storageKey, serverName string) error {
	if len(mediaID) < 5 {
		return fmt.Errorf("media_id слишком короткий")
	}

	candidates := []string{filepath.Join(e.opts.MediaSourceDir, filepath.FromSlash(storageKey))}

	if serverName != "" {
		candidates = append(candidates, filepath.Join(e.opts.MediaSourceDir, serverName, mediaID))
	}

	var (
		data []byte
		err  error
	)

	for _, path := range candidates {
		data, err = os.ReadFile(path) //nolint:gosec // путь собран из storage_key нашей БД или server/media_id
		if err == nil {
			break
		}
	}

	if err != nil {
		return fmt.Errorf("прочитать объект (%v): %w", candidates, err)
	}

	destKey := filepath.ToSlash(filepath.Join("local_content", mediaID[0:2], mediaID[2:4], mediaID[4:]))
	dest := filepath.Join(e.opts.MediaOutDir, filepath.FromSlash(destKey))

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// 0o600: выгрузка медиа не должна быть доступна другим пользователям хоста.
	//nolint:gosec // путь собран из media_id (валидируется длиной) и каталога, заданного оператором
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return fmt.Errorf("записать %s: %w", dest, err)
	}

	return nil
}

// Text отдаёт отчёт в текстовом виде.
func (r Report) Text() string {
	var b strings.Builder

	fmt.Fprintf(&b, "export report\n")
	fmt.Fprintf(&b, "started:  %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "finished: %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "duration: %s\n", r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond))
	fmt.Fprintf(&b, "users=%d rooms=%d aliases=%d memberships=%d events=%d (excluded=%d restored=%d) state_rows=%d receipts=%d media=%d media_copied=%d media_failed=%d\n",
		r.Users, r.Rooms, r.Aliases, r.Memberships, r.Events, r.Excluded, r.Restored, r.StateRows,
		r.Receipts, r.Media, r.MediaCopied, r.MediaFailed)

	return b.String()
}
