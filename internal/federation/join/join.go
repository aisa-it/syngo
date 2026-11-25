// Package join реализует исходящий join-флоу федерации (ADR-0007):
// наш пользователь входит в комнату на чужом сервере — make_join → подпись → send_join →
// применение полученного состояния одной транзакцией.
package join

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/federation/transport"
	"github.com/aisa-it/syngo/internal/store"
)

// Ошибки исходящего join.
var (
	ErrRemoteRejected = errors.New("join: remote server rejected join")
	ErrNoEvent        = errors.New("join: remote make_join returned no event")
	ErrBadResponse    = errors.New("join: malformed remote response")
	// ErrUnsupportedRoomVersion — версия комнаты вне нашего скоупа (auth/redaction v10..v12).
	ErrUnsupportedRoomVersion = errors.New("join: unsupported room version")
	// ErrBadTemplate - шаблон make_join/make_leave не то событие, что мы просили.
	ErrBadTemplate = errors.New("join: invalid membership template")
	// ErrBadState - ответ send_join не прошел проверку (подписи, auth rules, create).
	ErrBadState = errors.New("join: invalid send_join state")
)

// Sender — транспорт федерации (реализуется transport.Client).
type Sender interface {
	Send(ctx context.Context, destination, method, path string, body []byte) ([]byte, int, error)
}

// Store — применение результата join к локальной БД.
type Store interface {
	// ApplyRemoteJoin пишет комнату, state/auth_chain/event и membership в одной транзакции
	// и ставит join-событие в outbox. Если verify вернул ошибку — транзакция откатывается.
	ApplyRemoteJoin(ctx context.Context, roomID, roomVersion, userID, origin string, event map[string]any,
		state, authChain []map[string]any, verify func(map[string]any) error) error
	// ApplyLocalMembershipChange пишет локальное membership-событие (S2S leave).
	ApplyLocalMembershipChange(ctx context.Context, pdu store.FedPDU) error
}

// Joiner — исходящий join в чужую комнату.
type Joiner struct {
	client Sender
	store  Store
	key    *signing.Key
	origin string
	// debug — необязательный логгер (ставится в проде на slog.Debug).
	debug func(msg string, args ...any)
	// keys - поиск ключей серверов (SetKeyLookup); nil - у самого сервера.
	keys KeyLookup
}

// New создаёт Joiner.
func New(client Sender, st Store, key *signing.Key, origin string) *Joiner {
	return &Joiner{client: client, store: st, key: key, origin: origin}
}

// logPayload пишет исходящий PDU в лог (base64) для разбора парсером Synapse.
func (j *Joiner) logPayload(payload []byte) {
	if j.debug == nil {
		return
	}

	j.debug("api: outgoing join pdu", "b64", base64.RawStdEncoding.EncodeToString(payload))
}

// SetDebugLogger включает отладочный лог исходящих PDU (тело в base64).
func (j *Joiner) SetDebugLogger(fn func(msg string, args ...any)) {
	j.debug = fn
}

// Join выполняет make_join → send_join → применение состояния через сервер комнаты.
func (j *Joiner) Join(ctx context.Context, roomID, userID string) error {
	return j.JoinVia(ctx, roomID, userID, nil)
}

// JoinVia - Join через серверы-кандидаты (servers из ответа /query/directory):
// make_join пробуется на каждом по очереди, send_join идет тому, кто
// ответил годным шаблоном, как _try_destination_list в Synapse. Сервер из
// room_id - последний кандидат. Шаблон и ответ send_join проверяются одинаково
// для любого кандидата.
func (j *Joiner) JoinVia(ctx context.Context, roomID, userID string, servers []string) error {
	if j.key == nil {
		return errors.New("join: no signing key")
	}

	candidates := j.joinCandidates(roomID, servers)
	if len(candidates) == 0 {
		if domainOf(roomID) == j.origin {
			return fmt.Errorf("%w: room is local", ErrBadResponse)
		}

		return fmt.Errorf("%w: invalid room id %q", ErrBadResponse, roomID)
	}

	var lastErr error

	for _, remote := range candidates {
		template, err := j.makeJoin(ctx, remote, roomID, userID)
		if err != nil {
			lastErr = err

			continue
		}

		return j.sendJoin(ctx, remote, roomID, userID, template)
	}

	return lastErr
}

// joinCandidates - серверы для make_join без повторов и без своего: сначала
// переданные, затем сервер из room_id.
func (j *Joiner) joinCandidates(roomID string, servers []string) []string {
	seen := map[string]bool{"": true, j.origin: true}
	out := make([]string, 0, len(servers)+1)

	for _, server := range append(append([]string{}, servers...), domainOf(roomID)) {
		if seen[server] {
			continue
		}

		seen[server] = true
		out = append(out, server)
	}

	return out
}

// joinTemplate - проверенный шаблон make_join: версия комнаты и заполненное
// join-событие.
type joinTemplate struct {
	roomVersionRaw string
	roomVersion    int
	event          map[string]any
}

// makeJoin получает шаблон join-события от remote и проверяет его до подписи.
func (j *Joiner) makeJoin(ctx context.Context, remote, roomID, userID string) (joinTemplate, error) {
	// ?ver= обязателен: Synapse проверяет, поддерживает ли наш сервер версию комнаты,
	// и без списка отвечает 400 M_INCOMPATIBLE_ROOM_VERSION. Объявляются
	// только версии, правила которых реализованы: 10..12 (v12; раньше
	// ver=12 давал шаблон, который отвергался уже после send_join).
	path := "/_matrix/federation/v1/make_join/" + url.PathEscape(roomID) + "/" + url.PathEscape(userID) +
		supportedVersionsQuery()

	body, status, err := j.client.Send(ctx, remote, http.MethodGet, path, nil)
	if err != nil {
		return joinTemplate{}, fmt.Errorf("join: make_join: %w", err)
	}

	if status/100 != 2 {
		return joinTemplate{}, fmt.Errorf("%w: make_join HTTP %d: %s", ErrRemoteRejected, status, rejectedBody(body))
	}

	var makeJoin struct {
		RoomVersion *string        `json:"room_version"`
		Event       map[string]any `json:"event"`
	}

	if err := json.Unmarshal(body, &makeJoin); err != nil {
		return joinTemplate{}, fmt.Errorf("%w: %w", ErrBadResponse, err)
	}

	if makeJoin.Event == nil {
		return joinTemplate{}, ErrNoEvent
	}

	// Версия комнаты проверяется до подписи и send_join: от нее зависят правила
	// redaction при подсчете event_id и подписи (v11 != v10), и комнату с
	// нереализованными правилами лучше не начинать.
	roomVersionRaw, roomVersion, err := templateRoomVersion(makeJoin.RoomVersion)
	if err != nil {
		return joinTemplate{}, err
	}

	// Шаблон проверяется до подписи: раньше подписывалось что угодно - пир
	// получал нашу подпись под m.room.message.
	event, err := membershipTemplate(makeJoin.Event, roomID, userID, "join")
	if err != nil {
		return joinTemplate{}, err
	}

	return joinTemplate{roomVersionRaw: roomVersionRaw, roomVersion: roomVersion, event: event}, nil
}

// sendJoin подписывает шаблон, отправляет send_join на remote, проверяет ответ
// и применяет состояние.
func (j *Joiner) sendJoin(ctx context.Context, remote, roomID, userID string, template joinTemplate) error {
	roomVersionRaw, roomVersion, event := template.roomVersionRaw, template.roomVersion, template.event

	signed, eventID, payload, err := j.signTemplate(event, roomVersion)
	if err != nil {
		return err
	}

	// 3. send_join: v2 для room v3+.
	sendPath := "/_matrix/federation/v2/send_join/" + url.PathEscape(roomID) + "/" + url.PathEscape(eventID)

	// Ответ send_join - все состояние комнаты с auth chain: общий лимит ответа
	// федерации (1 МиБ) большую комнату не вмещает.
	response, status, err := j.sendLarge(ctx, remote, http.MethodPut, sendPath, payload, SendJoinMaxResponseBytes)
	if err != nil {
		return fmt.Errorf("join: send_join: %w", err)
	}

	if status/100 != 2 {
		return fmt.Errorf("%w: send_join HTTP %d: %s", ErrRemoteRejected, status, rejectedBody(response))
	}

	if j.debug != nil {
		j.debug("api: send_join response", "b64", base64.RawStdEncoding.EncodeToString(response))
	}

	resp, err := parseSendJoinResponse(response)
	if err != nil {
		return err
	}

	// restricted join: резидент добавляет свою подпись (join_authorised_via_users_server)
	// и возвращает событие в ответе - сохраняем его подписи вместе с нашей.
	mergeReturnedSignatures(signed, resp.event, eventID, roomVersion)

	// 4. Проверка ответа до записи: подпись сервера-отправителя каждого события,
	// room_id, пересчитанный event_id, auth rules, create.
	checked, err := j.checkSendJoinResponse(ctx, roomID, roomVersionRaw, roomVersion, signed, resp.state, resp.authChain)
	if err != nil {
		return err
	}

	// Локальному хранилищу нужен event_id (в PDU его нет для room v3+).
	signed["event_id"] = eventID

	return j.store.ApplyRemoteJoin(ctx, roomID, roomVersionRaw, userID, remote, signed,
		checked.state, checked.authChain, checked.verify)
}

// SendJoinMaxResponseBytes - лимит ответа send_join. Synapse допускает 500 МиБ
// (MAX_RESPONSE_SIZE_SEND_JOIN); ответ разбирается в памяти целиком, поэтому
// здесь меньше - этого хватает комнатам на десятки тысяч участников.
const SendJoinMaxResponseBytes = 128 << 20

// largeSender - транспорт с параметрами запроса (transport.Client).
type largeSender interface {
	Do(ctx context.Context, destination, method, path string, body []byte,
		opts transport.RequestOptions) ([]byte, int, error)
}

// sendLarge - запрос с увеличенным лимитом ответа, если транспорт его умеет.
func (j *Joiner) sendLarge(
	ctx context.Context, destination, method, path string, body []byte, limit int64,
) (response []byte, status int, err error) {
	if client, ok := j.client.(largeSender); ok {
		return client.Do(ctx, destination, method, path, body, transport.RequestOptions{MaxResponseBytes: limit})
	}

	return j.client.Send(ctx, destination, method, path, body)
}

// supportedVersionsQuery - параметры ver= для make_join: версии комнат, правила
// которых реализованы (auth rules, redaction, state resolution).
func supportedVersionsQuery() string {
	var b strings.Builder

	for version := core.DefaultRoomVersion; version <= core.FederationMaxRoomVersion; version++ {
		if b.Len() == 0 {
			b.WriteString("?")
		} else {
			b.WriteString("&")
		}

		b.WriteString("ver=" + strconv.Itoa(version))
	}

	return b.String()
}

// templateRoomVersion - версия комнаты из ответа make_join/make_leave. По спеке
// без поля версия - "1": ее правила не реализованы, как и любые вне 10..12.
func templateRoomVersion(raw *string) (version string, parsed int, err error) {
	version = "1"
	if raw != nil {
		version = *raw
	}

	if !supportedRoomVersion(version) {
		return "", 0, fmt.Errorf("%w: %q (supported: %d..%d)", ErrUnsupportedRoomVersion, version,
			core.DefaultRoomVersion, core.FederationMaxRoomVersion)
	}

	parsed, _ = strconv.Atoi(version)

	return version, parsed, nil
}

// membershipTemplate проверяет шаблон make_join/make_leave и заполняет его: тип -
// m.room.member, membership - ожидаемый, sender/state_key/room_id, если сервер их
// указал, - наш пользователь и эта комната. Иначе наш ключ подписал бы событие,
// которое выбрал пир.
func membershipTemplate(template map[string]any, roomID, userID, membership string) (map[string]any, error) {
	if eventType, _ := template["type"].(string); eventType != "m.room.member" {
		return nil, fmt.Errorf("%w: type %v", ErrBadTemplate, template["type"])
	}

	content, ok := template["content"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: content is not an object", ErrBadTemplate)
	}

	if value, _ := content["membership"].(string); value != membership {
		return nil, fmt.Errorf("%w: membership %v, expected %s", ErrBadTemplate, content["membership"], membership)
	}

	for field, want := range map[string]string{"sender": userID, "state_key": userID, "room_id": roomID} {
		value, present := template[field]
		if !present {
			continue
		}

		if got, _ := value.(string); got != want {
			return nil, fmt.Errorf("%w: %s %v, expected %s", ErrBadTemplate, field, value, want)
		}
	}

	for _, field := range []string{"prev_events", "auth_events"} {
		if _, ok := template[field].([]any); !ok {
			return nil, fmt.Errorf("%w: %s is not a list", ErrBadTemplate, field)
		}
	}

	event := make(map[string]any, len(template)+4)

	for key, value := range template {
		switch key {
		// origin - поле формата v1/v2: для room v3+ Synapse отвергает PDU с ним
		// («Invalid event JSON»); event_id в PDU v3+ не входит («v2/v3
		// events must not have an explicit event_id»); hashes и подписи считаем сами.
		case "origin", "event_id", "hashes", "signatures", "unsigned":
			continue
		}

		event[key] = value
	}

	event["origin_server_ts"] = timeNowMillis()
	event["sender"] = userID
	event["state_key"] = userID
	event["room_id"] = roomID

	return event, nil
}

// signTemplate считает hashes и event_id события по правилам версии комнаты и
// подписывает его. Возвращает подписанный объект (без event_id), event_id и JSON
// для отправки.
func (j *Joiner) signTemplate(
	event map[string]any, roomVersion int,
) (signed map[string]any, eventID string, payload []byte, err error) {
	// hashes.sha256 обязателен: без него Synapse отвергает PDU («missing field
	// hashes»), и reference hash считается уже с ним.
	hash, err := signing.ContentHash(event)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: content hash: %w", ErrBadResponse, err)
	}

	event["hashes"] = map[string]string{"sha256": hash}

	// event_id для room v3+ не входит в PDU: считаем reference hash и кладем его
	// только в URL send_join/send_leave.
	eventID, err = computeEventID(event, roomVersion)
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: event_id: %w", ErrBadResponse, err)
	}

	signed, err = eventjson.SignVersioned(*j.key, j.origin, event, roomVersion)
	if err != nil {
		return nil, "", nil, err
	}

	payload, err = core.MarshalPDU(signed)
	if err != nil {
		return nil, "", nil, err
	}

	j.logPayload(payload)

	return signed, eventID, payload, nil
}

// sendJoinResponse - разобранный ответ send_join.
type sendJoinResponse struct {
	state     []map[string]any
	authChain []map[string]any
	// event - join-событие с подписью резидента (restricted join), если есть.
	event map[string]any
}

// parseSendJoinResponse разбирает ответ send_join: v1 [200, {state, auth_chain}]
// или v2 {state, auth_chain, event}.
func parseSendJoinResponse(raw []byte) (sendJoinResponse, error) {
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return sendJoinResponse{}, fmt.Errorf("%w: %w", ErrBadResponse, err)
	}

	// v1: [200, {...}]
	if array, ok := generic.([]any); ok {
		if len(array) != 2 {
			return sendJoinResponse{}, ErrBadResponse
		}

		generic = array[1]
	}

	object, ok := generic.(map[string]any)
	if !ok {
		return sendJoinResponse{}, ErrBadResponse
	}

	state, authChain, err := extractEvents(object)
	if err != nil {
		return sendJoinResponse{}, err
	}

	event, _ := object["event"].(map[string]any)

	return sendJoinResponse{state: state, authChain: authChain, event: event}, nil
}

// mergeReturnedSignatures добавляет к нашему join подписи из события ответа,
// если это то же событие (тот же event_id). Своя подпись не перезаписывается.
func mergeReturnedSignatures(signed, returned map[string]any, eventID string, roomVersion int) {
	if returned == nil {
		return
	}

	clone := make(map[string]any, len(returned))

	for key, value := range returned {
		if key != "event_id" {
			clone[key] = value
		}
	}

	if id, err := computeEventID(clone, roomVersion); err != nil || id != eventID {
		return
	}

	theirs, ok := returned["signatures"].(map[string]any)
	if !ok {
		return
	}

	merged := map[string]any{}

	for server, value := range theirs {
		merged[server] = value
	}

	switch ours := signed["signatures"].(type) {
	case map[string]any:
		for server, value := range ours {
			merged[server] = value
		}
	case map[string]map[string]string:
		for server, value := range ours {
			merged[server] = value
		}
	}

	signed["signatures"] = merged
}

// extractEvents достаёт state и auth_chain.
func extractEvents(object map[string]any) (state, authChain []map[string]any, err error) {
	toObjects := func(value any) ([]map[string]any, error) {
		raw, ok := value.([]any)
		if !ok {
			if value == nil {
				return nil, nil
			}

			return nil, ErrBadResponse
		}

		out := make([]map[string]any, 0, len(raw))

		for _, item := range raw {
			obj, ok := item.(map[string]any)
			if !ok {
				return nil, ErrBadResponse
			}

			out = append(out, obj)
		}

		return out, nil
	}

	if state, err = toObjects(object["state"]); err != nil {
		return nil, nil, err
	}

	if authChain, err = toObjects(object["auth_chain"]); err != nil {
		return nil, nil, err
	}

	return state, authChain, nil
}

// supportedRoomVersion — поддерживаем ли мы правила для этой версии комнаты.
func supportedRoomVersion(version string) bool {
	parsed, err := strconv.Atoi(version)

	return err == nil && strconv.Itoa(parsed) == version &&
		parsed >= core.DefaultRoomVersion && parsed <= core.FederationMaxRoomVersion
}

// timeNowMillis — текущее время в миллисекундах (вынесено для читаемости).
func timeNowMillis() int64 { return time.Now().UnixMilli() }

// domainOf возвращает server_name идентификатора Matrix: по грамматике спеки это
// все после первого двоеточия, и server_name сам может содержать порт -
// !r:example.com:8448 -> example.com:8448. Раньше бралось после последнего
// двоеточия и выходило "8448".
func domainOf(id string) string {
	if _, server, found := strings.Cut(id, ":"); found {
		return server
	}

	return ""
}

// Leave выполняет исходящий leave: make_leave → подпись → send_leave → локальное
// membership leave. PDU собирается как у join: версия комнаты из
// make_leave, шаблон проверяется, hashes, event_id по правилам версии только в
// URL, без event_id и origin в теле. Раньше leave уходил без hashes, с event_id
// и origin в теле, по правилам v10 - Synapse его отвергал.
func (j *Joiner) Leave(ctx context.Context, roomID, userID string) error {
	return j.LeaveVia(ctx, roomID, userID, nil)
}

// LeaveVia - Leave через серверы-кандидаты: у комнаты v12 в room_id домена нет,
// make_leave идет серверам, которые знает вызывающий (создатель, пригласивший,
// серверы участников); сервер из room_id - последний кандидат.
func (j *Joiner) LeaveVia(ctx context.Context, roomID, userID string, servers []string) error {
	if j.key == nil {
		return errors.New("join: no signing key")
	}

	candidates := j.joinCandidates(roomID, servers)
	if len(candidates) == 0 {
		return fmt.Errorf("%w: room is not remote", ErrBadResponse)
	}

	var lastErr error

	for _, remote := range candidates {
		if lastErr = j.leaveThrough(ctx, remote, roomID, userID); lastErr == nil {
			return nil
		}
	}

	return lastErr
}

// leaveThrough - make_leave/send_leave через один сервер.
func (j *Joiner) leaveThrough(ctx context.Context, remote, roomID, userID string) error {
	// У make_leave нет параметра ver (он только у make_join/make_knock).
	path := "/_matrix/federation/v1/make_leave/" + url.PathEscape(roomID) + "/" + url.PathEscape(userID)

	body, status, err := j.client.Send(ctx, remote, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("leave: make_leave: %w", err)
	}

	if status/100 != 2 {
		return fmt.Errorf("%w: make_leave HTTP %d", ErrRemoteRejected, status)
	}

	var makeLeave struct {
		RoomVersion *string        `json:"room_version"`
		Event       map[string]any `json:"event"`
	}

	if err := json.Unmarshal(body, &makeLeave); err != nil {
		return fmt.Errorf("%w: %w", ErrBadResponse, err)
	}

	if makeLeave.Event == nil {
		return ErrNoEvent
	}

	_, roomVersion, err := templateRoomVersion(makeLeave.RoomVersion)
	if err != nil {
		return err
	}

	event, err := membershipTemplate(makeLeave.Event, roomID, userID, "leave")
	if err != nil {
		return err
	}

	signed, eventID, payload, err := j.signTemplate(event, roomVersion)
	if err != nil {
		return err
	}

	// v2 отвечает {} (v1 - [200, {}]); Synapse поддерживает обе.
	sendPath := "/_matrix/federation/v2/send_leave/" + url.PathEscape(roomID) + "/" + url.PathEscape(eventID)

	leaveResp, status, err := j.client.Send(ctx, remote, http.MethodPut, sendPath, payload)
	if err != nil {
		return fmt.Errorf("leave: send_leave: %w", err)
	}

	if status/100 != 2 {
		return fmt.Errorf("%w: send_leave HTTP %d: %s", ErrRemoteRejected, status, rejectedBody(leaveResp))
	}

	signed["event_id"] = eventID

	pdu, err := fedPDUFromMap(signed)
	if err != nil {
		return err
	}

	// Исходный PDU - ровно то, что ушло в send_leave.
	pdu.PDU = payload

	return j.store.ApplyLocalMembershipChange(ctx, pdu)
}

// fedPDUFromMap собирает store.FedPDU из подписанного события.
func fedPDUFromMap(object map[string]any) (store.FedPDU, error) {
	pdu := store.FedPDU{
		EventID: stringValue(object["event_id"]),
		RoomID:  stringValue(object["room_id"]),
		Type:    stringValue(object["type"]),
		Sender:  stringValue(object["sender"]),
	}

	if pdu.EventID == "" || pdu.RoomID == "" {
		return store.FedPDU{}, ErrBadResponse
	}

	if content, ok := object["content"]; ok {
		raw, err := json.Marshal(content)
		if err != nil {
			return store.FedPDU{}, err
		}

		pdu.Content = raw
	}

	if stateKey, ok := object["state_key"].(string); ok {
		pdu.StateKey = &stateKey
	}

	if depth, ok := object["depth"].(float64); ok {
		pdu.Depth = int64(depth)
	}

	if ts, ok := object["origin_server_ts"].(float64); ok {
		pdu.OriginTS = int64(ts)
	}

	pdu.Prev = stringsFromAny(object["prev_events"])
	pdu.Auth = stringsFromAny(object["auth_events"])

	return pdu, nil
}

// stringsFromAny — []string из []any.
func stringsFromAny(value any) []string {
	raw, ok := value.([]any)
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

// stringValue безопасно достаёт строку из map.
func stringValue(v any) string {
	s, _ := v.(string)

	return s
}

// computeEventID считает event_id так же, как Synapse: «$» + base64url(sha256(канонический JSON))
// без полей event_id, signatures и unsigned.
func computeEventID(event map[string]any, version int) (string, error) {
	// Reference hash считается по **pruned** событию (как prune_event в Synapse):
	// content урезается по правилам redaction версии комнаты. Версию передаёт
	// вызывающий (make_join.RoomVersion), иначе join в v11-комнату считался бы
	// по правилам v10 и id расходился с Synapse.
	if version <= 0 {
		version = core.DefaultRoomVersion
	}

	return eventjson.EventID(event, version)
}

// rejectedBody — тело отказа удалённого сервера в одну строку (для логов: без
// него 502 не даёт понять причину).
func rejectedBody(body []byte) string {
	const limit = 400

	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > limit {
		text = text[:limit] + "…"
	}

	return text
}
