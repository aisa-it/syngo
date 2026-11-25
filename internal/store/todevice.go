package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// ToDeviceEvent — одно to-device сообщение из очереди (тип - в core).
type ToDeviceEvent = core.ToDeviceEvent

// DeviceTarget — адрес доставки: устройство получателя (тип - в core).
type DeviceTarget = core.DeviceTarget

var (
	_ core.ToDeviceStore  = (*DB)(nil)
	_ core.SyncQueueStore = (*DB)(nil)
)

// SendToDevice пишет сообщение в очередь устройств. Идемпотентность — по txn_id:
// повторный PUT с тем же txn возвращает alreadyProcessed = true и ничего не пишет.
func (db *DB) SendToDevice(
	ctx context.Context,
	userID, deviceID, eventType, txnID string,
	messages map[DeviceTarget]json.RawMessage,
) (bool, error) {
	return db.SendToDeviceFederated(ctx, userID, deviceID, eventType, txnID, messages, nil)
}

var _ core.FederatedToDeviceStore = (*DB)(nil)

// SendToDeviceFederated - SendToDevice плюс EDU m.direct_to_device получателям
// других серверов (edus: destination -> EDU) в той же транзакции: повтор txn_id
// не шлет EDU второй раз, обрыв после записи не теряет его.
func (db *DB) SendToDeviceFederated(
	ctx context.Context,
	userID, deviceID, eventType, txnID string,
	messages map[DeviceTarget]json.RawMessage,
	edus map[string][]byte,
) (bool, error) {
	if db == nil || db.pool == nil {
		return false, ErrNotConfigured
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: to-device begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// transactions — общая идемпотентность клиентских PUT-ов (endpoint различает типы).
	const txnQuery = `
		INSERT INTO transactions (user_id, device_id, endpoint, txn_id)
		VALUES ($1, $2, 'sendToDevice:' || $3, $4)
		ON CONFLICT DO NOTHING`

	tag, err := tx.Exec(ctx, txnQuery, userID, deviceID, eventType, txnID)
	if err != nil {
		return false, fmt.Errorf("store: to-device txn: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return true, nil
	}

	const insert = `
		INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
		VALUES ($1, $2, $3, $4, $5)`

	for target, content := range messages {
		// device_id "*" — все устройства пользователя (запросы верификации,
		// m.secret.request). Раньше он писался как устройство с именем «*» и не
		// доставлялся никому.
		if target.DeviceID == "*" {
			devices, err := userDevices(ctx, tx, target.UserID)
			if err != nil {
				return false, err
			}

			for _, deviceID := range devices {
				if _, err := tx.Exec(ctx, insert, target.UserID, deviceID, eventType, userID, content); err != nil {
					return false, fmt.Errorf("store: to-device insert %s/%s: %w", target.UserID, deviceID, err)
				}
			}

			continue
		}

		if _, err := tx.Exec(ctx, insert, target.UserID, target.DeviceID, eventType, userID, content); err != nil {
			return false, fmt.Errorf("store: to-device insert %s/%s: %w", target.UserID, target.DeviceID, err)
		}
	}

	for destination, edu := range edus {
		if !db.outboxDestinationAllowed(destination) {
			continue
		}

		if _, err := tx.Exec(ctx, `INSERT INTO federation_outbox (destination, edu_json) VALUES ($1, $2::jsonb)`,
			destination, string(edu)); err != nil {
			return false, fmt.Errorf("store: to-device edu %s: %w", destination, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: to-device commit: %w", err)
	}

	return false, nil
}

// userDevices возвращает все устройства пользователя (для адресации "*").
func userDevices(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT device_id FROM devices WHERE user_id = $1 ORDER BY device_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: devices of %s: %w", userID, err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var deviceID string
		if err := rows.Scan(&deviceID); err != nil {
			return nil, fmt.Errorf("store: scan device of %s: %w", userID, err)
		}

		out = append(out, deviceID)
	}

	return out, rows.Err()
}

// PendingToDevice отдаёт недоставленные сообщения устройства после позиции since.
func (db *DB) PendingToDevice(ctx context.Context, userID, deviceID string, since int64, limit int) ([]ToDeviceEvent, error) {
	if db == nil || db.pool == nil {
		return nil, ErrNotConfigured
	}

	if limit <= 0 || limit > 100 {
		limit = 100
	}

	const query = `
		SELECT id, event_type, sender, content, stream_ordering
		FROM device_inbox
		WHERE user_id = $1 AND device_id = $2 AND stream_ordering > $3
		ORDER BY stream_ordering
		LIMIT $4`

	rows, err := db.pool.Query(ctx, query, userID, deviceID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending to-device: %w", err)
	}

	defer rows.Close()

	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ToDeviceEvent, error) {
		var ev ToDeviceEvent

		return ev, row.Scan(&ev.ID, &ev.Type, &ev.Sender, &ev.Content, &ev.StreamPos)
	})
	if err != nil {
		return nil, fmt.Errorf("store: collect to-device: %w", err)
	}

	return events, nil
}

// DeleteToDevice удаляет доставленные сообщения (доставка одноразовая).
func (db *DB) DeleteToDevice(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	if _, err := db.pool.Exec(ctx, `DELETE FROM device_inbox WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: delete to-device: %w", err)
	}

	return nil
}

// RecordDeviceKeysChange фиксирует изменение ключей устройства (для /keys/changes).
func (db *DB) RecordDeviceKeysChange(ctx context.Context, userID, deviceID string) error {
	const query = `INSERT INTO device_lists_changes (user_id, device_id) VALUES ($1, $2)`

	if _, err := db.pool.Exec(ctx, query, userID, deviceID); err != nil {
		return fmt.Errorf("store: record device keys change: %w", err)
	}

	return nil
}

// DeviceListChangesFor возвращает пользователей, чьи device-листы менялись в
// окне (since, upper] и которые видны userID: сам пользователь и те, с кем у него
// есть общая комната. Плюс все участники комнат, куда userID вошел в
// этом окне: при join пишется одна строка на вступившего, а не по строке на
// каждого участника комнаты - круг получателей считается здесь.
//
// Участники других серверов есть только в room_state (членство из /send не
// пишется в room_memberships), поэтому соседи ищутся и по нему:
// иначе изменения их устройств из m.device_list_update не доходили до клиента.
//
// Фильтр по общим комнатам в SQL: раньше читалась вся таблица изменений после
// since (Seq Scan, 165 мс на 200 тыс. строк) и отдельным запросом - все соседи
// пользователя по комнатам (55-90 мс у пользователя с 1000 комнат). Окно
// (since, upper] берется по индексу device_lists_changes_pos_idx (миграция
// 0032), членство изменившихся - по индексу user_id с проверкой по комнатам
// пользователя, вход самого пользователя - по позициям его событий членства:
// 5-10 мс у пользователя с 50 комнатами, 18-38 мс с 1000 комнатами на окне в 20
// изменений (синтетика 1 млн событий).
func (db *DB) DeviceListChangesFor(ctx context.Context, userID string, since, upper int64) ([]string, error) {
	const query = `
		WITH changed AS MATERIALIZED (
			SELECT DISTINCT user_id FROM device_lists_changes
			WHERE stream_ordering > $2 AND stream_ordering <= $3
		), mine AS MATERIALIZED (
			SELECT room_id, event_id FROM room_memberships WHERE user_id = $1 AND membership = 'join'
		), joined AS MATERIALIZED (
			SELECT m.room_id FROM mine m
			JOIN events je ON je.event_id = m.event_id
			WHERE je.stream_ordering > $2 AND je.stream_ordering <= $3
		)
		SELECT ch.user_id FROM changed ch WHERE ch.user_id = $1
		UNION
		SELECT ch.user_id FROM changed ch
		CROSS JOIN LATERAL (
			SELECT 1 FROM room_memberships other
			WHERE other.user_id = ch.user_id AND other.membership = 'join'
			  AND other.room_id IN (SELECT room_id FROM mine)
			LIMIT 1
		) shared
		UNION
		SELECT other.user_id FROM joined j
		JOIN room_memberships other ON other.room_id = j.room_id AND other.membership = 'join'
		UNION
		SELECT ch.user_id FROM changed ch
		WHERE EXISTS (
			SELECT 1 FROM room_state rs JOIN events e ON e.event_id = rs.event_id
			WHERE rs.type = 'm.room.member' AND rs.state_key = ch.user_id
			  AND rs.room_id IN (SELECT room_id FROM mine)
			  AND e.content->>'membership' = 'join')
		UNION
		SELECT rs.state_key FROM joined j
		JOIN room_state rs ON rs.room_id = j.room_id AND rs.type = 'm.room.member'
		JOIN events e ON e.event_id = rs.event_id
		WHERE e.content->>'membership' = 'join'
		ORDER BY 1`

	rows, err := db.pool.Query(ctx, query, userID, since, upper)
	if err != nil {
		return nil, fmt.Errorf("store: device list changes: %w", err)
	}

	defer rows.Close()

	users := []string{}

	for rows.Next() {
		var changed string
		if err := rows.Scan(&changed); err != nil {
			return nil, fmt.Errorf("store: scan device list change: %w", err)
		}

		users = append(users, changed)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate device list changes: %w", err)
	}

	return users, nil
}

// SyncPosition — максимальная позиция по событиям, to-device и изменениям ключей.
// Используется как next_batch: иначе to-device, записанный после последнего события,
// выпадал бы из окна синхронизации.
func (db *DB) SyncPosition(ctx context.Context) (int64, error) {
	const query = `
		SELECT greatest(
			coalesce((SELECT max(stream_ordering) FROM events), 0),
			coalesce((SELECT max(stream_ordering) FROM device_inbox), 0),
			coalesce((SELECT max(stream_ordering) FROM device_lists_changes), 0),
			coalesce((SELECT max(stream_ordering) FROM account_data), 0)
		)`

	var position int64

	if err := db.pool.QueryRow(ctx, query).Scan(&position); err != nil {
		return 0, fmt.Errorf("store: sync position: %w", err)
	}

	// Безопасная позиция: stream_ordering выдаёт nextval внутри транзакции, и
	// транзакция с меньшим номером может закоммититься позже. Отдавать такую
	// дыру в next_batch нельзя — событие выпадет навсегда. Поэтому
	// ограничиваем позицию минимумом ещё не видимых (незакоммиченных) номеров.
	return db.safePosition(ctx, position)
}

// MinPendingToDevice возвращает минимальную позицию недоставленного сообщения устройства.
// ok=false — очередь устройства пуста. Нужно для /sync: next_batch не должен уходить
// выше недоставленного to-device, иначе остаток пачки станет недостижим (следующий sync
// придёт с since больше их stream_ordering).
func (db *DB) MinPendingToDevice(ctx context.Context, userID, deviceID string) (pos int64, ok bool, err error) {
	const query = `
		SELECT coalesce(min(stream_ordering), 0) FROM device_inbox
		WHERE user_id = $1 AND device_id = $2`

	if err := db.pool.QueryRow(ctx, query, userID, deviceID).Scan(&pos); err != nil {
		return 0, false, fmt.Errorf("store: min pending to-device: %w", err)
	}

	return pos, pos > 0, nil
}

// safePosition ограничивает max-позицию снизу: если какой-то выданный
// stream_ordering ещё не виден (транзакция не закоммичена), next_batch не должен
// его перескакивать. Невидимые номера регистрируются при вставке (см.
// registerPendingPosition), проверяются и снимаются здесь же.
// pendingPositionTTL — сколько незакоммиченный номер может ограничивать
// безопасную позицию. Позиция в памяти процесса: при нескольких инстансах
// номера чужих транзакций здесь не видны — стенд и prod запускаются в одном
// экземпляре (см. ADR-0005).
// TTL заметно больше максимального времени транзакции:
// событие, закоммиченное дольше 5 минут, физически не живёт дольше — значит,
// номер залип (падение/откат без снятия), и держать из-за него /sync нельзя.
const pendingPositionTTL = 5 * time.Minute

func (db *DB) safePosition(ctx context.Context, maxPosition int64) (int64, error) {
	safe := maxPosition

	candidates := db.pendingSnapshot()
	if len(candidates) == 0 {
		return safe, nil
	}

	// Один запрос на все номера, а не EXISTS на каждый.
	rows, err := db.pool.Query(ctx,
		`SELECT stream_ordering FROM events WHERE stream_ordering = ANY($1)`, candidates)
	if err != nil {
		// Проверка не удалась — ведём себя КОНСЕРВАТИВНО: считаем все номера
		// невидимыми и не пускаем next_batch за минимальный из них. Возврат
		// maxPosition здесь был бы «оптимистичной» позицией (потеря событий при
		// сбое БД).
		return minCandidate(safe, candidates), nil //nolint:nilerr // консервативно: позиция не растёт
	}

	visible := make(map[int64]struct{}, len(candidates))

	for rows.Next() {
		var pos int64
		if err := rows.Scan(&pos); err == nil {
			visible[pos] = struct{}{}
		}
	}

	rows.Close()

	for _, candidate := range candidates {
		if _, ok := visible[candidate]; ok {
			db.forgetPendingPosition(candidate)

			continue
		}

		if candidate > maxPosition {
			continue
		}

		if candidate-1 < safe {
			safe = candidate - 1
		}
	}

	if safe < 0 {
		safe = 0
	}

	return safe, nil
}

// minCandidate — позиция перед минимальным из номеров (консервативный откат).
func minCandidate(safe int64, candidates []int64) int64 {
	for _, candidate := range candidates {
		if candidate-1 < safe {
			safe = candidate - 1
		}
	}

	if safe < 0 {
		return 0
	}

	return safe
}

// registerPendingPosition запоминает выданный, но ещё не закоммиченный номер.
func (db *DB) registerPendingPosition(position int64) {
	db.pendingMu.Lock()
	defer db.pendingMu.Unlock()

	if db.pending == nil {
		db.pending = map[int64]time.Time{}
	}

	db.pending[position] = time.Now()
}

func (db *DB) forgetPendingPosition(position int64) {
	db.pendingMu.Lock()
	defer db.pendingMu.Unlock()

	delete(db.pending, position)
}

func (db *DB) pendingSnapshot() []int64 {
	db.pendingMu.Lock()
	defer db.pendingMu.Unlock()

	out := make([]int64, 0, len(db.pending))

	for position, registeredAt := range db.pending {
		// Страховка от «залипших» номеров: если регистрация провисела дольше
		// pendingPositionTTL и событие так и не появилось, считаем её утечкой
		// (откат без снятия, падение процесса) и перестаём ограничивать позицию.
		if time.Since(registeredAt) > pendingPositionTTL {
			delete(db.pending, position)

			continue
		}

		out = append(out, position)
	}

	return out
}

// RegisterPendingForTest регистрирует номер как «выданный, но не закоммиченный».
// Только для интеграционных тестов безопасной позиции /sync.
func (db *DB) RegisterPendingForTest(position int64) {
	db.registerPendingPosition(position)
}

// ExpirePendingForTest состаривает все регистрации — проверка TTL в тестах.
func (db *DB) ExpirePendingForTest() {
	db.pendingMu.Lock()
	defer db.pendingMu.Unlock()

	old := time.Now().Add(-2 * pendingPositionTTL)
	for position := range db.pending {
		db.pending[position] = old
	}
}

// TransactionIDsFor - txn_id, с которыми устройство отправило события eventIDs:
// unsigned.transaction_id для local echo отправителя. Один запрос.
func (db *DB) TransactionIDsFor(ctx context.Context, userID, deviceID string, eventIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT event_id, txn_id FROM transactions
		WHERE event_id = ANY($3::text[]) AND user_id = $1 AND device_id = $2`, userID, deviceID, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("store: transaction ids: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var eventID, txnID string
		if err := rows.Scan(&eventID, &txnID); err != nil {
			return nil, fmt.Errorf("store: scan transaction id: %w", err)
		}

		out[eventID] = txnID
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: transaction ids rows: %w", err)
	}

	return out, nil
}

// DeviceListLeft - device_lists.left для /sync: кто из участников
// покинутых пользователем комнат (leftRooms) и вышедших из его комнат (leavers)
// больше не делит с ним ни одной комнаты с join. Один запрос.
func (db *DB) DeviceListLeft(ctx context.Context, userID string, leftRooms, leavers []string) ([]string, error) {
	if len(leftRooms) == 0 && len(leavers) == 0 {
		return nil, nil
	}

	const query = `
		WITH candidates AS (
			SELECT m.user_id FROM room_memberships m
			WHERE m.room_id = ANY($2::text[]) AND m.membership = 'join'
			UNION
			SELECT unnest($3::text[])
		)
		SELECT c.user_id FROM candidates c
		WHERE c.user_id <> $1
		  AND NOT EXISTS (
			SELECT 1 FROM room_memberships a
			JOIN room_memberships b ON b.room_id = a.room_id
			WHERE a.user_id = $1 AND a.membership = 'join'
			  AND b.user_id = c.user_id AND b.membership = 'join')
		ORDER BY c.user_id`

	rows, err := db.pool.Query(ctx, query, userID, leftRooms, leavers)
	if err != nil {
		return nil, fmt.Errorf("store: device list left: %w", err)
	}

	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: device list left: %w", err)
	}

	return users, nil
}
