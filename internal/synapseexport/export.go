// Package synapseexport выгружает события из БД Synapse и сверяет результат
// импорта с источником.
//
// Раньше выгрузка шла psql-циклом export-events.sql с жесткой верхней границей
// stream_ordering и без остановки на ошибке: свежие события и пачки с ошибкой
// терялись молча, а сверка шла с дампом, а не с Synapse. Здесь выгрузка идет
// одним снимком (REPEATABLE READ), по всем stream_ordering, включая
// отрицательные (backfill) и пустые, и заканчивается манифестом; любая ошибка
// прерывает выгрузку.
package synapseexport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/importevents"
)

// Querier - чтение из PostgreSQL (pgx.Tx, *pgx.Conn, *pgxpool.Pool).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options - параметры выгрузки.
type Options struct {
	// BatchSize - строк на запрос (keyset-пагинация по stream_ordering).
	BatchSize int
	// Progress - куда печатать прогресс (nil - не печатать).
	Progress io.Writer
	// Now - часы для exported_at в манифесте (тесты).
	Now func() time.Time
}

// EventLine - строка дампа kind=event. Тело события - исходный PDU строкой
// (event_json.json): из него импорт берет content, ссылки, подписи и hashes.
type EventLine struct {
	Kind           string  `json:"kind"`
	RoomID         string  `json:"room_id"`
	EventID        string  `json:"event_id"`
	Type           string  `json:"type"`
	Sender         string  `json:"sender"`
	StateKey       *string `json:"state_key"`
	OriginTS       int64   `json:"origin_server_ts"`
	Depth          int64   `json:"depth"`
	Stream         *int64  `json:"stream_ordering"`
	Outlier        bool    `json:"outlier"`
	RejectedReason *string `json:"rejected_reason,omitempty"`
	SoftFailed     bool    `json:"soft_failed,omitempty"`
	FormatVersion  *int64  `json:"format_version,omitempty"`
	JSON           string  `json:"json"`
}

// ErrSnapshotChanged - число выгруженных событий не совпало с подсчетом в том
// же снимке (выгрузка шла не в REPEATABLE READ или источник менялся).
var ErrSnapshotChanged = errors.New("synapseexport: число событий изменилось во время выгрузки")

// ExportEvents выгружает все события Synapse в JSONL и дописывает манифест.
// q должен быть транзакцией REPEATABLE READ (см. ExportSnapshot), иначе
// параллельная запись в Synapse даст расхождение с подсчетом.
func ExportEvents(ctx context.Context, q Querier, w io.Writer, opts Options) (importevents.Manifest, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10_000
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	manifest := importevents.Manifest{Kind: "manifest"}

	var minStream, maxStream *int64

	if err := q.QueryRow(ctx, `
		SELECT count(*), min(stream_ordering), max(stream_ordering),
		       count(*) FILTER (WHERE stream_ordering IS NULL), count(DISTINCT room_id)
		FROM events`).Scan(&manifest.Events, &minStream, &maxStream, &manifest.NullStream, &manifest.Rooms); err != nil {
		return manifest, fmt.Errorf("synapseexport: подсчет событий: %w", err)
	}

	if minStream != nil {
		manifest.MinStream, manifest.MaxStream = *minStream, *maxStream
	}

	query, err := eventsQuery(ctx, q)
	if err != nil {
		return manifest, err
	}

	out := bufio.NewWriterSize(w, 1<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	var written int64

	emit := func(line *EventLine) error {
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("synapseexport: запись %s: %w", line.EventID, err)
		}

		written++

		if opts.Progress != nil && written%100_000 == 0 {
			fmt.Fprintf(opts.Progress, "events: %d/%d\n", written, manifest.Events)
		}

		return nil
	}

	// Курсор с минимума int64: отрицательные stream_ordering (backfill) тоже
	// попадают в выгрузку, верхней границы нет.
	cursor := int64(math.MinInt64)

	for {
		lines, err := fetchEvents(ctx, q, query+`
			WHERE e.stream_ordering > $1
			ORDER BY e.stream_ordering
			LIMIT $2`, cursor, opts.BatchSize)
		if err != nil {
			return manifest, err
		}

		for i := range lines {
			if err := emit(&lines[i]); err != nil {
				return manifest, err
			}
		}

		if len(lines) < opts.BatchSize {
			break
		}

		cursor = *lines[len(lines)-1].Stream
	}

	// События без stream_ordering (в схеме Synapse колонка допускает NULL).
	lastID := ""

	for {
		lines, err := fetchEvents(ctx, q, query+`
			WHERE e.stream_ordering IS NULL AND e.event_id COLLATE "C" > $1
			ORDER BY e.event_id COLLATE "C"
			LIMIT $2`, lastID, opts.BatchSize)
		if err != nil {
			return manifest, err
		}

		for i := range lines {
			if err := emit(&lines[i]); err != nil {
				return manifest, err
			}
		}

		if len(lines) < opts.BatchSize {
			break
		}

		lastID = lines[len(lines)-1].EventID
	}

	if written != manifest.Events {
		return manifest, fmt.Errorf("%w: подсчитано %d, выгружено %d", ErrSnapshotChanged, manifest.Events, written)
	}

	// Текущее состояние комнат - как его держит Synapse (current_state_events,
	// материализация state groups), а не пересчет у нас. Строки
	// комнаты идут подряд: импорт заменяет состояние комнаты целиком.
	states, err := exportState(ctx, q, enc)
	if err != nil {
		return manifest, err
	}

	manifest.StateRows = states

	manifest.ExportedAt = opts.Now().UTC().Format(time.RFC3339)

	if err := enc.Encode(manifest); err != nil {
		return manifest, fmt.Errorf("synapseexport: манифест: %w", err)
	}

	if err := out.Flush(); err != nil {
		return manifest, fmt.Errorf("synapseexport: запись дампа: %w", err)
	}

	return manifest, nil
}

// StateLine - строка дампа kind=state.
type StateLine struct {
	Kind     string `json:"kind"`
	RoomID   string `json:"room_id"`
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
	EventID  string `json:"event_id"`
}

func exportState(ctx context.Context, q Querier, enc *json.Encoder) (int64, error) {
	rows, err := q.Query(ctx, `
		SELECT room_id, type, state_key, event_id FROM current_state_events
		ORDER BY room_id COLLATE "C", type COLLATE "C", state_key COLLATE "C"`)
	if err != nil {
		return 0, fmt.Errorf("synapseexport: current_state_events: %w", err)
	}

	defer rows.Close()

	var written int64

	for rows.Next() {
		line := StateLine{Kind: "state"}
		if err := rows.Scan(&line.RoomID, &line.Type, &line.StateKey, &line.EventID); err != nil {
			return written, fmt.Errorf("synapseexport: чтение состояния: %w", err)
		}

		if err := enc.Encode(&line); err != nil {
			return written, fmt.Errorf("synapseexport: запись состояния: %w", err)
		}

		written++
	}

	if err := rows.Err(); err != nil {
		return written, fmt.Errorf("synapseexport: current_state_events: %w", err)
	}

	return written, nil
}

// eventsQuery собирает SELECT событий. Причина отклонения в новых версиях
// Synapse лежит в events.rejection_reason, в старых - только в rejections.
func eventsQuery(ctx context.Context, q Querier) (string, error) {
	reason, err := rejectionExpr(ctx, q)
	if err != nil {
		return "", err
	}

	// json и internal_metadata берутся текстом: приведение к jsonb падает на
	// \u0000 в content (так раньше терялась вся пачка).
	return `
		SELECT e.event_id, e.room_id, e.type, coalesce(e.sender, ''), e.state_key,
		       coalesce(e.origin_server_ts, 0), e.depth, e.stream_ordering, e.outlier,
		       ` + reason + `, j.json, j.internal_metadata, j.format_version::bigint
		FROM events e
		LEFT JOIN event_json j ON j.event_id = e.event_id
		LEFT JOIN rejections rj ON rj.event_id = e.event_id`, nil
}

// rejectionExpr - выражение причины отклонения для events e LEFT JOIN rejections rj.
func rejectionExpr(ctx context.Context, q Querier) (string, error) {
	hasColumn, err := hasColumn(ctx, q, "events", "rejection_reason")
	if err != nil {
		return "", err
	}

	if hasColumn {
		return "coalesce(e.rejection_reason, rj.reason)", nil
	}

	return "rj.reason", nil
}

// hasColumn - есть ли колонка в таблице текущей схемы (схема Synapse меняется от версии к версии).
func hasColumn(ctx context.Context, q Querier, table, column string) (bool, error) {
	var exists bool

	if err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, table, column).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("synapseexport: схема %s: %w", table, err)
	}

	return exists, nil
}

func fetchEvents(ctx context.Context, q Querier, query string, args ...any) ([]EventLine, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("synapseexport: выборка событий: %w", err)
	}

	defer rows.Close()

	var lines []EventLine

	for rows.Next() {
		line := EventLine{Kind: "event"}

		var body, metadata *string

		if err := rows.Scan(&line.EventID, &line.RoomID, &line.Type, &line.Sender, &line.StateKey,
			&line.OriginTS, &line.Depth, &line.Stream, &line.Outlier, &line.RejectedReason,
			&body, &metadata, &line.FormatVersion); err != nil {
			return nil, fmt.Errorf("synapseexport: чтение события: %w", err)
		}

		if body != nil {
			line.JSON = *body
		}

		if metadata != nil {
			var meta struct {
				SoftFailed bool `json:"soft_failed"`
			}

			// Битые метаданные не повод терять событие: флага просто нет.
			if json.Unmarshal([]byte(*metadata), &meta) == nil {
				line.SoftFailed = meta.SoftFailed
			}
		}

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("synapseexport: выборка событий: %w", err)
	}

	return lines, nil
}

// ExportSnapshot открывает на conn транзакцию REPEATABLE READ READ ONLY и
// выгружает события одним снимком.
func ExportSnapshot(ctx context.Context, conn *pgx.Conn, w io.Writer, opts Options) (importevents.Manifest, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return importevents.Manifest{}, fmt.Errorf("synapseexport: снимок: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	return ExportEvents(ctx, tx, w, opts)
}
