package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/state"
)

// Состояние на событие (state groups) и forward extremities комнаты.
//
// Группа - дельта к prev_group или полный снимок; цепочка дельт не длиннее
// maxStateChain, иначе пишется снимок (как state groups Synapse). room_state_groups -
// группа, совпадающая с room_state: локальное событие строит от нее дельту, а любая
// запись room_state снимает ее триггером (миграция 0044).

const (
	// maxStateChain - максимальная длина цепочки дельт до снимка.
	maxStateChain = 50
	// maxStateDelta - больше измененных ключей - снимок, а не дельта.
	maxStateDelta = 100
)

// lockRoomState - блокировка состояния комнаты до конца транзакции: запись
// локального события и разрешение входящих идут по комнате последовательно.
func lockRoomState(ctx context.Context, tx pgx.Tx, roomID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('room_state:' || $1))`, roomID); err != nil {
		return fmt.Errorf("store: lock room state %s: %w", roomID, err)
	}

	return nil
}

var _ core.StateResolutionStore = (*DB)(nil)

// WithRoomState выполняет fn в транзакции под блокировкой состояния комнаты.
func (db *DB) WithRoomState(ctx context.Context, roomID string, fn func(core.RoomStateTx) error) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: room state begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockRoomState(ctx, tx, roomID); err != nil {
		return err
	}

	if err := fn(newRoomStateTx(tx, roomID)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: room state commit: %w", err)
	}

	return nil
}

// roomStateTx - core.RoomStateTx поверх транзакции pgx.
type roomStateTx struct {
	tx     pgx.Tx
	roomID string
	// groups - содержимое загруженных и записанных групп.
	groups map[int64]state.StateMap
	// chain - длина цепочки дельт группы.
	chain map[int64]int
	// eventGroups - группа состояния после события.
	eventGroups map[string]int64
}

func newRoomStateTx(tx pgx.Tx, roomID string) *roomStateTx {
	return &roomStateTx{
		tx: tx, roomID: roomID,
		groups: map[int64]state.StateMap{}, chain: map[int64]int{}, eventGroups: map[string]int64{},
	}
}

// RoomVersion - версия комнаты ("" - комнаты нет).
func (r *roomStateTx) RoomVersion(ctx context.Context) (string, error) {
	var version string

	err := r.tx.QueryRow(ctx, `SELECT room_version FROM rooms WHERE room_id = $1`, r.roomID).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: room version %s: %w", r.roomID, err)
	}

	return version, nil
}

// CurrentState - room_state комнаты.
func (r *roomStateTx) CurrentState(ctx context.Context) (state.StateMap, error) {
	rows, err := r.tx.Query(ctx, `SELECT type, state_key, event_id FROM room_state WHERE room_id = $1`, r.roomID)
	if err != nil {
		return nil, fmt.Errorf("store: current state %s: %w", r.roomID, err)
	}

	return scanStateMap(rows)
}

// scanStateMap читает строки (type, state_key, event_id) и закрывает rows.
func scanStateMap(rows pgx.Rows) (state.StateMap, error) {
	defer rows.Close()

	out := state.StateMap{}

	for rows.Next() {
		var (
			key state.Key
			id  string
		)

		if err := rows.Scan(&key.Type, &key.StateKey, &id); err != nil {
			return nil, fmt.Errorf("store: scan state: %w", err)
		}

		out[key] = id
	}

	return out, rows.Err()
}

// ForwardExtremities - forward extremities комнаты. Комната без них, но с room_state
// (записана до миграции 0044 или импортом), получает extremity на последнем
// событии таймлайна (не outlier) вне exclude со снимком room_state.
func (r *roomStateTx) ForwardExtremities(ctx context.Context, exclude []string) ([]string, error) {
	ids, err := r.queryIDs(ctx, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1 ORDER BY event_id`, r.roomID)
	if err != nil || len(ids) > 0 {
		return ids, err
	}

	if exclude == nil {
		exclude = []string{}
	}

	var latest string

	err = r.tx.QueryRow(ctx, `
		SELECT e.event_id FROM events e
		WHERE e.room_id = $1 AND NOT (e.event_id = ANY($2)) AND NOT e.outlier
		  AND EXISTS (SELECT 1 FROM room_state rs WHERE rs.room_id = $1)
		ORDER BY e.stream_ordering DESC LIMIT 1`, r.roomID, exclude).Scan(&latest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("store: seed extremity %s: %w", r.roomID, err)
	}

	groups, err := r.groupsOf(ctx, []string{latest})
	if err != nil {
		return nil, err
	}

	if _, ok := groups[latest]; !ok {
		current, err := r.CurrentState(ctx)
		if err != nil {
			return nil, err
		}

		group, err := r.snapshot(ctx, current)
		if err != nil {
			return nil, err
		}

		if err := r.setEventGroup(ctx, latest, group, false); err != nil {
			return nil, err
		}
	}

	if err := r.SetForwardExtremities(ctx, []string{latest}); err != nil {
		return nil, err
	}

	return []string{latest}, nil
}

// SetForwardExtremities заменяет forward extremities комнаты.
func (r *roomStateTx) SetForwardExtremities(ctx context.Context, ids []string) error {
	if ids == nil {
		ids = []string{}
	}

	if _, err := r.tx.Exec(ctx, `
		DELETE FROM room_forward_extremities WHERE room_id = $1 AND NOT (event_id = ANY($2))`, r.roomID, ids); err != nil {
		return fmt.Errorf("store: clear extremities %s: %w", r.roomID, err)
	}

	if _, err := r.tx.Exec(ctx, `
		INSERT INTO room_forward_extremities (room_id, event_id)
		SELECT $1, id FROM unnest($2::text[]) AS id
		ON CONFLICT DO NOTHING`, r.roomID, ids); err != nil {
		return fmt.Errorf("store: set extremities %s: %w", r.roomID, err)
	}

	return nil
}

// queryIDs - один столбец идентификаторов.
func (r *roomStateTx) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := r.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query ids: %w", err)
	}

	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: scan ids: %w", err)
	}

	return ids, nil
}

// groupsOf - группы состояния после событий (только записанные).
func (r *roomStateTx) groupsOf(ctx context.Context, eventIDs []string) (map[string]int64, error) {
	out := make(map[string]int64, len(eventIDs))
	missing := make([]string, 0, len(eventIDs))

	for _, id := range eventIDs {
		if group, ok := r.eventGroups[id]; ok {
			out[id] = group
		} else {
			missing = append(missing, id)
		}
	}

	if len(missing) == 0 {
		return out, nil
	}

	rows, err := r.tx.Query(ctx, `
		SELECT event_id, state_group FROM event_state_groups
		WHERE room_id = $1 AND event_id = ANY($2)`, r.roomID, missing)
	if err != nil {
		return nil, fmt.Errorf("store: event state groups: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			id    string
			group int64
		)

		if err := rows.Scan(&id, &group); err != nil {
			return nil, fmt.Errorf("store: scan event state group: %w", err)
		}

		out[id] = group
		r.eventGroups[id] = group
	}

	return out, rows.Err()
}

// loadGroup - содержимое группы: записи по цепочке prev_group, ближайшая побеждает.
func (r *roomStateTx) loadGroup(ctx context.Context, group int64) (state.StateMap, error) {
	if st, ok := r.groups[group]; ok {
		return st, nil
	}

	rows, err := r.tx.Query(ctx, `
		WITH RECURSIVE chain (id, prev, lvl) AS (
			SELECT id, prev_group, 0 FROM state_groups WHERE id = $1
			UNION ALL
			SELECT g.id, g.prev_group, c.lvl + 1 FROM state_groups g JOIN chain c ON g.id = c.prev
		)
		SELECT DISTINCT ON (e.type, e.state_key) e.type, e.state_key, e.event_id
		FROM chain c JOIN state_group_entries e ON e.state_group = c.id
		ORDER BY e.type, e.state_key, c.lvl`, group)
	if err != nil {
		return nil, fmt.Errorf("store: load state group %d: %w", group, err)
	}

	st, err := scanStateMap(rows)
	if err != nil {
		return nil, err
	}

	var chain int
	if err := r.tx.QueryRow(ctx, `SELECT chain_len FROM state_groups WHERE id = $1`, group).Scan(&chain); err != nil {
		return nil, fmt.Errorf("store: state group %d chain: %w", group, err)
	}

	r.chain[group] = chain

	r.groups[group] = st

	return st, nil
}

// StateAfter - состояние после событий (только тех, для которых оно записано).
func (r *roomStateTx) StateAfter(ctx context.Context, eventIDs []string) (map[string]state.StateMap, error) {
	groups, err := r.groupsOf(ctx, eventIDs)
	if err != nil {
		return nil, err
	}

	out := make(map[string]state.StateMap, len(groups))

	for id, group := range groups {
		st, err := r.loadGroup(ctx, group)
		if err != nil {
			return nil, err
		}

		out[id] = st
	}

	return out, nil
}

// eventColumns - колонки события для state.Event (rejected - из event_state_groups
// или rejected_reason: у отклоненного outlier состояния нет).
const eventColumns = `e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events,
	e.auth_events, e.depth, e.origin_server_ts, (coalesce(g.rejected, false) OR e.rejected_reason IS NOT NULL)`

// scanStateEvents читает события и закрывает rows.
func scanStateEvents(rows pgx.Rows) (map[string]state.Event, error) {
	defer rows.Close()

	out := map[string]state.Event{}

	for rows.Next() {
		var ev state.Event

		if err := rows.Scan(&ev.ID, &ev.RoomID, &ev.Type, &ev.Sender, &ev.StateKey, &ev.Content, &ev.Prev,
			&ev.Auth, &ev.Depth, &ev.OriginTS, &ev.Rejected); err != nil {
			return nil, fmt.Errorf("store: scan state event: %w", err)
		}

		out[ev.ID] = ev
	}

	return out, rows.Err()
}

// Events - события комнаты по id.
func (r *roomStateTx) Events(ctx context.Context, ids []string) (map[string]state.Event, error) {
	if len(ids) == 0 {
		return map[string]state.Event{}, nil
	}

	rows, err := r.tx.Query(ctx, `
		SELECT `+eventColumns+`
		FROM events e LEFT JOIN event_state_groups g ON g.event_id = e.event_id
		WHERE e.room_id = $1 AND e.event_id = ANY($2)`, r.roomID, ids)
	if err != nil {
		return nil, fmt.Errorf("store: state events: %w", err)
	}

	return scanStateEvents(rows)
}

// AuthChains - события ids вместе с полными auth chains (по auth_events событий
// комнаты: event_auth заполняется не для локальных событий). В комнате v12 create
// входит в цепочку неявно.
func (r *roomStateTx) AuthChains(ctx context.Context, ids []string) (map[string]state.Event, error) {
	if len(ids) == 0 {
		return map[string]state.Event{}, nil
	}

	roots := append([]string(nil), ids...)
	if auth.IsCreateHashRoomID(r.roomID) {
		roots = append(roots, "$"+r.roomID[1:])
	}

	rows, err := r.tx.Query(ctx, `
		WITH RECURSIVE chain (id) AS (
			SELECT unnest($2::text[])
			UNION
			SELECT unnest(e.auth_events) FROM events e JOIN chain c ON e.event_id = c.id
			WHERE e.room_id = $1
		)
		SELECT `+eventColumns+`
		FROM chain c JOIN events e ON e.event_id = c.id
		LEFT JOIN event_state_groups g ON g.event_id = e.event_id
		WHERE e.room_id = $1`, r.roomID, roots)
	if err != nil {
		return nil, fmt.Errorf("store: auth chains: %w", err)
	}

	return scanStateEvents(rows)
}

// SaveStateAfter записывает состояние после события. parentEventID - событие, от
// состояния которого after отличается мало (единственный prev): тогда пишется
// дельта или переиспользуется его группа.
func (r *roomStateTx) SaveStateAfter(
	ctx context.Context, eventID string, after state.StateMap, parentEventID string, rejected bool,
) error {
	group, err := r.groupFor(ctx, after, parentEventID)
	if err != nil {
		return err
	}

	return r.setEventGroup(ctx, eventID, group, rejected)
}

// groupFor - группа для состояния st: группа parentEventID, если совпадает, дельта
// к ней, если отличие небольшое и цепочка короткая, иначе снимок.
func (r *roomStateTx) groupFor(ctx context.Context, st state.StateMap, parentEventID string) (int64, error) {
	if parentEventID == "" {
		return r.snapshot(ctx, st)
	}

	groups, err := r.groupsOf(ctx, []string{parentEventID})
	if err != nil {
		return 0, err
	}

	parent, ok := groups[parentEventID]
	if !ok {
		return r.snapshot(ctx, st)
	}

	parentState, err := r.loadGroup(ctx, parent)
	if err != nil {
		return 0, err
	}

	delta, ok := stateDelta(parentState, st)
	if !ok {
		// Ключ пропал: дельта удалений не умеет.
		return r.snapshot(ctx, st)
	}

	if len(delta) == 0 {
		return parent, nil
	}

	if len(delta) > maxStateDelta || r.chain[parent] >= maxStateChain {
		return r.snapshot(ctx, st)
	}

	return r.writeGroup(ctx, parent, r.chain[parent]+1, delta, st)
}

// stateDelta - ключи st, отличающиеся от parent; false - в st пропал ключ parent
// (дельта удалений не умеет).
func stateDelta(parent, st state.StateMap) (state.StateMap, bool) {
	for key := range parent {
		if _, ok := st[key]; !ok {
			return nil, false
		}
	}

	delta := state.StateMap{}

	for key, id := range st {
		if parent[key] != id {
			delta[key] = id
		}
	}

	return delta, true
}

// changedStateRows - строки, которыми st отличается от current, по (type,
// state_key); EventID "" - ключ удален.
func changedStateRows(current, st state.StateMap) []core.StateRow {
	var changed []core.StateRow

	for key, id := range st {
		if current[key] != id {
			changed = append(changed, core.StateRow{Type: key.Type, StateKey: key.StateKey, EventID: id})
		}
	}

	for key := range current {
		if _, ok := st[key]; !ok {
			changed = append(changed, core.StateRow{Type: key.Type, StateKey: key.StateKey})
		}
	}

	sort.Slice(changed, func(i, j int) bool {
		if changed[i].Type != changed[j].Type {
			return changed[i].Type < changed[j].Type
		}

		return changed[i].StateKey < changed[j].StateKey
	})

	return changed
}

// snapshot - новая группа-снимок состояния st.
func (r *roomStateTx) snapshot(ctx context.Context, st state.StateMap) (int64, error) {
	return r.writeGroup(ctx, 0, 0, st, st)
}

// writeGroup пишет группу с записями entries; full - ее полное содержимое (кэш).
func (r *roomStateTx) writeGroup(ctx context.Context, prev int64, chain int, entries, full state.StateMap) (int64, error) {
	var prevArg any
	if prev != 0 {
		prevArg = prev
	}

	var group int64

	if err := r.tx.QueryRow(ctx, `
		INSERT INTO state_groups (room_id, prev_group, chain_len) VALUES ($1, $2, $3) RETURNING id`,
		r.roomID, prevArg, chain).Scan(&group); err != nil {
		return 0, fmt.Errorf("store: insert state group: %w", err)
	}

	types := make([]string, 0, len(entries))
	keys := make([]string, 0, len(entries))
	ids := make([]string, 0, len(entries))

	for key, id := range entries {
		types = append(types, key.Type)
		keys = append(keys, key.StateKey)
		ids = append(ids, id)
	}

	if _, err := r.tx.Exec(ctx, `
		INSERT INTO state_group_entries (state_group, type, state_key, event_id)
		SELECT $1, t, k, id FROM unnest($2::text[], $3::text[], $4::text[]) AS s(t, k, id)`,
		group, types, keys, ids); err != nil {
		return 0, fmt.Errorf("store: insert state group entries: %w", err)
	}

	cached := make(state.StateMap, len(full))
	for key, id := range full {
		cached[key] = id
	}

	r.groups[group] = cached
	r.chain[group] = chain

	return group, nil
}

// setEventGroup связывает событие с группой состояния после него.
func (r *roomStateTx) setEventGroup(ctx context.Context, eventID string, group int64, rejected bool) error {
	if _, err := r.tx.Exec(ctx, `
		INSERT INTO event_state_groups (event_id, room_id, state_group, rejected) VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id) DO NOTHING`, eventID, r.roomID, group, rejected); err != nil {
		return fmt.Errorf("store: event state group %s: %w", eventID, err)
	}

	r.eventGroups[eventID] = group

	return nil
}

// ApplyCurrentState делает room_state равным st: пишет изменившиеся ключи,
// удаляет пропавшие, обновляет room_memberships по изменившимся m.room.member и
// ставит группу комнаты (группа groupEventID или новый снимок). Возвращает
// изменившиеся строки (EventID "" - ключ удален).
func (r *roomStateTx) ApplyCurrentState(ctx context.Context, st state.StateMap, groupEventID string) ([]core.StateRow, error) {
	current, err := r.CurrentState(ctx)
	if err != nil {
		return nil, err
	}

	changed := changedStateRows(current, st)

	if err := r.writeRoomState(ctx, changed); err != nil {
		return nil, err
	}

	group, err := r.groupFor(ctx, st, groupEventID)
	if err != nil {
		return nil, err
	}

	// Триггер room_state снял группу комнаты при записи: ставим совпадающую.
	if _, err := r.tx.Exec(ctx, `
		INSERT INTO room_state_groups (room_id, state_group) VALUES ($1, $2)
		ON CONFLICT (room_id) DO UPDATE SET state_group = EXCLUDED.state_group`, r.roomID, group); err != nil {
		return nil, fmt.Errorf("store: room state group %s: %w", r.roomID, err)
	}

	return changed, nil
}

// writeRoomState пишет изменившиеся ключи room_state и членство.
func (r *roomStateTx) writeRoomState(ctx context.Context, changed []core.StateRow) error {
	var memberIDs []string

	for _, row := range changed {
		if row.EventID == "" {
			if _, err := r.tx.Exec(ctx, `
				DELETE FROM room_state WHERE room_id = $1 AND type = $2 AND state_key = $3`,
				r.roomID, row.Type, row.StateKey); err != nil {
				return fmt.Errorf("store: delete state %s/%s: %w", row.Type, row.StateKey, err)
			}

			continue
		}

		if _, err := r.tx.Exec(ctx, `
			INSERT INTO room_state (room_id, type, state_key, event_id) VALUES ($1, $2, $3, $4)
			ON CONFLICT (room_id, type, state_key) DO UPDATE
			SET event_id = EXCLUDED.event_id, updated_at = now()`,
			r.roomID, row.Type, row.StateKey, row.EventID); err != nil {
			return fmt.Errorf("store: write state %s/%s: %w", row.Type, row.StateKey, err)
		}

		if row.Type == "m.room.member" {
			memberIDs = append(memberIDs, row.EventID)
		}
	}

	if len(memberIDs) == 0 {
		return nil
	}

	// Членство - вслед за состоянием: requireJoined и /sync читают room_memberships.
	members, err := r.Events(ctx, memberIDs)
	if err != nil {
		return err
	}

	for _, id := range memberIDs {
		ev, ok := members[id]
		if !ok || ev.StateKey == nil {
			continue
		}

		membership := membershipOf(ev.Content)
		if membership == "" {
			continue
		}

		if _, err := r.tx.Exec(ctx, `
			INSERT INTO room_memberships (event_id, room_id, user_id, sender, membership)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (room_id, user_id) DO UPDATE
			SET event_id = EXCLUDED.event_id, sender = EXCLUDED.sender,
			    membership = EXCLUDED.membership, forgotten = false`,
			ev.ID, r.roomID, *ev.StateKey, ev.Sender, membership); err != nil {
			return fmt.Errorf("store: membership %s: %w", id, err)
		}
	}

	return nil
}

// localStateBase - группа, совпадающая с room_state до записи локального события.
type localStateBase struct {
	group int64
	chain int
	ok    bool
}

// readLocalStateBase читает группу комнаты до побочных эффектов локального события.
func readLocalStateBase(ctx context.Context, tx pgx.Tx, roomID string) (localStateBase, error) {
	var base localStateBase

	err := tx.QueryRow(ctx, `
		SELECT r.state_group, g.chain_len FROM room_state_groups r JOIN state_groups g ON g.id = r.state_group
		WHERE r.room_id = $1`, roomID).Scan(&base.group, &base.chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return localStateBase{}, nil
	}

	if err != nil {
		return localStateBase{}, fmt.Errorf("store: room state group %s: %w", roomID, err)
	}

	base.ok = true

	return base, nil
}

// recordLocalEventState пишет состояние после локального события (дельта к группе
// комнаты или снимок room_state), ставит группу комнаты и сдвигает forward
// extremities: prev_events события перестают быть extremities, событие - становится.
// Вызывается после записи события и room_state в той же транзакции.
func recordLocalEventState(ctx context.Context, tx pgx.Tx, event core.Event, base localStateBase) error {
	group := base.group

	switch {
	case !base.ok || (event.IsState() && base.chain >= maxStateChain):
		if err := tx.QueryRow(ctx, `
			INSERT INTO state_groups (room_id, chain_len) VALUES ($1, 0) RETURNING id`, event.RoomID).Scan(&group); err != nil {
			return fmt.Errorf("store: local state snapshot: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO state_group_entries (state_group, type, state_key, event_id)
			SELECT $1, type, state_key, event_id FROM room_state WHERE room_id = $2`, group, event.RoomID); err != nil {
			return fmt.Errorf("store: local state snapshot entries: %w", err)
		}
	case event.IsState():
		if err := tx.QueryRow(ctx, `
			INSERT INTO state_groups (room_id, prev_group, chain_len) VALUES ($1, $2, $3) RETURNING id`,
			event.RoomID, base.group, base.chain+1).Scan(&group); err != nil {
			return fmt.Errorf("store: local state delta: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO state_group_entries (state_group, type, state_key, event_id) VALUES ($1, $2, $3, $4)`,
			group, event.Type, *event.StateKey, event.EventID); err != nil {
			return fmt.Errorf("store: local state delta entry: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		WITH room_group AS (
			INSERT INTO room_state_groups (room_id, state_group) VALUES ($1, $3)
			ON CONFLICT (room_id) DO UPDATE SET state_group = EXCLUDED.state_group
		)
		INSERT INTO event_state_groups (event_id, room_id, state_group) VALUES ($2, $1, $3)
		ON CONFLICT (event_id) DO NOTHING`, event.RoomID, event.EventID, group); err != nil {
		return fmt.Errorf("store: local event state group %s: %w", event.EventID, err)
	}

	prev := event.PrevEvents
	if prev == nil {
		prev = []string{}
	}

	if _, err := tx.Exec(ctx, `
		WITH replaced AS (
			DELETE FROM room_forward_extremities WHERE room_id = $1 AND event_id = ANY($3)
		)
		INSERT INTO room_forward_extremities (room_id, event_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, event.RoomID, event.EventID, prev); err != nil {
		return fmt.Errorf("store: local extremities %s: %w", event.EventID, err)
	}

	return nil
}
