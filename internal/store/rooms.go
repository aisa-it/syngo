package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// RoomStore — реализация core.RoomStore поверх PostgreSQL.
type RoomStore struct {
	db *DB
}

// interface guard: RoomStore реализует доменный контракт.
var (
	_ core.RoomStore        = (*RoomStore)(nil)
	_ core.RoomBlockChecker = (*RoomStore)(nil)
	_ core.DirectoryStore   = (*RoomStore)(nil)
)

// RoomBlocked - блокировка комнаты admin API для доменного сервиса.
func (s *RoomStore) RoomBlocked(ctx context.Context, roomID string) (bool, error) {
	return s.db.RoomBlocked(ctx, roomID)
}

// PublicRoomsDirectory - каталог публичных комнат для доменного сервиса
// (поля).
func (s *RoomStore) PublicRoomsDirectory(ctx context.Context) ([]core.PublicRoom, error) {
	return s.db.PublicRoomsDirectory(ctx)
}

// NewRoomStore создаёт хранилище комнат.
func NewRoomStore(db *DB) *RoomStore {
	return &RoomStore{db: db}
}

// CreateRoom атомарно создаёт комнату, стартовые события, состояние и членство.
func (s *RoomStore) CreateRoom(ctx context.Context, room core.Room, events []core.Event) error {
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin create room: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	const insertRoom = `
		INSERT INTO rooms (room_id, room_version, creator, is_public)
		VALUES ($1, $2, $3, $4)`

	// rooms.room_version в Synapse — текст, поэтому версию храним строкой и парсим обратно.
	if _, err := tx.Exec(ctx, insertRoom, room.RoomID, strconv.Itoa(room.RoomVersion), room.Creator, room.IsPublic); err != nil {
		return fmt.Errorf("store: insert room %s: %w", room.RoomID, err)
	}

	var stateEvents, membershipEvents int

	for i := range events {
		event := &events[i]
		if _, err := insertEvent(ctx, tx, *event); err != nil {
			return err
		}

		// Группа состояния до побочных эффектов: запись room_state ее снимает.
		base, err := readLocalStateBase(ctx, tx, room.RoomID)
		if err != nil {
			return err
		}

		if event.IsState() {
			stateEvents++
		}

		if event.Type == "m.room.member" {
			membershipEvents++
		}

		if err := applyEventSideEffects(ctx, tx, *event); err != nil {
			return err
		}

		if err := recordLocalEventState(ctx, tx, *event, base); err != nil {
			return err
		}
	}

	// Материализация не должна «теряться»: если room_state/room_memberships не
	// заполнились, комната окажется мертвой для создателя.
	var stateRows, memberRows int

	if err := tx.QueryRow(ctx, `SELECT count(*) FROM room_state WHERE room_id = $1`, room.RoomID).Scan(&stateRows); err != nil {
		return fmt.Errorf("store: verify state %s: %w", room.RoomID, err)
	}

	if err := tx.QueryRow(ctx, `SELECT count(*) FROM room_memberships WHERE room_id = $1`, room.RoomID).Scan(&memberRows); err != nil {
		return fmt.Errorf("store: verify memberships %s: %w", room.RoomID, err)
	}

	if stateRows != stateEvents || memberRows != membershipEvents {
		return fmt.Errorf("store: create room %s: materialized state %d/%d, memberships %d/%d",
			room.RoomID, stateRows, stateEvents, memberRows, membershipEvents)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit room %s: %w", room.RoomID, err)
	}

	return nil
}

// SetRoomPublic меняет публикацию комнаты в каталоге.
func (s *RoomStore) SetRoomPublic(ctx context.Context, roomID string, public bool) error {
	const query = `UPDATE rooms SET is_public = $2 WHERE room_id = $1`

	tag, err := s.db.pool.Exec(ctx, query, roomID, public)
	if err != nil {
		return fmt.Errorf("store: set room %s public: %w", roomID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: room %s: %w", roomID, core.ErrNotFound)
	}

	return nil
}

// RoomByID читает комнату.
func (s *RoomStore) RoomByID(ctx context.Context, roomID string) (core.Room, error) {
	const query = `SELECT room_id, room_version, coalesce(creator, ''), is_public FROM rooms WHERE room_id = $1`

	var (
		room    core.Room
		version string
	)

	err := s.db.pool.QueryRow(ctx, query, roomID).
		Scan(&room.RoomID, &version, &room.Creator, &room.IsPublic)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Room{}, fmt.Errorf("store: room %s: %w", roomID, core.ErrNotFound)
		}

		return core.Room{}, fmt.Errorf("store: select room %s: %w", roomID, err)
	}

	room.RoomVersion, err = strconv.Atoi(version)
	if err != nil {
		return core.Room{}, fmt.Errorf("store: room %s version %q: %w", roomID, version, err)
	}

	return room, nil
}

var _ core.ForwardExtremityReader = (*RoomStore)(nil)

// ForwardExtremityEvents - события forward extremities комнаты: большая глубина и
// поздний stream_ordering первыми, не больше limit.
func (s *RoomStore) ForwardExtremityEvents(ctx context.Context, roomID string, limit int) ([]core.Event, error) {
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events,
		       e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by,'')
		FROM room_forward_extremities f
		JOIN events e ON e.event_id = f.event_id
		WHERE f.room_id = $1
		ORDER BY e.depth DESC, e.stream_ordering DESC
		LIMIT $2`

	rows, err := s.db.pool.Query(ctx, query, roomID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: forward extremities %s: %w", roomID, err)
	}

	return scanEventRows(rows)
}

// LatestEvents возвращает последние события комнаты по stream_ordering (для prev_events).
func (s *RoomStore) LatestEvents(ctx context.Context, roomID string, limit int) ([]core.Event, error) {
	const query = `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events
		WHERE room_id = $1 AND NOT outlier
		ORDER BY stream_ordering DESC
		LIMIT $2`

	rows, err := s.db.pool.Query(ctx, query, roomID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: latest events %s: %w", roomID, err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}

	return events, nil
}

// AppendEvent пишет событие, применяет состояние/членство и идемпотентность txn.
// Возвращает записанное событие и признак «уже было» (txn_id повторился).
func (s *RoomStore) AppendEvent(
	ctx context.Context,
	event core.Event,
	txn *core.TransactionKey,
) (core.Event, bool, error) {
	if txn != nil {
		existing, err := s.eventByTxn(ctx, txn)
		if err != nil {
			return core.Event{}, false, err
		}

		if existing != "" {
			return core.Event{EventID: existing, RoomID: event.RoomID}, true, nil
		}
	}

	// Очередь комнаты - до транзакции: ждущий не держит соединение пула.
	unlockRoom, err := s.db.roomLocks.lock(ctx, event.RoomID)
	if err != nil {
		return core.Event{}, false, err
	}

	defer unlockRoom()

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return core.Event{}, false, fmt.Errorf("store: begin append: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Локальные события комнаты пишутся последовательно с разрешением входящих:
	// группа состояния и forward extremities не должны разойтись с room_state.
	if err := lockRoomState(ctx, tx, event.RoomID); err != nil {
		return core.Event{}, false, err
	}

	base, err := readLocalStateBase(ctx, tx, event.RoomID)
	if err != nil {
		return core.Event{}, false, err
	}

	written, err := insertEvent(ctx, tx, event)
	if err != nil {
		return core.Event{}, false, err
	}

	// Номер выдан, но транзакция ещё не закоммичена: регистрируем, чтобы /sync
	// не отдал next_batch за это событие до его появления в БД.
	s.db.registerPendingPosition(written.StreamOrdering)

	// Если транзакция не закоммитится, номер никогда не станет видимым, и
	// safePosition навсегда застрянет на нём (sync перестанет двигаться).
	// Поэтому снимаем регистрацию на любом пути, кроме успешного commit.
	committed := false

	defer func() {
		if !committed {
			s.db.forgetPendingPosition(written.StreamOrdering)
		}
	}()

	// Серверы комнаты до события - получатели рассылки. Без
	// федерации не читаются: запрос по всему членству комнаты (3 мс на 450
	// участников) шел под блокировкой комнаты на каждое событие.
	var hostsBefore []string

	federate := !s.db.noFederationOutbox.Load()
	if federate {
		if hostsBefore, err = joinedHosts(ctx, tx, written.RoomID); err != nil {
			return core.Event{}, false, err
		}
	}

	if err := applyEventSideEffects(ctx, tx, written); err != nil {
		return core.Event{}, false, err
	}

	if err := recordLocalEventState(ctx, tx, written, base); err != nil {
		return core.Event{}, false, err
	}

	if federate {
		if err := s.enqueueFederation(ctx, tx, written, hostsBefore); err != nil {
			return core.Event{}, false, err
		}
	}

	// Redaction применяется в той же транзакции, что и запись события.
	if err := s.db.linkRedactions(ctx, tx, redactionRefOf(written)); err != nil {
		return core.Event{}, false, err
	}

	if txn != nil {
		const insertTxn = `
			INSERT INTO transactions (user_id, device_id, endpoint, txn_id, room_id, event_id)
			VALUES ($1, $2, $3, $4, $5, $6)`

		if _, err := tx.Exec(ctx, insertTxn,
			txn.UserID, txn.DeviceID, txn.Endpoint, txn.TxnID, event.RoomID, written.EventID,
		); err != nil {
			if isUniqueViolation(err) {
				// Гонка двух одинаковых txn: победитель уже записал событие.
				_ = tx.Rollback(ctx)

				existing, lookupErr := s.eventByTxn(ctx, txn)
				if lookupErr == nil && existing != "" {
					return core.Event{EventID: existing, RoomID: event.RoomID}, true, nil
				}
			}

			return core.Event{}, false, fmt.Errorf("store: insert txn: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return core.Event{}, false, fmt.Errorf("store: commit append: %w", err)
	}

	committed = true

	return written, false, nil
}

// enqueueFederation ставит локальное событие в outbox в транзакции его записи.
// Раньше постановка шла после коммита на контексте запроса, с
// ошибками только в лог: клиент оборвал соединение - событие не уходило никогда.
func (s *RoomStore) enqueueFederation(ctx context.Context, tx pgx.Tx, event core.Event, before []string) error {
	own := event.Origin
	if own == "" {
		own = domainOfMatrixID(event.Sender)
	}

	// Серверы после события отличаются от «до» только у membership удаленного
	// пользователя (kick/ban, ответ на invite): состояние перечитывается лишь тогда.
	after := before

	if event.Type == "m.room.member" && event.StateKey != nil && domainOfMatrixID(*event.StateKey) != own {
		var err error

		after, err = joinedHosts(ctx, tx, event.RoomID)
		if err != nil {
			return err
		}
	}

	// Приглашение пользователя другого сервера уже доставлено ему через /invite
	// (событие записывается после доставки): в /send оно этому серверу
	// не уходит. Раньше, если там уже были участники комнаты, invite приходил
	// повторно через /send, и Synapse отвергал его (soft-fail, «already in the room»).
	var invitee string

	if event.Type == "m.room.member" && event.StateKey != nil && membershipFromContent(event.Content) == "invite" {
		if host := domainOfMatrixID(*event.StateKey); host != own {
			invitee = host
		}
	}

	return s.db.enqueueEventTx(ctx, tx, outboxEvent{
		EventID: event.EventID, RoomID: event.RoomID, StreamOrdering: event.StreamOrdering, Own: own,
		OnBehalfOf: invitee,
	}, before, after)
}

// eventByTxn возвращает event_id ранее созданного события для этой txn (или пусто).
func (s *RoomStore) eventByTxn(ctx context.Context, txn *core.TransactionKey) (string, error) {
	const query = `
		SELECT coalesce(event_id, '') FROM transactions
		WHERE user_id = $1 AND device_id = $2 AND endpoint = $3 AND txn_id = $4`

	var eventID string

	err := s.db.pool.QueryRow(ctx, query, txn.UserID, txn.DeviceID, txn.Endpoint, txn.TxnID).Scan(&eventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}

		return "", fmt.Errorf("store: txn lookup: %w", err)
	}

	return eventID, nil
}

// EventByID читает событие комнаты по идентификатору.
func (s *RoomStore) EventByID(ctx context.Context, roomID, eventID string) (core.Event, error) {
	const query = `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events
		WHERE room_id = $1 AND event_id = $2`

	rows, err := s.db.pool.Query(ctx, query, roomID, eventID)
	if err != nil {
		return core.Event{}, fmt.Errorf("store: event %s: %w", eventID, err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return core.Event{}, err
	}

	if len(events) == 0 {
		return core.Event{}, fmt.Errorf("store: event %s: %w", eventID, core.ErrNotFound)
	}

	return events[0], nil
}

// RoomEvents отдаёт окно событий от курсора from в направлении backward.
func (s *RoomStore) RoomEvents(
	ctx context.Context,
	roomID string,
	from int64,
	limit int,
	backward bool,
) ([]core.Event, error) {
	query := `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events
		WHERE room_id = $1 AND stream_ordering < $2 AND NOT outlier
		ORDER BY stream_ordering DESC
		LIMIT $3`

	if !backward {
		query = `
			SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
			       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
			FROM events
			WHERE room_id = $1 AND stream_ordering > $2 AND NOT outlier
			ORDER BY stream_ordering ASC
			LIMIT $3`
	}

	rows, err := s.db.pool.Query(ctx, query, roomID, from, limit)
	if err != nil {
		return nil, fmt.Errorf("store: room events %s: %w", roomID, err)
	}

	return scanEventRows(rows)
}

// MinMaxStream возвращает границы stream_ordering комнаты (0,0 если событий нет).
func (s *RoomStore) MinMaxStream(ctx context.Context, roomID string) (minStream, maxStream int64, err error) {
	const query = `
		SELECT coalesce(min(stream_ordering), 0), coalesce(max(stream_ordering), 0)
		FROM events WHERE room_id = $1 AND NOT outlier`

	if err = s.db.pool.QueryRow(ctx, query, roomID).Scan(&minStream, &maxStream); err != nil {
		return 0, 0, fmt.Errorf("store: stream bounds %s: %w", roomID, err)
	}

	return minStream, maxStream, nil
}

// StreamAtDepth - граница топологической позиции Synapse (depth, stream) в
// stream_ordering комнаты (см. core.RoomService.PageCursor). Только для
// токенов Synapse, оставшихся у клиентов после переезда: идет по индексу
// events_room_depth_idx и читает историю по одну сторону от границы.
func (s *RoomStore) StreamAtDepth(
	ctx context.Context, roomID string, depth, stream int64, backward bool,
) (pos int64, found bool, err error) {
	query := `
		SELECT max(stream_ordering) FROM events
		WHERE room_id = $1 AND NOT outlier AND (depth, stream_ordering) <= ($2, $3)`

	if !backward {
		query = `
			SELECT min(stream_ordering) FROM events
			WHERE room_id = $1 AND NOT outlier AND (depth, stream_ordering) > ($2, $3)`
	}

	var bound *int64

	if err = s.db.pool.QueryRow(ctx, query, roomID, depth, stream).Scan(&bound); err != nil {
		return 0, false, fmt.Errorf("store: stream at depth %s: %w", roomID, err)
	}

	if bound == nil {
		return 0, false, nil
	}

	return *bound, true, nil
}

// Membership возвращает membership пользователя в комнате (ErrNotFound, если записи нет).
func (s *RoomStore) Membership(ctx context.Context, roomID, userID string) (string, error) {
	const query = `SELECT membership FROM room_memberships WHERE room_id = $1 AND user_id = $2`

	var membership string

	err := s.db.pool.QueryRow(ctx, query, roomID, userID).Scan(&membership)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("store: membership %s/%s: %w", roomID, userID, core.ErrNotFound)
		}

		return "", fmt.Errorf("store: membership %s/%s: %w", roomID, userID, err)
	}

	return membership, nil
}

// MembershipInfo - членство пользователя, позиция его события и forgotten
// (чтение покинутой комнаты до момента выхода).
func (s *RoomStore) MembershipInfo(ctx context.Context, roomID, userID string) (core.MembershipInfo, error) {
	const query = `
		SELECT rm.membership, rm.forgotten,
		       coalesce((SELECT e.stream_ordering FROM events e WHERE e.event_id = rm.event_id), 0)
		FROM room_memberships rm
		WHERE rm.room_id = $1 AND rm.user_id = $2`

	var info core.MembershipInfo

	err := s.db.pool.QueryRow(ctx, query, roomID, userID).Scan(&info.Membership, &info.Forgotten, &info.Stream)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.MembershipInfo{}, fmt.Errorf("store: membership %s/%s: %w", roomID, userID, core.ErrNotFound)
		}

		return core.MembershipInfo{}, fmt.Errorf("store: membership %s/%s: %w", roomID, userID, err)
	}

	return info, nil
}

// StateAtStream - состояние комнаты на позиции pos: для каждого (type,
// state_key) последнее state-событие не позже pos (как StateAtRoomsStream /sync).
func (s *RoomStore) StateAtStream(ctx context.Context, roomID string, pos int64) ([]core.Event, error) {
	const query = `
		SELECT DISTINCT ON (e.type, e.state_key)
		       e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering,
		       coalesce(e.redacted_by,'')
		FROM events e
		WHERE e.room_id = $1 AND e.state_key IS NOT NULL AND e.stream_ordering <= $2
		ORDER BY e.type, e.state_key, e.stream_ordering DESC`

	rows, err := s.db.pool.Query(ctx, query, roomID, pos)
	if err != nil {
		return nil, fmt.Errorf("store: state of %s at %d: %w", roomID, pos, err)
	}

	return scanEventRows(rows)
}

// insertEvent пишет событие и возвращает его с назначенным stream_ordering.
func insertEvent(ctx context.Context, tx pgx.Tx, event core.Event) (core.Event, error) {
	const query = `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content,
		                    prev_events, auth_events, depth, origin_server_ts, origin, pdu_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING stream_ordering, received_at`

	if event.PrevEvents == nil {
		event.PrevEvents = []string{}
	}

	if event.AuthEvents == nil {
		event.AuthEvents = []string{}
	}

	var receivedAt time.Time

	if err := tx.QueryRow(ctx, query,
		event.EventID, event.RoomID, event.Type, event.Sender, event.StateKey, string(jsonbSafe(event.Content)),
		event.PrevEvents, event.AuthEvents, event.Depth, event.OriginServerTS, event.Origin,
		nullText(string(event.PDU)), // исходный подписанный PDU
	).Scan(&event.StreamOrdering, &receivedAt); err != nil {
		if isUniqueViolation(err) {
			return core.Event{}, fmt.Errorf("store: duplicate event %s: %w", event.EventID, core.ErrBadJSON)
		}

		// Комнату удалили параллельным purge: «нет комнаты», а не 500.
		if isForeignKeyViolation(err) {
			return core.Event{}, fmt.Errorf("store: insert event %s: room is gone: %w", event.EventID, core.ErrNotFound)
		}

		return core.Event{}, fmt.Errorf("store: insert event %s: %w", event.EventID, err)
	}

	return event, nil
}

// applyEventSideEffects обновляет room_state и room_memberships для state-события.
func applyEventSideEffects(ctx context.Context, tx pgx.Tx, event core.Event) error {
	if !event.IsState() {
		return nil
	}

	stateKey := *event.StateKey

	const upsertState = `
		INSERT INTO room_state (room_id, type, state_key, event_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (room_id, type, state_key) DO UPDATE
		SET event_id = EXCLUDED.event_id, updated_at = now()`

	if _, err := tx.Exec(ctx, upsertState, event.RoomID, event.Type, stateKey, event.EventID); err != nil {
		return fmt.Errorf("store: upsert state %s/%s: %w", event.Type, stateKey, err)
	}

	if event.Type != "m.room.member" {
		return nil
	}

	membership := membershipFromContent(event.Content)
	if membership == "" {
		return nil
	}

	const upsertMembership = `
		INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (room_id, user_id) DO UPDATE
		SET event_id = EXCLUDED.event_id, sender = EXCLUDED.sender,
		    membership = EXCLUDED.membership, created_at = now(),
		    -- новое членство снимает «забыто» (forget относится к прошлому выходу)
		    forgotten = false`

	if _, err := tx.Exec(ctx, upsertMembership,
		event.EventID, event.RoomID, stateKey, event.Sender, membership,
	); err != nil {
		return fmt.Errorf("store: upsert membership %s: %w", stateKey, err)
	}

	// Смена членства — это изменение device-листа участника для остальных.
	// При join вступивший должен увидеть еще и device-листы всех участников
	// комнаты. Раньше для этого писалась строка на каждого участника отдельным
	// INSERT (200 входов в комнату - 20300 строк); теперь одна строка на
	// вступившего, а участники добавляются при чтении (DeviceListChangesFor).
	return recordDeviceListChange(ctx, tx, stateKey)
}

// recordDeviceListChange фиксирует изменение device-листа пользователя в транзакции.
func recordDeviceListChange(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO device_lists_changes (user_id, device_id) VALUES ($1, '')`, userID); err != nil {
		return fmt.Errorf("store: record device list change %s: %w", userID, err)
	}

	return nil
}

// membershipFromContent вытаскивает membership из content m.room.member.
func membershipFromContent(content []byte) string {
	var payload struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(content, &payload); err != nil {
		return ""
	}

	return payload.Membership
}

// scanEventRows читает строки событий и закрывает rows.
func scanEventRows(rows pgx.Rows) ([]core.Event, error) {
	defer rows.Close()

	var events []core.Event

	for rows.Next() {
		var (
			event core.Event
			state *string
		)

		if err := rows.Scan(
			&event.EventID, &event.RoomID, &event.Type, &event.Sender, &state, &event.Content,
			&event.PrevEvents, &event.AuthEvents, &event.Depth, &event.OriginServerTS,
			&event.Origin, &event.StreamOrdering, &event.RedactedBy,
		); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}

		event.StateKey = state
		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}

	return events, nil
}

// StateEvents возвращает материализованное состояние комнаты.
func (s *RoomStore) StateEvents(ctx context.Context, roomID string) ([]core.Event, error) {
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1
		ORDER BY e.type, e.state_key`

	rows, err := s.db.pool.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: state events %s: %w", roomID, err)
	}

	return scanEventRows(rows)
}

// CurrentStateEvent - текущее событие состояния (type, state_key) по первичному
// ключу room_state; нет - core.ErrNotFound.
func (s *RoomStore) CurrentStateEvent(ctx context.Context, roomID, eventType, stateKey string) (core.Event, error) {
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1 AND rs.type = $2 AND rs.state_key = $3`

	rows, err := s.db.pool.Query(ctx, query, roomID, eventType, stateKey)
	if err != nil {
		return core.Event{}, fmt.Errorf("store: state event %s %s: %w", roomID, eventType, err)
	}

	events, err := scanEventRows(rows)
	if err != nil {
		return core.Event{}, err
	}

	if len(events) == 0 {
		return core.Event{}, fmt.Errorf("store: state event %s %s: %w", roomID, eventType, core.ErrNotFound)
	}

	return events[0], nil
}

// AuthStateEvents возвращает create, power_levels, join_rules и членство members
// из текущего состояния комнаты - по первичному ключу room_state, без чтения
// всего состояния.
func (s *RoomStore) AuthStateEvents(ctx context.Context, roomID string, members []string) ([]core.Event, error) {
	const query = `
		SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
		       e.auth_events, e.depth, e.origin_server_ts, e.origin, e.stream_ordering, coalesce(e.redacted_by,'')
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.room_id = $1
		  AND ((rs.type IN ('m.room.create', 'm.room.power_levels', 'm.room.join_rules') AND rs.state_key = '')
		       OR (rs.type = 'm.room.member' AND rs.state_key = ANY($2)))
		ORDER BY e.type, e.state_key`

	rows, err := s.db.pool.Query(ctx, query, roomID, members)
	if err != nil {
		return nil, fmt.Errorf("store: auth state events %s: %w", roomID, err)
	}

	return scanEventRows(rows)
}

// AuthEventIDs отдаёт event_id'ы auth-событий для нового события комнаты:
// create, power_levels, member отправителя и (для m.room.member) участника, плюс
// join_rules. Набор соответствует auth_types_for_event в Synapse для наших типов:
// id считается по каноническому JSON с auth_events, поэтому они нужны до хеширования.
func (s *RoomStore) AuthEventIDs(
	ctx context.Context, roomID, sender, eventType string, stateKey *string,
) ([]string, error) {
	types := [][2]string{
		{"m.room.create", ""},
		{"m.room.power_levels", ""},
		{"m.room.member", sender},
	}

	if eventType == "m.room.member" {
		if stateKey == nil || *stateKey == "" {
			return nil, fmt.Errorf("store: auth events: member without state_key")
		}

		types = append(types, [2]string{"m.room.member", *stateKey}, [2]string{"m.room.join_rules", ""})
	}

	const query = `
		SELECT event_id FROM room_state
		WHERE room_id = $1 AND type = $2 AND state_key = $3`

	out := make([]string, 0, len(types))

	for _, pair := range types {
		var eventID string

		err := s.db.pool.QueryRow(ctx, query, roomID, pair[0], pair[1]).Scan(&eventID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue // состояния ещё нет (genesis) — не ошибка
			}

			return nil, fmt.Errorf("store: auth events %s/%s: %w", pair[0], pair[1], err)
		}

		out = append(out, eventID)
	}

	return out, nil
}

// PublicRoomsDirectory возвращает опубликованные комнаты для GET /publicRooms
// со сводкой текущего состояния: name, topic, canonical_alias, avatar,
// join_rules, history_visibility, guest_access, тип комнаты и число участников.
// Заблокированные комнаты (room_blocks) в каталог не попадают.
func (db *DB) PublicRoomsDirectory(ctx context.Context) ([]core.PublicRoom, error) {
	const query = `
		SELECT r.room_id,
			(SELECT count(*) FROM room_memberships m
			 WHERE m.room_id = r.room_id AND m.membership = 'join'),
			coalesce(jsonb_object_agg(s.type, e.content) FILTER (WHERE s.type IS NOT NULL), '{}')
		FROM rooms r
		LEFT JOIN room_state s ON s.room_id = r.room_id AND s.state_key = ''
			AND s.type IN ('m.room.name', 'm.room.topic', 'm.room.canonical_alias', 'm.room.avatar',
				'm.room.join_rules', 'm.room.history_visibility', 'm.room.guest_access', 'm.room.create')
		LEFT JOIN events e ON e.event_id = s.event_id
		WHERE r.is_public = true
			AND NOT EXISTS (SELECT 1 FROM room_blocks b WHERE b.room_id = r.room_id)
		GROUP BY r.room_id`

	rows, err := db.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: public rooms: %w", err)
	}

	defer rows.Close()

	var rooms []core.PublicRoom

	for rows.Next() {
		var (
			room  core.PublicRoom
			state map[string]json.RawMessage
		)

		if err := rows.Scan(&room.RoomID, &room.JoinedMembers, &state); err != nil {
			return nil, fmt.Errorf("store: scan public room: %w", err)
		}

		fillPublicRoom(&room, state)
		rooms = append(rooms, room)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: public rooms rows: %w", err)
	}

	return rooms, nil
}

// fillPublicRoom раскладывает content state-событий в поля каталога.
func fillPublicRoom(room *core.PublicRoom, state map[string]json.RawMessage) {
	field := func(eventType, key string) string {
		var content map[string]json.RawMessage
		if json.Unmarshal(state[eventType], &content) != nil {
			return ""
		}

		var value string
		if json.Unmarshal(content[key], &value) != nil {
			return ""
		}

		return value
	}

	room.Name = field("m.room.name", "name")
	room.Topic = field("m.room.topic", "topic")
	room.CanonicalAlias = field("m.room.canonical_alias", "alias")
	room.AvatarURL = field("m.room.avatar", "url")
	room.JoinRule = field("m.room.join_rules", "join_rule")
	room.RoomType = field("m.room.create", "type")
	room.WorldReadable = field("m.room.history_visibility", "history_visibility") == "world_readable"
	room.GuestCanJoin = field("m.room.guest_access", "guest_access") == "can_join"

	if room.JoinRule == "" {
		room.JoinRule = "invite" // без join_rules комната закрыта (auth rules)
	}
}

// ForgetRoom помечает комнату забытой для пользователя: она перестаёт приходить в
// /sync. По спеке забыть можно только комнату, из которой вышел (не join), иначе
// клиент мог бы «спрятать» активную комнату.
func (s *RoomStore) ForgetRoom(ctx context.Context, userID, roomID string) error {
	const query = `
		UPDATE room_memberships SET forgotten = true
		WHERE room_id = $1 AND user_id = $2 AND membership <> 'join'`

	tag, err := s.db.pool.Exec(ctx, query, roomID, userID)
	if err != nil {
		return fmt.Errorf("store: forget room %s/%s: %w", roomID, userID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: forget room %s/%s: %w", roomID, userID, core.ErrForbidden)
	}

	return nil
}

var _ core.RedactionEventStore = (*DB)(nil)

// EventsByIDs возвращает события по id (порядок не гарантирован; отсутствующие пропускаются).
func (db *DB) EventsByIDs(ctx context.Context, ids []string) ([]core.Event, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	const query = `
		SELECT event_id, room_id, type, sender, state_key, content, prev_events, auth_events,
		       depth, origin_server_ts, origin, stream_ordering, coalesce(redacted_by,'')
		FROM events WHERE event_id = ANY($1)`

	rows, err := db.pool.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("store: events by ids: %w", err)
	}

	return scanEventRows(rows)
}
