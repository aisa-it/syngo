package join_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/join"
	"github.com/aisa-it/syngo/internal/federation/signing"
	"github.com/aisa-it/syngo/internal/store"
)

const (
	remoteRoom = "!remote:remote.test"
	ourUser    = "@bob:ours.test"
)

// roomEvent подписывает событие комнаты по правилам версии: hashes, reference
// hash event_id, подпись по redaction версии (как Synapse). Возвращает id и PDU
// без event_id в generic-виде.
func roomEvent(
	t *testing.T, key signing.Key, server string, version int, event map[string]any,
) (eventID string, pdu map[string]any) {
	t.Helper()

	hash, err := signing.ContentHash(event)
	if err != nil {
		t.Fatalf("content hash: %v", err)
	}

	event["hashes"] = map[string]any{"sha256": hash}

	id, err := eventjson.EventID(event, version)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	signed, err := eventjson.SignVersioned(key, server, event, version)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return id, generic(t, signed)
}

func generic(t *testing.T, object map[string]any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	return out
}

// remoteRoomFixture - комната remote.test: create, join alice, power_levels,
// join_rules public (state и auth_chain ответа send_join).
type remoteRoomFixture struct {
	version int
	// roomID - remoteRoom; у комнаты v12 - хеш create.
	roomID string
	state  []map[string]any
	ids    map[string]string // тип -> event_id
	last   string
	depth  int
}

func newRoomFixture(t *testing.T, key signing.Key, version int) *remoteRoomFixture {
	t.Helper()

	room := &remoteRoomFixture{version: version, roomID: remoteRoom, ids: map[string]string{}}

	createContent := map[string]any{"room_version": fmt.Sprint(version)}
	if version < 11 {
		createContent["creator"] = "@alice:remote.test"
	}

	// v12: уровень создателя бесконечен и в power_levels не пишется.
	users := map[string]any{"@alice:remote.test": 100}
	if version >= 12 {
		users = map[string]any{}
	}

	room.add(t, key, "m.room.create", "", createContent, nil)
	room.add(t, key, "m.room.member", "@alice:remote.test", map[string]any{"membership": "join"},
		[]string{"m.room.create"})
	room.add(t, key, "m.room.power_levels", "",
		map[string]any{"users": users, "users_default": 0},
		[]string{"m.room.create", "m.room.member"})
	room.add(t, key, "m.room.join_rules", "", map[string]any{"join_rule": "public"},
		[]string{"m.room.create", "m.room.member", "m.room.power_levels"})

	return room
}

// add добавляет state-событие alice; authTypes - типы auth_events из уже добавленных.
func (r *remoteRoomFixture) add(t *testing.T, key signing.Key, eventType, stateKey string, content map[string]any, authTypes []string) string {
	t.Helper()

	authIDs := r.authIDs(authTypes)

	prev := []any{}
	if r.last != "" {
		prev = []any{r.last}
	}

	r.depth++

	event := map[string]any{
		"room_id": r.roomID, "type": eventType, "state_key": stateKey, "sender": "@alice:remote.test",
		"content": content, "depth": r.depth, "origin_server_ts": 1_700_000_000_000 + r.depth,
		"prev_events": prev, "auth_events": authIDs,
	}

	// v12: у create нет room_id, room_id комнаты - его id с «!».
	hashCreate := r.version >= 12 && eventType == "m.room.create"
	if hashCreate {
		delete(event, "room_id")
	}

	id, pdu := roomEvent(t, key, "remote.test", r.version, event)

	if hashCreate {
		r.roomID = "!" + strings.TrimPrefix(id, "$")
	}

	r.ids[eventType] = id
	r.last = id
	r.state = append(r.state, pdu)

	return id
}

// addJoin добавляет вход пользователя remote.test в публичную комнату.
func (r *remoteRoomFixture) addJoin(t *testing.T, key signing.Key, user string) {
	t.Helper()

	r.depth++

	id, pdu := roomEvent(t, key, "remote.test", r.version, map[string]any{
		"room_id": r.roomID, "type": "m.room.member", "state_key": user, "sender": user,
		"content": map[string]any{"membership": "join"}, "depth": r.depth,
		"origin_server_ts": 1_700_000_000_000 + r.depth, "prev_events": []any{r.last},
		"auth_events": r.authIDs([]string{"m.room.create", "m.room.power_levels", "m.room.join_rules"}),
	})

	r.last = id
	r.state = append(r.state, pdu)
}

// authIDs - id событий указанных типов для auth_events; в комнате v12 create
// в auth_events не входит.
func (r *remoteRoomFixture) authIDs(types []string) []any {
	out := make([]any, 0, len(types))

	for _, typ := range types {
		if r.version >= 12 && typ == "m.room.create" {
			continue
		}

		out = append(out, r.ids[typ])
	}

	return out
}

// template - шаблон make_join для ourUser.
func (r *remoteRoomFixture) template() map[string]any {
	return map[string]any{
		"type": "m.room.member", "room_id": r.roomID, "sender": ourUser, "state_key": ourUser,
		"content": map[string]any{"membership": "join"}, "depth": r.depth + 1, "origin": "remote.test",
		"origin_server_ts": 1, "prev_events": []any{r.last},
		"auth_events": r.authIDs([]string{"m.room.create", "m.room.power_levels", "m.room.join_rules"}),
	}
}

// remoteServer — «чужой» homeserver: отдаёт make_join/send_join и ключи.
type remoteServer struct {
	key        signing.Key
	mu         sync.Mutex
	requests   []string
	keyFetches int
	// roomVersion - версия в ответе make_join (по умолчанию - версия комнаты).
	roomVersion string
	room        *remoteRoomFixture
	// template - шаблон make_join (по умолчанию room.template()).
	template map[string]any
	// state/authChain - ответ send_join (по умолчанию состояние комнаты).
	state, authChain []map[string]any
	sendJoinBody     []byte
	// leaveBody - тело последнего send_leave.
	leaveBody []byte
}

func newRemoteServer(t *testing.T) (*remoteServer, *httptest.Server) {
	t.Helper()

	return newRemoteServerVersion(t, 10)
}

func newRemoteServerVersion(t *testing.T, version int) (*remoteServer, *httptest.Server) {
	t.Helper()

	key, err := signing.Generate("a_remote")
	if err != nil {
		t.Fatalf("remote key: %v", err)
	}

	remote := &remoteServer{key: key, roomVersion: fmt.Sprint(version)}
	remote.room = newRoomFixture(t, key, version)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.mu.Lock()
		defer remote.mu.Unlock()

		remote.requests = append(remote.requests, r.Method+" "+r.URL.RequestURI())

		switch {
		case strings.HasSuffix(r.URL.Path, "/_matrix/key/v2/server"):
			remote.keyFetches++

			signed, err := signing.SignJSON(key, "remote.test", map[string]any{
				"server_name": "remote.test", "valid_until_ts": time.Now().Add(time.Hour).UnixMilli(),
				"verify_keys": map[string]any{key.KeyID: map[string]any{"key": key.Public()}},
			})
			if err != nil {
				t.Errorf("sign keys: %v", err)

				return
			}

			_ = json.NewEncoder(w).Encode(signed)
		case strings.Contains(r.URL.Path, "/make_join/"):
			template := remote.template
			if template == nil {
				template = remote.room.template()
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"room_version": remote.roomVersion, "event": template})
		case strings.Contains(r.URL.Path, "/make_leave/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"room_version": "10",
				"event": map[string]any{
					"type": "m.room.member", "content": map[string]any{"membership": "leave"}, "depth": 7,
					"prev_events": []string{"$e:remote.test"}, "auth_events": []string{"$create:remote.test"},
				},
			})
		case strings.Contains(r.URL.Path, "/send_leave/"):
			body, _ := io.ReadAll(r.Body)
			remote.leaveBody = body

			_ = json.NewEncoder(w).Encode([]any{200, map[string]any{}})
		case strings.Contains(r.URL.Path, "/send_join/"):
			remote.sendJoinBody, _ = io.ReadAll(r.Body)

			state, authChain := remote.state, remote.authChain
			if state == nil {
				state, authChain = remote.room.state, remote.room.state
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"origin": "remote.test", "state": state, "auth_chain": authChain,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(server.Close)

	return remote, server
}

func (r *remoteServer) sentSendJoin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return strings.Contains(strings.Join(r.requests, " "), "send_join")
}

// fakeStore — применение join в памяти.
type fakeStore struct {
	applied     bool
	err         error
	roomVersion string
	state       []map[string]any
	authChain   []map[string]any
	joinEvent   map[string]any
	leave       store.FedPDU
}

// ApplyLocalMembershipChange — заглушка для исходящего leave.
func (f *fakeStore) ApplyLocalMembershipChange(_ context.Context, pdu store.FedPDU) error {
	f.applied = true
	f.leave = pdu

	return f.err
}

func (f *fakeStore) ApplyRemoteJoin(_ context.Context, roomID, roomVersion, userID, origin string, event map[string]any,
	state, authChain []map[string]any, verify func(map[string]any) error,
) error {
	f.roomVersion = roomVersion
	for i := range state {
		if err := verify(state[i]); err != nil {
			return err
		}
	}

	f.applied = true
	f.state = state
	f.authChain = authChain
	f.joinEvent = event

	return f.err
}

func (f *fakeStore) stateIDs() map[string]bool {
	out := map[string]bool{}

	for _, ev := range f.state {
		id, _ := ev["event_id"].(string)
		out[id] = true
	}

	return out
}

// client — транспорт, направляющий запросы на httptest-сервер.
type client struct{ base string }

func (c client) Send(ctx context.Context, destination, method, path string, body []byte) (respBody []byte, status int, err error) {
	_ = ctx
	_ = destination

	url := c.base + path

	req, err := http.NewRequest(method, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}

	return raw, resp.StatusCode, nil
}

func ourKey(t *testing.T) *signing.Key {
	t.Helper()

	key, err := signing.Generate("a_ours")
	if err != nil {
		t.Fatalf("our key: %v", err)
	}

	return &key
}

// TestJoinRemote — make_join → подпись → send_join → применение state.
func TestJoinRemote(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)
	st := &fakeStore{}

	joiner := join.New(client{base: server.URL}, st, ourKey(t), "ours.test")

	if err := joiner.Join(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("join: %v", err)
	}

	if !st.applied || len(st.state) != 4 {
		t.Fatalf("ApplyRemoteJoin: applied=%v state=%d", st.applied, len(st.state))
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()

	paths := strings.Join(remote.requests, " ")

	if !strings.Contains(paths, "make_join") || !strings.Contains(paths, "send_join") {
		t.Fatalf("ожидали make_join и send_join, получили: %s", paths)
	}
}

// TestJoinRemoteRejectsBadStateSignature — подделанная подпись state-события отклоняется,
// применение не происходит (транзакционность).
func TestJoinRemoteRejectsBadStateSignature(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)

	state := append([]map[string]any{}, remote.room.state...)
	broken := generic(t, state[2])
	broken["signatures"].(map[string]any)["remote.test"].(map[string]any)[remote.key.KeyID] = "AAAA"
	state[2] = broken
	remote.state, remote.authChain = state, state

	st := &fakeStore{}

	err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser)
	if !errors.Is(err, join.ErrBadState) {
		t.Fatalf("поддельная подпись state должна отклоняться, получили %v", err)
	}

	if st.applied {
		t.Fatal("при ошибке подписи состояние применяться не должно")
	}
}

// TestJoinRemoteUsesRoomVersion — версия комнаты из make_join доходит до хранилища.
// Комната v11: create подписан по redaction v11 (content целиком), раньше подпись
// проверялась по v10, и join в v11 падал после send_join.
func TestJoinRemoteUsesRoomVersion(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServerVersion(t, 11)
	st := &fakeStore{}

	if err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("join v11: %v", err)
	}

	if st.roomVersion != "11" || len(st.state) != 4 {
		t.Fatalf("room_version = %q, state = %d, ожидали 11 и 4", st.roomVersion, len(st.state))
	}

	var sent map[string]any
	if err := json.Unmarshal(remote.sendJoinBody, &sent); err != nil {
		t.Fatalf("send_join body: %v", err)
	}

	if _, ok := sent["event_id"]; ok {
		t.Fatalf("event_id в PDU v11: %v", sent)
	}

	if _, ok := sent["origin"]; ok {
		t.Fatalf("origin в PDU v11: %v", sent)
	}
}

// TestJoinRemoteRejectsUnsupportedRoomVersion — v9/v13 и версия не числом
// отвергаются до подписи и send_join; make_join объявляет только 10..12
// (v12; раньше ver=12 и проверка после send_join).
func TestJoinRemoteRejectsUnsupportedRoomVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"9", "13", "011"} {
		remote, server := newRemoteServer(t)
		remote.roomVersion = version

		st := &fakeStore{}

		err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
			Join(context.Background(), remoteRoom, ourUser)
		if !errors.Is(err, join.ErrUnsupportedRoomVersion) {
			t.Fatalf("версия %s: ожидали ErrUnsupportedRoomVersion, получили %v", version, err)
		}

		if st.applied || remote.sentSendJoin() {
			t.Fatalf("версия %s: send_join=%v applied=%v", version, remote.sentSendJoin(), st.applied)
		}

		remote.mu.Lock()
		makeJoin := remote.requests[0]
		remote.mu.Unlock()

		if !strings.HasSuffix(makeJoin, "?ver=10&ver=11&ver=12") {
			t.Fatalf("make_join объявляет версии: %s", makeJoin)
		}
	}
}

// TestJoinRemoteRejectsLocalRoom — локальная комната не идёт в федеративный join.
func TestJoinRemoteRejectsLocalRoom(t *testing.T) {
	t.Parallel()

	joiner := join.New(client{}, &fakeStore{}, ourKey(t), "ours.test")

	if err := joiner.Join(context.Background(), "!local:ours.test", ourUser); err == nil {
		t.Fatal("локальная комната должна отвергаться")
	}
}

// TestJoinTemplateValidated - шаблон make_join не m.room.member нашего
// пользователя в этой комнате отвергается до подписи: раньше наш ключ подписывал
// любое событие пира, например m.room.message.
func TestJoinTemplateValidated(t *testing.T) {
	t.Parallel()

	cases := map[string]func(map[string]any){
		"type":       func(tmpl map[string]any) { tmpl["type"] = "m.room.message" },
		"membership": func(tmpl map[string]any) { tmpl["content"] = map[string]any{"membership": "invite"} },
		"state_key":  func(tmpl map[string]any) { tmpl["state_key"] = "@victim:ours.test" },
		"sender":     func(tmpl map[string]any) { tmpl["sender"] = "@victim:ours.test" },
		"room_id":    func(tmpl map[string]any) { tmpl["room_id"] = "!other:remote.test" },
	}

	for name, mutate := range cases {
		remote, server := newRemoteServer(t)
		remote.template = remote.room.template()
		mutate(remote.template)

		st := &fakeStore{}

		err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
			Join(context.Background(), remoteRoom, ourUser)
		if !errors.Is(err, join.ErrBadTemplate) {
			t.Errorf("%s: ожидали ErrBadTemplate, получили %v", name, err)
		}

		if remote.sentSendJoin() || st.applied {
			t.Errorf("%s: подписанный шаблон ушел в send_join", name)
		}
	}
}

// foreignEvent - событие, подписанное только remote.test, с заданными полями.
func foreignEvent(t *testing.T, remote *remoteServer, fields map[string]any) map[string]any {
	t.Helper()

	event := map[string]any{
		"room_id": remoteRoom, "type": "m.room.topic", "state_key": "", "sender": "@alice:remote.test",
		"content": map[string]any{"topic": "x"}, "depth": 10, "origin_server_ts": 1,
		"prev_events": []any{remote.room.last},
		"auth_events": []any{
			remote.room.ids["m.room.create"], remote.room.ids["m.room.power_levels"],
			remote.room.ids["m.room.member"],
		},
	}

	for key, value := range fields {
		event[key] = value
	}

	_, pdu := roomEvent(t, remote.key, "remote.test", 10, event)

	return pdu
}

// TestJoinRejectsForeignRoomEvent - событие чужой комнаты (наша приватная) в
// ответе send_join отвергает ответ: раньше пир так подбрасывал сообщения в
// локальные комнаты.
func TestJoinRejectsForeignRoomEvent(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)
	forged := foreignEvent(t, remote, map[string]any{
		"room_id": "!private:ours.test", "type": "m.room.message", "sender": "@ceo:ours.test",
		"content": map[string]any{"msgtype": "m.text", "body": "переведите деньги"},
	})
	forged["event_id"] = "$attacker-chosen-id"
	remote.state = append(append([]map[string]any{}, remote.room.state...), forged)
	remote.authChain = remote.room.state

	st := &fakeStore{}

	err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser)
	if !errors.Is(err, join.ErrBadState) || st.applied {
		t.Fatalf("событие чужой комнаты: err=%v applied=%v", err, st.applied)
	}
}

// TestJoinRequiresSenderServerSignature - событие от имени пользователя другого
// сервера, подписанное только пиром, отвергается: нужна подпись сервера
// отправителя, а не любая валидная.
func TestJoinRequiresSenderServerSignature(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)
	forged := foreignEvent(t, remote, map[string]any{
		"type": "m.room.member", "state_key": "@ceo:ours.test", "sender": "@ceo:ours.test",
		"content": map[string]any{"membership": "join"},
	})
	remote.state = append(append([]map[string]any{}, remote.room.state...), forged)
	remote.authChain = remote.room.state

	st := &fakeStore{}

	err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser)
	if !errors.Is(err, join.ErrBadState) || st.applied {
		t.Fatalf("подпись не сервера отправителя: err=%v applied=%v", err, st.applied)
	}
}

// TestJoinRecomputesEventID - присланный event_id не сохраняется: id
// пересчитывается по событию.
func TestJoinRecomputesEventID(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)

	state := make([]map[string]any, 0, len(remote.room.state))
	for _, ev := range remote.room.state {
		copied := generic(t, ev)
		copied["event_id"] = "$attacker-chosen-id"
		state = append(state, copied)
	}

	remote.state, remote.authChain = state[:1], state[:1]
	// Остальное состояние - с правильными id (присланный id не используется вовсе).
	remote.state = append(remote.state, remote.room.state[1:]...)
	remote.authChain = remote.state

	st := &fakeStore{}

	if err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("join: %v", err)
	}

	ids := st.stateIDs()
	if ids["$attacker-chosen-id"] || !ids[remote.room.ids["m.room.create"]] {
		t.Fatalf("state ids = %v, ожидали пересчитанный id create %s", ids, remote.room.ids["m.room.create"])
	}
}

// TestJoinRejectsUnauthorizedState - state-событие, не авторизованное своими
// auth_events (пользователь не в комнате меняет тему), не попадает в
// состояние; ответ без create отвергается целиком.
func TestJoinRejectsUnauthorizedState(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)
	intruder := foreignEvent(t, remote, map[string]any{"sender": "@mallory:remote.test"})
	remote.state = append(append([]map[string]any{}, remote.room.state...), intruder)
	remote.authChain = remote.room.state

	st := &fakeStore{}

	if err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("join: %v", err)
	}

	if len(st.state) != 4 {
		t.Fatalf("неавторизованное событие в состоянии: %d событий", len(st.state))
	}

	remote2, server2 := newRemoteServer(t)
	remote2.state, remote2.authChain = remote2.room.state[1:], remote2.room.state[1:]
	st2 := &fakeStore{}

	err := join.New(client{base: server2.URL}, st2, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser)
	if !errors.Is(err, join.ErrBadState) || st2.applied {
		t.Fatalf("state без create: err=%v applied=%v", err, st2.applied)
	}
}

// TestJoinKeyFetchOncePerServer - 20 событий одного сервера - один запрос его
// ключей: раньше ключ запрашивался на каждое событие.
func TestJoinKeyFetchOncePerServer(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)

	for i := 0; i < 20; i++ {
		remote.room.addJoin(t, remote.key, fmt.Sprintf("@u%d:remote.test", i))
	}

	st := &fakeStore{}

	if err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Join(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("join: %v", err)
	}

	if len(st.state) != 24 {
		t.Fatalf("state = %d, ожидали 24", len(st.state))
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()

	if remote.keyFetches != 1 {
		t.Fatalf("запросов ключей remote.test = %d, ожидали 1", remote.keyFetches)
	}
}

// TestLeaveRemoteStoresSentPDU - исходящий leave сохраняется с исходным
// PDU - ровно тем JSON, что ушел в send_leave.
func TestLeaveRemoteStoresSentPDU(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServer(t)
	st := &fakeStore{}

	if err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
		Leave(context.Background(), remoteRoom, ourUser); err != nil {
		t.Fatalf("leave: %v", err)
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()

	if !st.applied || len(st.leave.PDU) == 0 || string(st.leave.PDU) != string(remote.leaveBody) {
		t.Fatalf("исходный PDU leave = %s, ушло в send_leave %s", st.leave.PDU, remote.leaveBody)
	}

	if st.leave.Depth != 7 {
		t.Fatalf("depth = %d, ожидали 7", st.leave.Depth)
	}
}

// routedClient - транспорт по destination: известные серверы идут на httptest,
// остальные недоступны. Запоминает порядок destination.
type routedClient struct {
	mu     sync.Mutex
	routes map[string]string
	dests  []string
}

func (c *routedClient) Send(ctx context.Context, destination, method, path string, body []byte) (respBody []byte, status int, err error) {
	c.mu.Lock()
	c.dests = append(c.dests, destination)
	base, ok := c.routes[destination]
	c.mu.Unlock()

	if !ok {
		return nil, 0, errors.New("unreachable " + destination)
	}

	return client{base: base}.Send(ctx, destination, method, path, body)
}

// TestJoinViaTriesServers - join по алиасу идет через servers из
// /query/directory по очереди (недоступный пропускается, свой сервер - нет),
// send_join - тому же серверу, что ответил на make_join.
func TestJoinViaTriesServers(t *testing.T) {
	t.Parallel()

	_, server := newRemoteServer(t)

	ours, err := signing.Generate("a_ours")
	if err != nil {
		t.Fatalf("our key: %v", err)
	}

	transport := &routedClient{routes: map[string]string{"remote.test": server.URL}}
	st := &fakeStore{}
	joiner := join.New(transport, st, &ours, "ours.test")

	// Комната фикстуры - на remote.test: он и в servers, и сервер room_id.
	err = joiner.JoinVia(context.Background(), remoteRoom, ourUser,
		[]string{"down.test", "ours.test", "remote.test", "down.test"})
	if err != nil {
		t.Fatalf("join via: %v", err)
	}

	if !st.applied {
		t.Fatal("ApplyRemoteJoin не вызван")
	}

	// Первый - недоступный, дальше только remote.test: make_join, send_join и ключи.
	got := strings.Join(transport.dests, " ")
	if transport.dests[0] != "down.test" || strings.Count(got, "remote.test") != len(transport.dests)-1 ||
		len(transport.dests) < 3 {
		t.Fatalf("destination: %q", got)
	}

	// Все кандидаты недоступны - ошибка последнего, сервер комнаты пробуется последним.
	transport = &routedClient{}
	joiner = join.New(transport, &fakeStore{}, &ours, "ours.test")

	if err := joiner.JoinVia(context.Background(), "!remote:other.test", "@bob:ours.test",
		[]string{"down.test"}); err == nil {
		t.Fatal("ожидали ошибку при недоступных серверах")
	}

	if got := strings.Join(transport.dests, " "); got != "down.test other.test" {
		t.Fatalf("destination: %q", got)
	}
}
