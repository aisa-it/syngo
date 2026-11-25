package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
)

// SeenInbox регистрирует входящую транзакцию. Если пара (origin, txn) уже была —
// возвращает seen=true и ничего не делает (дедуп переживает рестарт, UNIQUE в БД).
func (db *DB) SeenInbox(ctx context.Context, origin, txnID string) (bool, error) {
	const query = `
		INSERT INTO federation_inbox (origin, txn_id) VALUES ($1, $2)
		ON CONFLICT (origin, txn_id) DO NOTHING`

	tag, err := db.pool.Exec(ctx, query, origin, txnID)
	if err != nil {
		return false, fmt.Errorf("store: federation inbox %s/%s: %w", origin, txnID, err)
	}

	return tag.RowsAffected() == 0, nil
}

// SetDestinationRoom запоминает, что комната отправляется на destination.
func (db *DB) SetDestinationRoom(ctx context.Context, destination, roomID string) error {
	const query = `
		INSERT INTO destination_rooms (destination, room_id) VALUES ($1, $2)
		ON CONFLICT (destination, room_id) DO NOTHING`

	if _, err := db.pool.Exec(ctx, query, destination, roomID); err != nil {
		return fmt.Errorf("store: destination room %s/%s: %w", destination, roomID, err)
	}

	return nil
}

// DestinationRooms возвращает комнаты для destination.
func (db *DB) DestinationRooms(ctx context.Context, destination string) ([]string, error) {
	const query = `SELECT room_id FROM destination_rooms WHERE destination = $1 ORDER BY room_id`

	rows, err := db.pool.Query(ctx, query, destination)
	if err != nil {
		return nil, fmt.Errorf("store: destination rooms: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			return nil, fmt.Errorf("store: scan destination room: %w", err)
		}

		out = append(out, roomID)
	}

	return out, rows.Err()
}

// ErrEmptyBatch — нет элементов для отправки.
var ErrEmptyBatch = errors.New("store: empty outbox batch")

// FedPDU — входящее событие для применения в транзакции федерации.
type FedPDU struct {
	EventID  string
	RoomID   string
	Type     string
	Sender   string
	StateKey *string
	Content  []byte
	Prev     []string
	Auth     []string
	Depth    int64
	OriginTS int64
	// Redacts — цель redaction (top-level в комнатах до v11).
	Redacts string
	// RedactedBy — redaction-событие, скрывшее content (при чтении content вырезается).
	RedactedBy string
	// Signatures — подписи события от сервера-источника (jsonb), отдаются пирам как есть.
	Signatures []byte
	// Hashes — content hash события от источника (jsonb): для чужих событий
	// отдаём сохранённый, чтобы не пересчитывать канонизацию.
	Hashes []byte
	// Origin — домен сервера, создавшего событие: входит в подписываемый JSON,
	// поэтому обязан возвращаться пиру как есть.
	Origin string
	// PDU — исходный PDU (events.pdu_json): чужое событие — как пришло, свое — как
	// мы его подписали. Федерация отдает ровно его; пусто — PDU не сохранен
	// (события до миграции 0041), отдающая сторона собирает его из полей.
	PDU json.RawMessage
}

// SaveEventAuth сохраняет auth-цепочку события (event_auth).
func (db *DB) SaveEventAuth(ctx context.Context, eventID string, authEvents []string) error {
	if len(authEvents) == 0 {
		return nil
	}

	const query = `
		INSERT INTO event_auth (event_id, auth_event_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`

	for _, authID := range authEvents {
		if _, err := db.pool.Exec(ctx, query, eventID, authID); err != nil {
			return fmt.Errorf("store: save event auth %s: %w", eventID, err)
		}
	}

	return nil
}

// AuthEventsByID возвращает события по id (auth-цепочка для проверки правил,
// отдача в федерацию) вместе с исходным PDU. Порядок - произвольный.
func (db *DB) AuthEventsByID(ctx context.Context, eventIDs []string) ([]FedPDU, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}

	const query = `
		SELECT event_id, room_id, type, sender, state_key, content, coalesce(redacted_by,''),
		       prev_events, auth_events, depth, origin_server_ts, signatures, origin, hashes, pdu_json
		FROM events WHERE event_id = ANY($1)`

	rows, err := db.pool.Query(ctx, query, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("store: auth events by id: %w", err)
	}

	defer rows.Close()

	var out []FedPDU

	for rows.Next() {
		var (
			pdu FedPDU
			raw *string
		)

		if err := rows.Scan(&pdu.EventID, &pdu.RoomID, &pdu.Type, &pdu.Sender, &pdu.StateKey,
			&pdu.Content, &pdu.RedactedBy, &pdu.Prev, &pdu.Auth, &pdu.Depth, &pdu.OriginTS, &pdu.Signatures, &pdu.Origin, &pdu.Hashes,
			&raw); err != nil {
			return nil, fmt.Errorf("store: scan auth event: %w", err)
		}

		if raw != nil {
			pdu.PDU = json.RawMessage(*raw)
		}

		out = append(out, pdu)
	}

	return out, rows.Err()
}

// EventPDU возвращает событие для федерации: поля и исходный PDU (FedPDU.PDU,
// пусто - PDU не сохранен и собирается из полей). Нет события - core.ErrNotFound.
// Для набора событий - AuthEventsByID.
func (db *DB) EventPDU(ctx context.Context, eventID string) (FedPDU, error) {
	pdus, err := db.AuthEventsByID(ctx, []string{eventID})
	if err != nil {
		return FedPDU{}, err
	}

	if len(pdus) == 0 {
		return FedPDU{}, fmt.Errorf("store: event %s: %w", eventID, core.ErrNotFound)
	}

	return pdus[0], nil
}

// ApplyRemoteJoin применяет результат исходящего join: комната, state, auth_chain
// и join-событие пишутся одной транзакцией, подписи проверяются до записи.
func (db *DB) ApplyRemoteJoin(
	ctx context.Context,
	roomID, roomVersion, userID, remoteOrigin string,
	joinEvent map[string]any,
	state, authChain []map[string]any,
	verify func(map[string]any) error,
) error {
	joinPDU, err := pduFromMap(joinEvent)
	if err != nil {
		return fmt.Errorf("store: remote join event: %w", err)
	}

	statePDUs := make([]FedPDU, 0, len(state))
	for i := range state {
		pdu, err := pduFromMap(state[i])
		if err != nil {
			return fmt.Errorf("store: remote join state: %w", err)
		}

		if verify != nil {
			if err := verify(state[i]); err != nil {
				// Диагностика интеропа: без самого события не понять,
				// что именно не сходится — подпись, hashes или redaction-версия.
				snippet, _ := json.Marshal(state[i])
				if len(snippet) > 700 {
					snippet = snippet[:700]
				}

				return fmt.Errorf("store: state signature %s (%s): %w", pdu.EventID, snippet, err)
			}
		}

		statePDUs = append(statePDUs, pdu)
	}

	authPDUs := make([]FedPDU, 0, len(authChain))
	for i := range authChain {
		pdu, err := pduFromMap(authChain[i])
		if err != nil {
			return fmt.Errorf("store: remote join auth_chain: %w", err)
		}

		if verify != nil {
			if err := verify(authChain[i]); err != nil {
				return fmt.Errorf("store: auth_chain signature %s: %w", pdu.EventID, err)
			}
		}

		authPDUs = append(authPDUs, pdu)
	}

	// Запрет ссылок на несуществующие события: все auth_events join-события и
	// присланного state должны резолвиться внутри state/auth_chain. Если нет —
	// id посчитаны по-разному (или пир прислал неполный state), и событие уедет
	// с битой ссылкой.
	known := make(map[string]struct{}, len(statePDUs)+len(authPDUs))

	for i := range statePDUs {
		known[statePDUs[i].EventID] = struct{}{}
	}

	for i := range authPDUs {
		known[authPDUs[i].EventID] = struct{}{}
	}

	// Ссылки могут указывать и на события, уже лежащие в БД (повторный join,
	// догон state): такие считаем известными.
	missing := make([]string, 0)

	for _, candidates := range [][]string{joinPDU.Auth} {
		missing = append(missing, unresolvedRefs(candidates, known)...)
	}

	for i := range statePDUs {
		missing = append(missing, unresolvedRefs(statePDUs[i].Auth, known)...)
	}

	if len(missing) > 0 {
		rows, queryErr := db.pool.Query(ctx,
			`SELECT event_id FROM events WHERE event_id = ANY($1)`, missing)
		if queryErr == nil {
			present := make(map[string]struct{}, len(missing))

			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					present[id] = struct{}{}
				}
			}

			rows.Close()

			missing = unresolvedRefs(missing, present)
		}

		if len(missing) > 0 {
			return fmt.Errorf("store: remote join references unknown auth events %v", missing)
		}
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: remote join begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Версия комнаты приходит из make_join: правила redaction/auth зависят от нее.
	if roomVersion == "" {
		roomVersion = "10"
	}

	// Создатель - из m.room.create присланного состояния, а не вошедший;
	// строку, заведенную входящим invite (создатель - пригласивший, версия по
	// умолчанию), join поправляет.
	creator := remoteRoomCreator(statePDUs)
	if creator == "" {
		creator = userID
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO rooms (room_id, creator, room_version) VALUES ($1, $2, $3)
		ON CONFLICT (room_id) DO UPDATE SET creator = EXCLUDED.creator, room_version = EXCLUDED.room_version`,
		roomID, creator, roomVersion); err != nil {
		return fmt.Errorf("store: remote join room: %w", err)
	}

	// Состояние комнаты - под блокировкой; комната с room_state, но без forward
	// extremities (до миграции 0044) получает опору до записи outlier-событий
	// state и auth_chain.
	if err := lockRoomState(ctx, tx, roomID); err != nil {
		return err
	}

	roomState := newRoomStateTx(tx, roomID)

	if _, err := roomState.ForwardExtremities(ctx, nil); err != nil {
		return err
	}

	// outlier: state и auth_chain из send_join - состояние до входа, а не новые
	// события таймлайна; раньше после join весь state приходил клиенту в timeline.
	// В таймлайн попадает только сам join.
	insertEvents := func(events []FedPDU, origin string, outlier bool) error {
		for i := range events {
			pdu := &events[i]

			prev := pdu.Prev
			if prev == nil {
				prev = []string{}
			}

			auth := pdu.Auth
			if auth == nil {
				auth = []string{}
			}

			eventOrigin := domainOfMatrixID(pdu.Sender)
			if eventOrigin == "" {
				eventOrigin = origin
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events,
				                    auth_events, depth, origin_server_ts, origin, signatures, hashes, pdu_json, outlier)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
				ON CONFLICT (event_id) DO NOTHING`,
				pdu.EventID, pdu.RoomID, pdu.Type, pdu.Sender, pdu.StateKey, pdu.Content,
				prev, auth, pdu.Depth, pdu.OriginTS, eventOrigin, nullJSON(pdu.Signatures), nullJSON(pdu.Hashes),
				nullText(string(pdu.PDU)), outlier); err != nil {
				return fmt.Errorf("store: remote join event %s: %w", pdu.EventID, err)
			}

			for _, authID := range auth {
				if _, err := tx.Exec(ctx, `
					INSERT INTO event_auth (event_id, auth_event_id) VALUES ($1,$2)
					ON CONFLICT DO NOTHING`, pdu.EventID, authID); err != nil {
					return fmt.Errorf("store: remote join event_auth %s: %w", pdu.EventID, err)
				}
			}
		}

		return nil
	}

	if err := insertEvents(authPDUs, remoteOrigin, true); err != nil {
		return err
	}

	if err := insertEvents(statePDUs, remoteOrigin, true); err != nil {
		return err
	}

	if err := insertEvents([]FedPDU{joinPDU}, remoteOrigin, false); err != nil {
		return err
	}

	// Состояние: присланный state - состояние ДО join (state resolution v2 с
	// локальным состоянием на prev join, если оно есть: неавторизованное событие из
	// ответа не перезаписывает локальное); join - поверх, extremities
	// сдвигаются, room_state и room_memberships - по итогу.
	if err := applyRemoteJoinState(ctx, roomState, roomID, statePDUs, joinPDU); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO destination_rooms (destination, room_id) VALUES ($1,$2)
		ON CONFLICT DO NOTHING`, remoteOrigin, roomID); err != nil {
		return fmt.Errorf("store: remote join destination room: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: remote join commit: %w", err)
	}

	return nil
}

// remoteRoomCreator - создатель комнаты по m.room.create: content.creator
// (v1-v10), иначе отправитель create (v11 убрал creator из content).
func remoteRoomCreator(state []FedPDU) string {
	for i := range state {
		pdu := &state[i]
		if pdu.Type != "m.room.create" || pdu.StateKey == nil || *pdu.StateKey != "" {
			continue
		}

		var content struct {
			Creator string `json:"creator"`
		}

		if err := json.Unmarshal(pdu.Content, &content); err == nil && content.Creator != "" {
			return content.Creator
		}

		return pdu.Sender
	}

	return ""
}

// pduFromMap собирает FedPDU из произвольного JSON-объекта события.
func pduFromMap(object map[string]any) (FedPDU, error) {
	pdu := FedPDU{
		EventID: stringFromAny(object["event_id"]),
		RoomID:  stringFromAny(object["room_id"]),
		Type:    stringFromAny(object["type"]),
		Sender:  stringFromAny(object["sender"]),
	}

	// m.room.create комнаты v12 без room_id: room_id - его event_id с «!», PDU
	// хранится как пришел, без room_id.
	if stateKey, ok := object["state_key"].(string); ok && stateKey == "" && pdu.Type == "m.room.create" &&
		object["room_id"] == nil && strings.HasPrefix(pdu.EventID, "$") {
		pdu.RoomID = "!" + pdu.EventID[1:]
	}

	if pdu.EventID == "" || pdu.RoomID == "" || pdu.Sender == "" {
		return FedPDU{}, fmt.Errorf("missing event_id/room_id/sender")
	}

	if content, ok := object["content"]; ok {
		raw, err := json.Marshal(content)
		if err != nil {
			return FedPDU{}, err
		}

		pdu.Content = raw
	}

	if stateKey, ok := object["state_key"].(string); ok {
		pdu.StateKey = &stateKey
	}

	pdu.Prev = stringsFromAny(object["prev_events"])
	pdu.Auth = stringsFromAny(object["auth_events"])
	pdu.Depth = int64FromAny(object["depth"])
	pdu.OriginTS = int64FromAny(object["origin_server_ts"])

	// signatures/hashes обязаны переживать приём: без них мы пересылаем события
	// пира без его подписи, и он не может их проверить («Signature on retrieved
	// event … invalid»). pduFromMap терял оба поля.
	if sigs, ok := object["signatures"]; ok && sigs != nil {
		if raw, err := json.Marshal(sigs); err == nil {
			pdu.Signatures = raw
		}
	}

	if hashes, ok := object["hashes"]; ok && hashes != nil {
		if raw, err := json.Marshal(hashes); err == nil {
			pdu.Hashes = raw
		}
	}

	// Исходный PDU - объект без event_id: его добавляет исходящий join себе для
	// хранения, а в PDU комнат v3+ (у нас 10/11) поля нет.
	wire := make(map[string]any, len(object))

	for key, value := range object {
		if key != "event_id" {
			wire[key] = value
		}
	}

	raw, err := core.MarshalPDU(wire)
	if err != nil {
		return FedPDU{}, err
	}

	pdu.PDU = raw

	return pdu, nil
}

// stringFromAny — строка из any.
func stringFromAny(v any) string {
	s, _ := v.(string)

	return s
}

// stringsFromAny — []string из []any.
func stringsFromAny(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))

	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

// int64FromAny — int64 из float64/json.Number.
func int64FromAny(v any) int64 {
	switch value := v.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case json.Number:
		n, _ := value.Int64()

		return n
	}

	return 0
}

// domainOfMatrixID — server_name идентификатора @user:domain / !room:domain: все
// после первого двоеточия, server_name может содержать порт (@u:example.com:8448
// -> example.com:8448). Раньше бралось после последнего двоеточия.
func domainOfMatrixID(id string) string {
	if _, server, found := strings.Cut(id, ":"); found {
		return server
	}

	return ""
}

// membershipOf — membership из content события m.room.member.
func membershipOf(content []byte) string {
	var parsed struct {
		Membership string `json:"membership"`
	}

	if err := json.Unmarshal(content, &parsed); err != nil {
		return ""
	}

	return parsed.Membership
}

// ApplyLocalMembershipChange пишет локальное membership-событие (наш пользователь вышел из чужой
// комнаты через S2S leave или отказался от приглашения out-of-band): событие, состояние
// на событие, forward extremities, room_state и room_memberships одной транзакцией
// под блокировкой комнаты. Событие уже принято резидентным сервером
// (send_leave) или касается только нашего пользователя (отказ от приглашения в
// комнату, где у нас нет состояния), поэтому по состоянию до него не проверяется.
func (db *DB) ApplyLocalMembershipChange(ctx context.Context, pdu FedPDU) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: local membership begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockRoomState(ctx, tx, pdu.RoomID); err != nil {
		return err
	}

	prev := pdu.Prev
	if prev == nil {
		prev = []string{}
	}

	auth := pdu.Auth
	if auth == nil {
		auth = []string{}
	}

	origin := domainFromMatrixID(pdu.Sender)

	if _, err := tx.Exec(ctx, `
		INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events,
		                    auth_events, depth, origin_server_ts, origin, signatures, hashes, pdu_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (event_id) DO NOTHING`,
		pdu.EventID, pdu.RoomID, pdu.Type, pdu.Sender, pdu.StateKey, pdu.Content,
		prev, auth, pdu.Depth, pdu.OriginTS, origin, nullJSON(pdu.Signatures), nullJSON(pdu.Hashes),
		nullText(string(pdu.PDU))); err != nil {
		return fmt.Errorf("store: local membership event: %w", err)
	}

	if err := applyTrustedEventState(ctx, newRoomStateTx(tx, pdu.RoomID), pdu); err != nil {
		return fmt.Errorf("store: local membership state: %w", err)
	}

	return tx.Commit(ctx)
}

// domainFromMatrixID — server_name идентификатора (@user:domain), как domainOfMatrixID.
func domainFromMatrixID(id string) string { return domainOfMatrixID(id) }

// StateRow — строка материализованного состояния (тип - в core).
type StateRow = core.StateRow

// StateEventIDs возвращает текущее состояние комнаты (event_id по ключу).
func (db *DB) StateEventIDs(ctx context.Context, roomID string) ([]StateRow, error) {
	const query = `SELECT type, state_key, event_id FROM room_state WHERE room_id = $1`

	rows, err := db.pool.Query(ctx, query, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: state event ids: %w", err)
	}

	defer rows.Close()

	var out []StateRow

	for rows.Next() {
		var row StateRow
		if err := rows.Scan(&row.Type, &row.StateKey, &row.EventID); err != nil {
			return nil, fmt.Errorf("store: scan state row: %w", err)
		}

		out = append(out, row)
	}

	return out, rows.Err()
}

// unresolvedRefs возвращает id, которых нет в known.
func unresolvedRefs(ids []string, known map[string]struct{}) []string {
	var missing []string

	for _, id := range ids {
		if _, ok := known[id]; !ok {
			missing = append(missing, id)
		}
	}

	return missing
}
