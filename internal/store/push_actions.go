package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Хранимые действия push-правил и счетчики непрочитанного, аналог
// event_push_actions Synapse: строка - событие, для которого правила получателя
// дали notify. Счетчики - сводка event_push_summary: готовые числа
// на пользователя, комнату и тред, прочитанные строки удаляются в фоне.

// Типы хранимых действий - домена.
type (
	PushAction    = core.PushAction
	PushReadUpTo  = core.PushReadUpTo
	PushRecipient = core.PushRecipient
	PushReadMark  = core.PushReadMark
	PushBatch     = core.PushBatch
	PushCounts    = core.PushCounts
)

var (
	_ core.PushActionsStore   = (*DB)(nil)
	_ core.NotificationsStore = (*DB)(nil)
)

// PushStreamState - позиция обработки, граница досчета старых событий и
// последняя позиция событий (одним запросом).
func (db *DB) PushStreamState(ctx context.Context) (processed, backfillUpto, latest int64, err error) {
	err = db.pool.QueryRow(ctx, `
		SELECT processed, backfill_upto, (SELECT coalesce(max(stream_ordering), 0) FROM events)
		FROM event_push_stream`).Scan(&processed, &backfillUpto, &latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, 0, nil
	}

	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: push stream state: %w", err)
	}

	return processed, backfillUpto, latest, nil
}

// ResetPushStream объявляет события до position старыми: они не разбираются
// по одному, а досчитываются по пользователю, как события до миграции 0039
// (массовый импорт истории в обход API). Отметки досчета сбрасываются.
func (db *DB) ResetPushStream(ctx context.Context, position int64) error {
	_, err := db.pool.Exec(ctx, `
		WITH bf AS (DELETE FROM event_push_backfill)
		UPDATE event_push_stream SET processed = $1, backfill_upto = $1`, position)
	if err != nil {
		return fmt.Errorf("store: reset push stream: %w", err)
	}

	return nil
}

// PushEventsAfter - события с позицией в (after, upper] по возрастанию, не
// больше limit (порция обработчика push-действий).
func (db *DB) PushEventsAfter(ctx context.Context, after, upper int64, limit int) ([]core.Event, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events
		WHERE stream_ordering > $1 AND stream_ordering <= $2
		ORDER BY stream_ordering
		LIMIT $3`, after, upper, limit)
	if err != nil {
		return nil, fmt.Errorf("store: push events after %d: %w", after, err)
	}

	return scanEventRows(rows)
}

// PushRecipients - локальные (активные) участники комнат с позицией входа,
// одним запросом.
func (db *DB) PushRecipients(ctx context.Context, roomIDs []string) (map[string][]PushRecipient, error) {
	out := make(map[string][]PushRecipient, len(roomIDs))
	if len(roomIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT m.room_id, m.user_id,
		       coalesce((SELECT j.stream_ordering FROM events j WHERE j.event_id = m.event_id), 0)
		FROM room_memberships m
		JOIN users u ON u.user_id = m.user_id AND NOT u.deactivated
		WHERE m.room_id = ANY($1) AND m.membership = 'join'`, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push recipients: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID string
			r      PushRecipient
		)

		if err := rows.Scan(&roomID, &r.UserID, &r.JoinStream); err != nil {
			return nil, fmt.Errorf("store: scan push recipient: %w", err)
		}

		out[roomID] = append(out[roomID], r)
	}

	return out, rows.Err()
}

// PushReadMarks - отметки прочтения пользователей userIDs в комнатах roomIDs с
// известной позицией события (один запрос).
func (db *DB) PushReadMarks(ctx context.Context, roomIDs, userIDs []string) ([]PushReadMark, error) {
	if len(roomIDs) == 0 || len(userIDs) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT room_id, user_id, thread_id, event_stream FROM receipts
		WHERE room_id = ANY($1) AND user_id = ANY($2)
		  AND receipt_type IN ('m.read', 'm.read.private') AND event_stream IS NOT NULL`, roomIDs, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push read marks: %w", err)
	}

	defer rows.Close()

	var out []PushReadMark

	for rows.Next() {
		var m PushReadMark
		if err := rows.Scan(&m.RoomID, &m.UserID, &m.ThreadID, &m.Stream); err != nil {
			return nil, fmt.Errorf("store: scan push read mark: %w", err)
		}

		out = append(out, m)
	}

	return out, rows.Err()
}

// Запросы записи порции (ApplyPushBatch): каждый следующий видит изменения
// предыдущих, все - в одной транзакции.
const (
	// pushLeftQuery - выход из комнаты: строки и сводка комнаты пользователя.
	// Идет первым: действия порции после повторного входа сохраняются.
	pushLeftQuery = `
		WITH lv AS (
			SELECT * FROM unnest($1::text[], $2::text[]) AS l(user_id, room_id)
		), da AS (
			DELETE FROM event_push_actions a USING lv
			WHERE a.user_id = lv.user_id AND a.room_id = lv.room_id
		)
		DELETE FROM event_push_summary s USING lv
		WHERE s.user_id = lv.user_id AND s.room_id = lv.room_id`

	// pushRedactedQuery - удаленные (redaction) события: строки удаляются, сводка
	// уменьшается на непрочитанные из них.
	pushRedactedQuery = `
		WITH del AS (
			DELETE FROM event_push_actions a USING events e
			WHERE e.redacted_by = ANY($1::text[]) AND a.event_id = e.event_id
			RETURNING a.user_id, a.room_id, a.thread_id, a.stream_ordering, a.highlight
		), dd AS (
			SELECT d.user_id, d.room_id, d.thread_id, count(*) AS n, count(*) FILTER (WHERE d.highlight) AS h
			FROM del d
			JOIN event_push_summary s
			  ON s.user_id = d.user_id AND s.room_id = d.room_id AND s.thread_id = d.thread_id
			WHERE d.stream_ordering > s.read_upto
			GROUP BY d.user_id, d.room_id, d.thread_id
		)
		UPDATE event_push_summary s
		SET notif_count = greatest(s.notif_count - dd.n, 0),
		    highlight_count = greatest(s.highlight_count - dd.h, 0)
		FROM dd
		WHERE s.user_id = dd.user_id AND s.room_id = dd.room_id AND s.thread_id = dd.thread_id`

	// pushInsertQuery - новые действия и прирост сводки по реально вставленным
	// строкам (повтор события счетчик не увеличивает). Ключи сводки - по
	// порядку: блокировки строк сводки берутся в одном порядке во всех путях.
	pushInsertQuery = `
		WITH ins AS (
			INSERT INTO event_push_actions (user_id, room_id, event_id, stream_ordering, thread_id, highlight)
			SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::text[], $6::boolean[])
			ON CONFLICT (user_id, event_id) DO NOTHING
			RETURNING user_id, room_id, thread_id, highlight
		)
		INSERT INTO event_push_summary AS s (user_id, room_id, thread_id, notif_count, highlight_count)
		SELECT user_id, room_id, thread_id, count(*), count(*) FILTER (WHERE highlight)
		FROM ins
		GROUP BY user_id, room_id, thread_id
		ORDER BY user_id, room_id, thread_id
		ON CONFLICT (user_id, room_id, thread_id) DO UPDATE
		SET notif_count = s.notif_count + EXCLUDED.notif_count,
		    highlight_count = s.highlight_count + EXCLUDED.highlight_count`

	// pushReadQuery - свое событие: позиция прочтения ленты поднимается, счетчик
	// пересчитывается по строкам выше нее (обычно их нет). Строки не удаляются -
	// это делает фоновая чистка (PrunePushActions).
	pushReadQuery = `
		UPDATE event_push_summary s
		SET read_upto = m.upto,
		    (notif_count, highlight_count) = (
		        SELECT count(*), count(*) FILTER (WHERE a.highlight)
		        FROM event_push_actions a
		        WHERE a.user_id = s.user_id AND a.room_id = s.room_id AND a.thread_id = s.thread_id
		          AND a.stream_ordering > m.upto)
		FROM (
			SELECT user_id, room_id, thread_id, max(upto) AS upto
			FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[]) AS r(user_id, room_id, thread_id, upto)
			GROUP BY user_id, room_id, thread_id
		) m
		WHERE s.user_id = m.user_id AND s.room_id = m.room_id AND s.thread_id = m.thread_id
		  AND s.read_upto < m.upto AND s.notif_count > 0`

	// pushProcessedQuery - сдвиг позиции обработки или отметка досчета ($2).
	pushProcessedQuery = `
		WITH bf AS (
			INSERT INTO event_push_backfill (user_id)
			SELECT $2::text WHERE $2::text <> ''
			ON CONFLICT (user_id) DO NOTHING
		)
		UPDATE event_push_stream SET processed = greatest(processed, $1) WHERE $2::text = ''`
)

// ApplyPushBatch записывает порцию в одной транзакции (запросы пачкой, один
// обмен): выходы из комнат, удаленные события, вставка действий с приростом
// сводки event_push_summary, неявное прочтение своим событием, сдвиг позиции
// обработки. Прочтение и выход внутри порции обработчик уже учел в памяти.
//
// BackfillFor - досчет старых событий пользователя: вместо сдвига позиции
// пользователь отмечается в event_push_backfill.
func (db *DB) ApplyPushBatch(ctx context.Context, b PushBatch) error {
	batch := &pgx.Batch{}

	if len(b.Left) > 0 {
		lUser, lRoom := make([]string, 0, len(b.Left)), make([]string, 0, len(b.Left))

		for _, l := range b.Left {
			lUser = append(lUser, l.UserID)
			lRoom = append(lRoom, l.RoomID)
		}

		batch.Queue(pushLeftQuery, lUser, lRoom)
	}

	if len(b.RedactedBy) > 0 {
		batch.Queue(pushRedactedQuery, b.RedactedBy)
	}

	if n := len(b.Actions); n > 0 {
		aUser, aRoom, aEvent, aThread := make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]string, 0, n)
		aStream, aHighlight := make([]int64, 0, n), make([]bool, 0, n)

		for _, a := range b.Actions {
			aUser = append(aUser, a.UserID)
			aRoom = append(aRoom, a.RoomID)
			aEvent = append(aEvent, a.EventID)
			aStream = append(aStream, a.Stream)
			aThread = append(aThread, a.ThreadID)
			aHighlight = append(aHighlight, a.Highlight)
		}

		batch.Queue(pushInsertQuery, aUser, aRoom, aEvent, aStream, aThread, aHighlight)
	}

	if n := len(b.ReadUpTo); n > 0 {
		rUser, rRoom, rThread, rStream := make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]int64, 0, n)

		for _, r := range b.ReadUpTo {
			rUser = append(rUser, r.UserID)
			rRoom = append(rRoom, r.RoomID)
			rThread = append(rThread, r.ThreadID)
			rStream = append(rStream, r.Stream)
		}

		batch.Queue(pushReadQuery, rUser, rRoom, rThread, rStream)
	}

	batch.Queue(pushProcessedQuery, b.Processed, b.BackfillFor)

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: apply push batch begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store: apply push batch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: apply push batch commit: %w", err)
	}

	return nil
}

// MarkPushRead - receipt (m.read, m.read.private) до события eventID: во всех
// тредах (unthreaded) или в треде threadID ("" - основная лента) позиция
// прочтения сводки поднимается до события, счетчик пересчитывается по строкам
// выше нее (индекс по позиции, обычно ноль строк). Строки не удаляются (фоновая
// чистка PrunePushActions). Без force уже прочитанные ленты (позиция не ниже,
// счетчик ноль) не переписываются; force - повтор receipt после записи порции,
// разобранной до него: пересчет безусловный.
func (db *DB) MarkPushRead(ctx context.Context, userID, roomID, threadID, eventID string, unthreaded, force bool) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE event_push_summary s
		SET read_upto = greatest(s.read_upto, e.stream_ordering),
		    (notif_count, highlight_count) = (
		        SELECT count(*), count(*) FILTER (WHERE a.highlight)
		        FROM event_push_actions a
		        WHERE a.user_id = s.user_id AND a.room_id = s.room_id AND a.thread_id = s.thread_id
		          AND a.stream_ordering > greatest(s.read_upto, e.stream_ordering))
		FROM (SELECT stream_ordering FROM events WHERE event_id = $3 AND room_id = $2) e
		WHERE s.user_id = $1 AND s.room_id = $2 AND ($4 OR s.thread_id = $5)
		  AND ($6 OR (s.read_upto < e.stream_ordering AND s.notif_count > 0))`,
		userID, roomID, eventID, unthreaded, threadID, force)
	if err != nil {
		return fmt.Errorf("store: mark push read: %w", err)
	}

	return nil
}

// PrunePushActions - фоновая чистка: удаляет прочитанные строки (не выше
// позиции прочтения сводки) в комнатах пользователей keys (UserID, RoomID).
// Счетчики не меняются: они считают только строки выше позиции.
func (db *DB) PrunePushActions(ctx context.Context, keys []PushReadUpTo) error {
	if len(keys) == 0 {
		return nil
	}

	users, rooms := make([]string, 0, len(keys)), make([]string, 0, len(keys))

	for _, k := range keys {
		users = append(users, k.UserID)
		rooms = append(rooms, k.RoomID)
	}

	_, err := db.pool.Exec(ctx, `
		DELETE FROM event_push_actions a
		USING unnest($1::text[], $2::text[]) AS k(user_id, room_id), event_push_summary s
		WHERE s.user_id = k.user_id AND s.room_id = k.room_id
		  AND a.user_id = s.user_id AND a.room_id = s.room_id AND a.thread_id = s.thread_id
		  AND a.stream_ordering <= s.read_upto`, users, rooms)
	if err != nil {
		return fmt.Errorf("store: prune push actions: %w", err)
	}

	return nil
}

// PushBackfilled сообщает, досчитаны ли старые события пользователя.
func (db *DB) PushBackfilled(ctx context.Context, userID string) (bool, error) {
	var done bool

	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM event_push_backfill WHERE user_id = $1)`, userID).Scan(&done)
	if err != nil {
		return false, fmt.Errorf("store: push backfilled: %w", err)
	}

	return done, nil
}

// PushBackfillPending - активные пользователи с комнатами (join) без отметки
// досчета старых событий: досчет идет в фоне при старте.
func (db *DB) PushBackfillPending(ctx context.Context) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT u.user_id FROM users u
		WHERE NOT u.deactivated
		  AND NOT EXISTS (SELECT 1 FROM event_push_backfill b WHERE b.user_id = u.user_id)
		  AND EXISTS (SELECT 1 FROM room_memberships m WHERE m.user_id = u.user_id AND m.membership = 'join')
		ORDER BY u.user_id`)
	if err != nil {
		return nil, fmt.Errorf("store: push backfill pending: %w", err)
	}

	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: scan push backfill pending: %w", err)
	}

	return users, nil
}

// PushCountsFor - счетчики уведомлений пользователя по комнатам и тредам
// (комната -> тред -> счетчики, "" - основная лента) из сводки: один запрос по
// ключу, O(комнат), а не O(непрочитанных строк).
func (db *DB) PushCountsFor(ctx context.Context, userID string, roomIDs []string) (map[string]map[string]PushCounts, error) {
	out := map[string]map[string]PushCounts{}
	if len(roomIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT room_id, thread_id, notif_count, highlight_count
		FROM event_push_summary
		WHERE user_id = $1 AND room_id = ANY($2) AND notif_count > 0`, userID, roomIDs)
	if err != nil {
		return nil, fmt.Errorf("store: push counts: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			roomID, threadID string
			c                PushCounts
		)

		if err := rows.Scan(&roomID, &threadID, &c.Notification, &c.Highlight); err != nil {
			return nil, fmt.Errorf("store: scan push counts: %w", err)
		}

		if out[roomID] == nil {
			out[roomID] = map[string]PushCounts{}
		}

		out[roomID][threadID] = c
	}

	return out, rows.Err()
}

// PushUnreadRooms - число комнат (join) с уведомлениями: бейдж push.
func (db *DB) PushUnreadRooms(ctx context.Context, userID string) (int, error) {
	var n int

	err := db.pool.QueryRow(ctx, `
		SELECT count(DISTINCT s.room_id)
		FROM event_push_summary s
		JOIN room_memberships m ON m.room_id = s.room_id AND m.user_id = s.user_id AND m.membership = 'join'
		WHERE s.user_id = $1 AND s.notif_count > 0`, userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: push unread rooms: %w", err)
	}

	return n, nil
}
