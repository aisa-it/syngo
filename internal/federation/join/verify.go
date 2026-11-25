package join

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/auth"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// checkedJoin - проверенный ответ send_join: события с пересчитанным event_id,
// прошедшие подпись и auth rules.
type checkedJoin struct {
	state     []map[string]any
	authChain []map[string]any
	accepted  map[string]struct{}
}

// verify - проверка для ApplyRemoteJoin: событие должно быть из проверенного набора.
func (c checkedJoin) verify(object map[string]any) error {
	id, _ := object["event_id"].(string)
	if _, ok := c.accepted[id]; !ok {
		return fmt.Errorf("%w: event %s was not checked", ErrBadState, id)
	}

	return nil
}

// authTypes - типы событий, допустимые в auth_events (спека, auth events selection).
var authTypes = map[string]struct{}{
	"m.room.create": {}, "m.room.power_levels": {}, "m.room.join_rules": {},
	"m.room.member": {}, "m.room.third_party_invite": {},
}

// checkSendJoinResponse проверяет state и auth_chain ответа send_join до записи.
// Раньше событие принималось по любой валидной подписи, room_id не
// сверялся, присланный event_id сохранялся, auth rules не проверялись: пир
// подбрасывал события в чужие комнаты от имени чужих пользователей.
//
//   - room_id каждого события - эта комната, иначе отказ всего ответа;
//   - event_id всегда пересчитывается по правилам версии комнаты, присланный
//     игнорируется;
//   - hashes обязательны; при несовпадении content hash событие редактируется
//     (как _check_sigs_and_hash Synapse);
//   - подпись сервера отправителя (домен sender) и, для restricted join, сервера
//     join_authorised_via_users_server - по redaction нужной версии;
//   - состояние до join: только state-события, без дублей ключей, ровно один
//     m.room.create нужной версии;
//   - auth rules: каждое событие авторизовано своими auth_events из ответа;
//     не прошедшее (и зависящее от него) отбрасывается, как rejected в Synapse.
//     Отвергнутый create или join, не авторизованный состоянием, - отказ.
func (j *Joiner) checkSendJoinResponse(
	ctx context.Context, roomID, roomVersionRaw string, roomVersion int,
	joinEvent map[string]any, state, authChain []map[string]any,
) (checkedJoin, error) {
	lookup := j.lookupForJoin(ctx)

	events := make(map[string]map[string]any, len(state)+len(authChain))

	prepare := func(list []map[string]any) ([]string, error) {
		ids := make([]string, 0, len(list))

		for _, object := range list {
			id, prepared, err := prepareRemoteEvent(object, roomID, roomVersion, lookup)
			if err != nil {
				return nil, err
			}

			if _, seen := events[id]; !seen {
				events[id] = prepared
			}

			ids = append(ids, id)
		}

		return ids, nil
	}

	stateIDs, err := prepare(state)
	if err != nil {
		return checkedJoin{}, err
	}

	authIDs, err := prepare(authChain)
	if err != nil {
		return checkedJoin{}, err
	}

	createID, err := checkStateShape(stateIDs, events, roomID, roomVersionRaw, roomVersion)
	if err != nil {
		return checkedJoin{}, err
	}

	checker := &authChecker{
		events: events, result: map[string]error{}, visiting: map[string]bool{},
		hashRoom: roomVersion >= core.HashRoomIDVersion, createID: createID,
	}

	if err := checker.check(createID); err != nil {
		return checkedJoin{}, fmt.Errorf("%w: m.room.create rejected: %w", ErrBadState, err)
	}

	out := checkedJoin{accepted: map[string]struct{}{}}

	for _, id := range sortedByDepth(stateIDs, events) {
		if err := checker.check(id); err != nil {
			j.debugf("join: state event rejected", "event_id", id, "error", err)

			continue
		}

		if _, dup := out.accepted[id]; !dup {
			out.accepted[id] = struct{}{}
			out.state = append(out.state, events[id])
		}
	}

	authAccepted := map[string]struct{}{}

	for _, id := range sortedByDepth(authIDs, events) {
		if err := checker.check(id); err != nil {
			j.debugf("join: auth_chain event rejected", "event_id", id, "error", err)

			continue
		}

		if _, dup := authAccepted[id]; !dup {
			authAccepted[id] = struct{}{}
			out.accepted[id] = struct{}{}
			out.authChain = append(out.authChain, events[id])
		}
	}

	// Наш join обязан быть авторизован присланным состоянием: иначе комната
	// записалась бы с членством, которого у резидента нет.
	if err := checker.checkWith(authEventFromMap(joinEvent, ""), stringList(joinEvent["auth_events"])); err != nil {
		return checkedJoin{}, fmt.Errorf("%w: join is not authorized by remote state: %w", ErrBadState, err)
	}

	return out, nil
}

// prepareRemoteEvent проверяет одно событие ответа и возвращает его с
// пересчитанным event_id.
func prepareRemoteEvent(
	object map[string]any, roomID string, roomVersion int,
	lookup func(origin, keyID string) (string, int64, error),
) (eventID string, prepared map[string]any, err error) {
	eventType, _ := object["type"].(string)
	sender, _ := object["sender"].(string)
	stateKey, isState := object["state_key"].(string)

	// v12: у m.room.create нет room_id - его id и есть room_id (сверяется ниже,
	// после пересчета id); у остальных room_id - эта комната.
	hashCreate := roomVersion >= core.HashRoomIDVersion && eventType == "m.room.create" && isState && stateKey == ""

	switch got, present := object["room_id"]; {
	case hashCreate && present:
		return "", nil, fmt.Errorf("%w: m.room.create of v12 room with room_id", ErrBadState)
	case !hashCreate && got != roomID:
		return "", nil, fmt.Errorf("%w: event of room %v in send_join of %s", ErrBadState, object["room_id"], roomID)
	}

	if eventType == "" || domainOf(sender) == "" {
		return "", nil, fmt.Errorf("%w: event without type or sender", ErrBadState)
	}

	// В PDU v3+ event_id нет: присланный (если есть) не доверяем, считаем свой.
	wire := make(map[string]any, len(object))

	for key, value := range object {
		if key != "event_id" {
			wire[key] = value
		}
	}

	id, err := computeEventID(wire, roomVersion)
	if err != nil {
		return "", nil, fmt.Errorf("%w: event_id: %w", ErrBadState, err)
	}

	if hashCreate && "!"+strings.TrimPrefix(id, "$") != roomID {
		return "", nil, fmt.Errorf("%w: m.room.create %s is not the create of %s", ErrBadState, id, roomID)
	}

	for _, server := range requiredSigners(wire) {
		if err := eventjson.VerifyVersioned(wire, server, roomVersion, lookup); err != nil {
			return "", nil, fmt.Errorf("%w: signature of %s on %s %s: %w", ErrBadState, server, eventType, id, err)
		}
	}

	hashes, _ := wire["hashes"].(map[string]any)
	expected, _ := hashes["sha256"].(string)

	if expected == "" {
		return "", nil, fmt.Errorf("%w: event %s without hashes", ErrBadState, id)
	}

	actual, err := signing.ContentHash(wire)
	if err != nil {
		return "", nil, fmt.Errorf("%w: content hash %s: %w", ErrBadState, id, err)
	}

	// Подпись верна, а content hash нет: событие подменено по дороге - берем
	// редактированную копию (так поступает Synapse), id от этого не меняется.
	if actual != expected {
		wire = genericCopy(eventjson.RedactVersioned(wire, roomVersion))
		delete(wire, "event_id")
	}

	wire["event_id"] = id

	return id, wire, nil
}

// requiredSigners - серверы, чья подпись обязательна (спека, room v3+): сервер
// отправителя, а у join через restricted-комнату - и сервер авторизовавшего.
func requiredSigners(object map[string]any) []string {
	sender, _ := object["sender"].(string)
	signers := []string{domainOf(sender)}

	if eventType, _ := object["type"].(string); eventType == "m.room.member" {
		content, _ := object["content"].(map[string]any)

		if membership, _ := content["membership"].(string); membership == "join" {
			if via, _ := content["join_authorised_via_users_server"].(string); via != "" {
				if server := domainOf(via); server != "" && server != signers[0] {
					signers = append(signers, server)
				}
			}
		}
	}

	return signers
}

// checkStateShape проверяет состояние до join: только state-события, без дублей
// ключей, ровно один m.room.create с версией комнаты из make_join; в комнате
// v12 room_id - хеш этого create.
func checkStateShape(
	stateIDs []string, events map[string]map[string]any, roomID, roomVersionRaw string, roomVersion int,
) (string, error) {
	type key struct{ eventType, stateKey string }

	seen := map[key]string{}

	var createID string

	for _, id := range stateIDs {
		object := events[id]
		eventType, _ := object["type"].(string)

		stateKey, ok := object["state_key"].(string)
		if !ok {
			return "", fmt.Errorf("%w: non-state event %s in state", ErrBadState, id)
		}

		k := key{eventType, stateKey}
		if prev, dup := seen[k]; dup && prev != id {
			return "", fmt.Errorf("%w: two events for %s/%s in state", ErrBadState, eventType, stateKey)
		}

		seen[k] = id

		if eventType == "m.room.create" && stateKey == "" {
			createID = id
		}
	}

	if createID == "" {
		return "", fmt.Errorf("%w: no m.room.create in state", ErrBadState)
	}

	content, _ := events[createID]["content"].(map[string]any)

	version, _ := content["room_version"].(string)
	if version == "" {
		version = "1"
	}

	if version != roomVersionRaw {
		return "", fmt.Errorf("%w: m.room.create room_version %q, make_join %q", ErrBadState, version, roomVersionRaw)
	}

	if hashRoom := auth.IsCreateHashRoomID(roomID); hashRoom != (roomVersion >= core.HashRoomIDVersion) ||
		(hashRoom && "!"+strings.TrimPrefix(createID, "$") != roomID) {
		return "", fmt.Errorf("%w: room_id %s does not match m.room.create %s of room v%s", ErrBadState,
			roomID, createID, roomVersionRaw)
	}

	return createID, nil
}

// authChecker - проверка auth rules событий ответа по их auth_events (обход в
// глубину с запоминанием): событие принято, если приняты все его auth_events
// и оно проходит правила по состоянию из них.
type authChecker struct {
	events   map[string]map[string]any
	result   map[string]error
	visiting map[string]bool
	// hashRoom - комната v12: create не в auth_events, а неявное auth-событие
	// каждого события (createID).
	hashRoom bool
	createID string
}

func (c *authChecker) check(id string) error {
	if err, done := c.result[id]; done {
		return err
	}

	object, ok := c.events[id]
	if !ok {
		return fmt.Errorf("%w: auth event %s is missing from response", auth.ErrUnauthorized, id)
	}

	if c.visiting[id] {
		return fmt.Errorf("%w: auth_events cycle at %s", auth.ErrUnauthorized, id)
	}

	c.visiting[id] = true
	err := c.checkWith(authEventFromMap(object, id), stringList(object["auth_events"]))
	c.visiting[id] = false
	c.result[id] = err

	return err
}

// checkWith авторизует событие по указанным auth_events.
func (c *authChecker) checkWith(ev auth.Event, authIDs []string) error {
	if ev.Type == "m.room.create" && ev.StateKey != nil && *ev.StateKey == "" {
		if len(authIDs) > 0 {
			return fmt.Errorf("%w: m.room.create with auth_events", auth.ErrUnauthorized)
		}

		return auth.Check(ev, auth.State{Create: &ev})
	}

	type key struct{ eventType, stateKey string }

	seen := map[key]struct{}{}
	authEvents := make([]auth.Event, 0, len(authIDs)+1)

	var hasCreate bool

	if c.hashRoom {
		if slices.Contains(authIDs, c.createID) {
			return fmt.Errorf("%w: m.room.create in auth_events of v12 room", auth.ErrUnauthorized)
		}

		authIDs = append([]string{c.createID}, authIDs...)
	}

	for _, authID := range authIDs {
		if err := c.check(authID); err != nil {
			return fmt.Errorf("auth event %s rejected: %w", authID, err)
		}

		authEvent := authEventFromMap(c.events[authID], authID)

		if _, ok := authTypes[authEvent.Type]; !ok || authEvent.StateKey == nil {
			return fmt.Errorf("%w: auth event %s of type %q", auth.ErrUnauthorized, authID, authEvent.Type)
		}

		k := key{authEvent.Type, *authEvent.StateKey}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("%w: duplicate auth events for %s/%s", auth.ErrUnauthorized, k.eventType, k.stateKey)
		}

		seen[k] = struct{}{}

		if authEvent.Type == "m.room.create" {
			hasCreate = true
		}

		authEvents = append(authEvents, authEvent)
	}

	if !hasCreate {
		return fmt.Errorf("%w: auth_events without m.room.create", auth.ErrUnauthorized)
	}

	state, err := auth.BuildState(authEvents)
	if err != nil {
		return err
	}

	return auth.Check(ev, state)
}

// authEventFromMap - событие для auth rules из JSON-объекта.
func authEventFromMap(object map[string]any, id string) auth.Event {
	ev := auth.Event{
		EventID: id,
		RoomID:  stringValue(object["room_id"]),
		Type:    stringValue(object["type"]),
		Sender:  stringValue(object["sender"]),
		Prev:    stringList(object["prev_events"]),
	}

	if stateKey, ok := object["state_key"].(string); ok {
		ev.StateKey = &stateKey
	}

	// m.room.create комнаты v12 без room_id: room_id - его id с «!».
	if ev.RoomID == "" && ev.Type == "m.room.create" && strings.HasPrefix(id, "$") {
		ev.RoomID = "!" + id[1:]
	}

	if raw, err := json.Marshal(object["content"]); err == nil {
		ev.Content = raw
	}

	switch depth := object["depth"].(type) {
	case float64:
		ev.Depth = int64(depth)
	case json.Number:
		ev.Depth, _ = depth.Int64()
	}

	return ev
}

// stringList - []string из []any или []string.
func stringList(value any) []string {
	if list, ok := value.([]string); ok {
		return list
	}

	return stringsFromAny(value)
}

// sortedByDepth - id в порядке depth (затем id): детерминированный порядок
// записи, auth-события раньше зависящих от них.
func sortedByDepth(ids []string, events map[string]map[string]any) []string {
	out := append([]string(nil), ids...)

	depth := func(id string) float64 {
		d, _ := events[id]["depth"].(float64)

		return d
	}

	sort.SliceStable(out, func(a, b int) bool {
		if depth(out[a]) != depth(out[b]) {
			return depth(out[a]) < depth(out[b])
		}

		return out[a] < out[b]
	})

	return out
}

// genericCopy приводит объект к generic-виду (map[string]any) через JSON.
func genericCopy(object map[string]any) map[string]any {
	raw, err := json.Marshal(object)
	if err != nil {
		return object
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return object
	}

	return out
}

// debugf пишет в отладочный лог, если он подключен.
func (j *Joiner) debugf(msg string, args ...any) {
	if j.debug != nil {
		j.debug(msg, args...)
	}
}
