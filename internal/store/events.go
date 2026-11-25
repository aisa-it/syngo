package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// EventStore — хранение событий. Домен и транспорты зависят от интерфейса, а не от pgx.
type EventStore interface {
	// AppendEvent пишет событие и возвращает его с назначенным stream_ordering.
	AppendEvent(ctx context.Context, ev core.Event) (core.Event, error)
	// RoomEventsSince отдаёт события комнаты со stream_ordering > since в возрастающем порядке.
	RoomEventsSince(ctx context.Context, roomID string, since int64, limit int) ([]core.Event, error)
	// UpsertRoomState обновляет materialized-состояние комнаты.
	UpsertRoomState(ctx context.Context, ref core.StateRef) error
	// RoomState отдаёт текущее состояние комнаты, упорядоченное по (type, state_key).
	RoomState(ctx context.Context, roomID string) ([]core.StateRef, error)
}

// interface guard: *DB реализует EventStore.
var _ EventStore = (*DB)(nil)

// AppendEvent пишет событие; stream_ordering назначает БД (sequence), поэтому он
// идемпотентен относительно порядка вызовов, а не времени на клиенте.
func (db *DB) AppendEvent(ctx context.Context, ev core.Event) (core.Event, error) {
	if db == nil || db.pool == nil {
		return core.Event{}, ErrNotConfigured
	}

	// nil-слайс pgx передаёт как NULL, а колонки NOT NULL DEFAULT '{}'.
	if ev.PrevEvents == nil {
		ev.PrevEvents = []string{}
	}

	if ev.AuthEvents == nil {
		ev.AuthEvents = []string{}
	}

	const query = `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content,
		                    prev_events, auth_events, depth, origin_server_ts, origin, pdu_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING stream_ordering, received_at`

	if err := db.pool.QueryRow(ctx, query,
		ev.EventID, ev.RoomID, ev.Type, ev.Sender, ev.StateKey, []byte(ev.Content),
		ev.PrevEvents, ev.AuthEvents, ev.Depth, ev.OriginServerTS, ev.Origin, nullText(string(ev.PDU)),
	).Scan(&ev.StreamOrdering, new(any)); err != nil {
		return core.Event{}, fmt.Errorf("store: append event %s: %w", ev.EventID, err)
	}

	return ev, nil
}

// RoomEventsSince возвращает события комнаты после курсора since в порядке
// поступления (stream_ordering). Это контракт /sync и /messages.
func (db *DB) RoomEventsSince(ctx context.Context, roomID string, since int64, limit int) ([]core.Event, error) {
	if db == nil || db.pool == nil {
		return nil, ErrNotConfigured
	}

	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	const query = `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events
		WHERE room_id = $1 AND stream_ordering > $2 AND NOT outlier
		ORDER BY stream_ordering
		LIMIT $3`

	rows, err := db.pool.Query(ctx, query, roomID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: room events since %d: %w", since, err)
	}
	defer rows.Close()

	var out []core.Event

	for rows.Next() {
		var ev core.Event
		var content []byte

		if err := rows.Scan(&ev.EventID, &ev.RoomID, &ev.Type, &ev.Sender, &ev.StateKey, &content,
			&ev.PrevEvents, &ev.AuthEvents, &ev.Depth, &ev.OriginServerTS,
			&ev.Origin, &ev.StreamOrdering, &ev.RedactedBy); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}

		ev.Content = content
		out = append(out, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}

	return out, nil
}

// UpsertRoomState обновляет текущее состояние комнаты (UNIQUE (room_id, type, state_key)).
func (db *DB) UpsertRoomState(ctx context.Context, ref core.StateRef) error {
	if db == nil || db.pool == nil {
		return ErrNotConfigured
	}

	const query = `
		INSERT INTO room_state (room_id, type, state_key, event_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (room_id, type, state_key)
		DO UPDATE SET event_id = EXCLUDED.event_id, updated_at = now()`

	if _, err := db.pool.Exec(ctx, query, ref.RoomID, ref.Type, ref.StateKey, ref.EventID); err != nil {
		return fmt.Errorf("store: upsert room state %s/%s: %w", ref.Type, ref.StateKey, err)
	}

	return nil
}

// RoomState отдаёт materialized-состояние комнаты.
func (db *DB) RoomState(ctx context.Context, roomID string) ([]core.StateRef, error) {
	if db == nil || db.pool == nil {
		return nil, ErrNotConfigured
	}

	const query = `
		SELECT room_id, type, state_key, event_id
		FROM room_state
		WHERE room_id = $1
		ORDER BY type, state_key`

	rows, err := db.pool.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: room state: %w", err)
	}
	defer rows.Close()

	refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.StateRef, error) {
		var ref core.StateRef
		err := row.Scan(&ref.RoomID, &ref.Type, &ref.StateKey, &ref.EventID)

		return ref, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: collect room state: %w", err)
	}

	return refs, nil
}

// EventRow — событие для батч-вставки при импорте истории.
type EventRow struct {
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
	StreamOrdering int64
	// PDUJSON - исходный PDU текстом (event_json.json Synapse), Signatures и
	// Hashes - его подписи и hashes. Пусто - не заполняются.
	PDUJSON    string
	Signatures []byte
	Hashes     []byte
	// Redacts - цель, если событие - m.room.redaction: запись в redactions,
	// применение - ApplyPendingRedactions после импорта.
	Redacts string
}

// SyncStreamOrdering поднимает sequence events_stream_ordering_seq до максимума,
// который уже есть в таблице (или оставляет как есть, если он больше).
//
// Без этого после импорта истории новый локальный /send получил бы nextval меньше
// импортированных значений: сломались бы порядок /messages и since-токены /sync.
func (db *DB) SyncStreamOrdering(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return ErrNotConfigured
	}

	const query = `
		SELECT setval(
			'events_stream_ordering_seq',
			greatest(
				(SELECT coalesce(max(stream_ordering), 0) FROM events),
				(SELECT last_value FROM events_stream_ordering_seq)
			)
		)`

	if _, err := db.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("store: sync stream ordering sequence: %w", err)
	}

	return nil
}

// InsertEvents пишет пачку событий и возвращает число реально вставленных строк.
// Конфликты по event_id пропускаются: повторный импорт идемпотентен.
func (db *DB) InsertEvents(ctx context.Context, events []EventRow) (int64, error) {
	if db == nil || db.pool == nil {
		return 0, ErrNotConfigured
	}

	if len(events) == 0 {
		return 0, nil
	}

	// stream_ordering из дампа передаём явно; если его нет (NULL) — берём nextval,
	// иначе событие оказалось бы «до» всех остальных.
	const query = `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content,
		                    prev_events, auth_events, depth, origin_server_ts, origin, stream_ordering,
		                    pdu_json, signatures, hashes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		        coalesce($12, nextval('events_stream_ordering_seq')), $13, $14, $15)
		ON CONFLICT (event_id) DO NOTHING`

	// auth-цепочка (event_auth) и связь redaction с целью - в той же пачке:
	// раньше event_auth при импорте не заполнялась, а redaction
	// ни на что не ссылалась. received_at redaction - время события: по нему
	// PurgeRedactedOriginals стирает оригинал по redaction.retention_period.
	const authQuery = `
		INSERT INTO event_auth (event_id, auth_event_id)
		SELECT $1, a FROM unnest($2::text[]) AS a
		ON CONFLICT DO NOTHING`

	const redactionQuery = `
		INSERT INTO redactions (event_id, room_id, redacts, received_at)
		VALUES ($1, $2, $3, to_timestamp($4::double precision / 1000))
		ON CONFLICT (event_id) DO NOTHING`

	batch := &pgx.Batch{}
	kinds := make([]bool, 0, len(events)) // true - INSERT события (его RowsAffected считаем)

	for i := range events {
		ev := &events[i]

		prev := ev.PrevEvents
		if prev == nil {
			prev = []string{}
		}

		auth := ev.AuthEvents
		if auth == nil {
			auth = []string{}
		}

		// Отрицательный stream_ordering - backfill Synapse: сохраняется как есть,
		// такие события стоят в начале истории комнаты.
		var stream any
		if ev.StreamOrdering != 0 {
			stream = ev.StreamOrdering
		}

		batch.Queue(query, ev.EventID, ev.RoomID, ev.Type, ev.Sender, ev.StateKey, ev.Content,
			prev, auth, ev.Depth, ev.OriginServerTS, ev.Origin, stream,
			nullText(ev.PDUJSON), nullJSON(ev.Signatures), nullJSON(ev.Hashes))
		kinds = append(kinds, true)

		if len(auth) > 0 {
			batch.Queue(authQuery, ev.EventID, auth)
			kinds = append(kinds, false)
		}

		if ev.Redacts != "" {
			batch.Queue(redactionQuery, ev.EventID, ev.RoomID, ev.Redacts, ev.OriginServerTS)
			kinds = append(kinds, false)
		}
	}

	results := db.pool.SendBatch(ctx, batch)

	var inserted int64

	for _, isEvent := range kinds {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()

			return inserted, fmt.Errorf("store: insert events batch: %w", err)
		}

		if isEvent {
			inserted += tag.RowsAffected()
		}
	}

	if err := results.Close(); err != nil {
		return inserted, fmt.Errorf("store: close events batch: %w", err)
	}

	return inserted, nil
}

// nullText - пустая строка как NULL.
func nullText(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// nullJSON - пустой JSON как NULL (колонка jsonb).
func nullJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}

	return string(value)
}
