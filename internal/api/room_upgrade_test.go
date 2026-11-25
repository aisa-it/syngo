package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

// TestRoomUpgrade — новая комната с перенесённым состоянием, tombstone
// в старой, участники приглашены.
func TestRoomUpgrade(t *testing.T) {
	t.Parallel()

	server := newRoomsServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"name":"Old","topic":"до обновления"}`)

	// bob вступает в старую комнату.
	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/invite", roomID),
		fmt.Sprintf(`{"user_id":%q}`, bob.UserID), alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("invite: %d", rec.Code)
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d", rec.Code)
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/upgrade", roomID), `{"new_version":"11"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("upgrade: status %d, body %s", rec.Code, rec.Body.String())
	}

	var resp upgradeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if resp.ReplacementRoom == "" || resp.ReplacementRoom == roomID {
		t.Fatalf("replacement_room = %q", resp.ReplacementRoom)
	}

	// Новая комната: alice уже внутри и видит перенесённое имя.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.name", resp.ReplacementRoom), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("state name новой комнаты: %d %s", rec.Code, rec.Body.String())
	}

	if body := rec.Body.String(); !contains(body, "Old") {
		t.Fatalf("имя не перенесено: %s", body)
	}

	// Tombstone в старой комнате указывает на новую.
	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/m.room.tombstone", roomID), "", alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("tombstone: %d %s", rec.Code, rec.Body.String())
	}

	if body := rec.Body.String(); !contains(body, resp.ReplacementRoom) {
		t.Fatalf("tombstone без replacement_room: %s", body)
	}

	// bob получил приглашение: join в новую комнату проходит только с invite.
	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", resp.ReplacementRoom), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob не приглашён в новую комнату: join %d %s", rec.Code, rec.Body.String())
	}
}

// upgradeState читает content state-события комнаты от имени владельца токена.
func upgradeState(t *testing.T, server *Server, token, roomID, eventType, stateKey string) (code int, content map[string]any) {
	t.Helper()

	rec := do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/state/%s/%s", roomID, eventType, stateKey), "", token)

	content = map[string]any{}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &content); err != nil {
			t.Fatalf("unmarshal %s: %v", eventType, err)
		}
	}

	return rec.Code, content
}

// doUpgrade вызывает /upgrade и возвращает код и replacement_room.
func doUpgrade(t *testing.T, server *Server, token, roomID, body string) (code int, replacement, raw string) {
	t.Helper()

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/upgrade", roomID), body, token)

	var resp upgradeResponse

	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal upgrade: %v", err)
		}
	}

	return rec.Code, resp.ReplacementRoom, rec.Body.String()
}

// newUpgradeServer - сервер комнат с доступом к in-memory хранилищу.
func newUpgradeServer(t *testing.T) (*Server, *coretest.MemRoomStore) {
	t.Helper()

	server := newAuthServer(t, core.AuthOptions{})
	store := coretest.NewMemRoomStore()
	server.rooms = core.NewRoomService(store, "test.local")

	return server, store
}

// TestRoomUpgradeForbiddenCreatesNothing - без права на m.room.tombstone
// upgrade отвечает 403 и новую комнату не создает (раньше комната создавалась до
// проверки прав и оставалась сиротой).
func TestRoomUpgradeForbiddenCreatesNothing(t *testing.T) {
	t.Parallel()

	server, store := newUpgradeServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{"preset":"public_chat"}`)

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	before := len(store.Rooms())

	code, _, body := doUpgrade(t, server, bob.AccessToken, roomID, `{"new_version":"11"}`)
	if code != http.StatusForbidden {
		t.Fatalf("upgrade без прав: %d %s, want 403", code, body)
	}

	if after := len(store.Rooms()); after != before {
		t.Fatalf("комнат было %d, стало %d: upgrade без прав создал комнату", before, after)
	}

	if code, _ := upgradeState(t, server, alice.AccessToken, roomID, "m.room.tombstone", ""); code != http.StatusNotFound {
		t.Fatalf("tombstone после отказа: %d", code)
	}
}

// TestRoomUpgradeTransfersState - predecessor и версия в m.room.create
// (v11 без creator), перенос состояния, power_levels с users, банов, алиасов,
// canonical_alias и публикации; ограничение уровней в старой комнате.
func TestRoomUpgradeTransfersState(t *testing.T) {
	t.Parallel()

	server, store := newUpgradeServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	bob := registerUser(t, server, "bob", "correct horse battery")

	roomID := createRoomWith(t, server, alice.AccessToken, `{
		"preset":"public_chat","visibility":"public","name":"Old","topic":"t",
		"room_alias_name":"old",
		"power_level_content_override":{"users":{"@bob:test.local":50}},
		"initial_state":[
			{"type":"m.room.encryption","content":{"algorithm":"m.megolm.v1.aes-sha2"}},
			{"type":"m.room.avatar","content":{"url":"mxc://test.local/avatar"}},
			{"type":"m.room.server_acl","content":{"allow":["*"],"deny":["evil.example"]}},
			{"type":"m.room.related_groups","content":{"groups":["+g:test.local"]}},
			{"type":"m.room.history_visibility","content":{"history_visibility":"joined"}},
			{"type":"m.room.guest_access","content":{"guest_access":"can_join"}}
		]}`)

	rec := do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/join", roomID), "", bob.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/ban", roomID), `{"user_id":"@mallory:test.local","reason":"spam"}`, alice.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodGet,
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=1", roomID), "", alice.AccessToken)

	var page struct {
		Chunk []struct {
			EventID string `json:"event_id"`
		} `json:"chunk"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Chunk) != 1 {
		t.Fatalf("messages: %d %s", rec.Code, rec.Body.String())
	}

	lastEventID := page.Chunk[0].EventID

	code, newRoomID, body := doUpgrade(t, server, alice.AccessToken, roomID, `{"new_version":"11"}`)
	if code != http.StatusOK || newRoomID == "" || newRoomID == roomID {
		t.Fatalf("upgrade: %d %s", code, body)
	}

	_, create := upgradeState(t, server, alice.AccessToken, newRoomID, "m.room.create", "")
	if create["room_version"] != "11" {
		t.Fatalf("room_version = %v", create["room_version"])
	}

	if _, has := create["creator"]; has {
		t.Fatalf("v11 create с creator: %v", create)
	}

	predecessor, _ := create["predecessor"].(map[string]any)
	if predecessor["room_id"] != roomID || predecessor["event_id"] != lastEventID {
		t.Fatalf("predecessor = %v, want room_id %s event_id %s", predecessor, roomID, lastEventID)
	}

	for _, want := range []struct{ eventType, field, value string }{
		{"m.room.name", "name", "Old"},
		{"m.room.topic", "topic", "t"},
		{"m.room.join_rules", "join_rule", "public"},
		{"m.room.history_visibility", "history_visibility", "joined"},
		{"m.room.guest_access", "guest_access", "can_join"},
		{"m.room.avatar", "url", "mxc://test.local/avatar"},
		{"m.room.encryption", "algorithm", "m.megolm.v1.aes-sha2"},
		{"m.room.canonical_alias", "alias", "#old:test.local"},
	} {
		code, content := upgradeState(t, server, alice.AccessToken, newRoomID, want.eventType, "")
		if code != http.StatusOK || content[want.field] != want.value {
			t.Errorf("%s в новой комнате: %d %v, want %s=%s", want.eventType, code, content, want.field, want.value)
		}
	}

	for _, eventType := range []string{"m.room.server_acl", "m.room.related_groups"} {
		if code, _ := upgradeState(t, server, alice.AccessToken, newRoomID, eventType, ""); code != http.StatusOK {
			t.Errorf("%s не перенесен: %d", eventType, code)
		}
	}

	_, levels := upgradeState(t, server, alice.AccessToken, newRoomID, "m.room.power_levels", "")
	users, _ := levels["users"].(map[string]any)
	if users["@bob:test.local"] != float64(50) || users["@alice:test.local"] != float64(100) {
		t.Errorf("users в power_levels новой комнаты: %v", users)
	}

	_, member := upgradeState(t, server, alice.AccessToken, newRoomID, "m.room.member", "@mallory:test.local")
	if member["membership"] != "ban" || member["reason"] != "spam" {
		t.Errorf("бан не перенесен: %v", member)
	}

	// Алиас ведет в новую комнату, старая комната его больше не объявляет.
	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/directory/room/%23old:test.local", "", "")
	if !contains(rec.Body.String(), newRoomID) {
		t.Errorf("алиас не перенесен: %d %s", rec.Code, rec.Body.String())
	}

	if _, oldAlias := upgradeState(t, server, alice.AccessToken, roomID, "m.room.canonical_alias", ""); oldAlias["alias"] != nil {
		t.Errorf("canonical_alias старой комнаты: %v", oldAlias)
	}

	rooms := store.Rooms()
	if rooms[roomID].IsPublic || !rooms[newRoomID].IsPublic {
		t.Errorf("публикация не перенесена: old=%v new=%v", rooms[roomID].IsPublic, rooms[newRoomID].IsPublic)
	}

	// Старая комната: обычные участники больше не пишут и не приглашают.
	_, oldLevels := upgradeState(t, server, alice.AccessToken, roomID, "m.room.power_levels", "")
	if oldLevels["events_default"] != float64(50) || oldLevels["invite"] != float64(50) {
		t.Errorf("уровни старой комнаты не ограничены: %v", oldLevels)
	}

	_, tombstone := upgradeState(t, server, alice.AccessToken, roomID, "m.room.tombstone", "")
	if tombstone["replacement_room"] != newRoomID {
		t.Errorf("tombstone: %v", tombstone)
	}
}

// TestRoomUpgradeRepeat - повторный upgrade той же комнаты (тот же или
// другой клиент) не теряет tombstone и не плодит комнаты - отдает существующую
// замену; upgrade разных комнат подряд пишет tombstone в каждой (раньше txn
// "upgrade" глотал второй tombstone, TestReviewSecondUpgradeNoTombstone).
func TestRoomUpgradeRepeat(t *testing.T) {
	t.Parallel()

	server, store := newUpgradeServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")

	first := createRoomWith(t, server, alice.AccessToken, `{}`)
	second := createRoomWith(t, server, alice.AccessToken, `{}`)

	for _, roomID := range []string{first, second} {
		code, replacement, body := doUpgrade(t, server, alice.AccessToken, roomID, `{"new_version":"11"}`)
		if code != http.StatusOK {
			t.Fatalf("upgrade %s: %d %s", roomID, code, body)
		}

		if _, tombstone := upgradeState(t, server, alice.AccessToken, roomID, "m.room.tombstone", ""); tombstone["replacement_room"] != replacement {
			t.Fatalf("tombstone %s: %v, want %s", roomID, tombstone, replacement)
		}
	}

	_, tombstone := upgradeState(t, server, alice.AccessToken, first, "m.room.tombstone", "")
	before := len(store.Rooms())

	code, replacement, body := doUpgrade(t, server, alice.AccessToken, first, `{"new_version":"10"}`)
	if code != http.StatusOK || replacement != tombstone["replacement_room"] {
		t.Fatalf("повторный upgrade: %d %s, want прежнюю замену %v", code, body, tombstone["replacement_room"])
	}

	if after := len(store.Rooms()); after != before {
		t.Fatalf("повторный upgrade создал комнату: %d -> %d", before, after)
	}
}

// TestRoomUpgradeVersionValidation - new_version обязателен (400
// M_MISSING_PARAM), неподдерживаемая версия - 400 M_UNSUPPORTED_ROOM_VERSION.
func TestRoomUpgradeVersionValidation(t *testing.T) {
	t.Parallel()

	server, store := newUpgradeServer(t)
	alice := registerUser(t, server, "alice", "correct horse battery")
	roomID := createRoomWith(t, server, alice.AccessToken, `{}`)
	before := len(store.Rooms())

	for _, tc := range []struct{ body, errcode string }{
		{`{}`, ErrCodeMissingParam},
		{`{"new_version":"13"}`, ErrCodeUnsupportedRoomVersion},
		{`{"new_version":"9"}`, ErrCodeUnsupportedRoomVersion},
		{`{"new_version":"abc"}`, ErrCodeUnsupportedRoomVersion},
	} {
		code, _, body := doUpgrade(t, server, alice.AccessToken, roomID, tc.body)
		if code != http.StatusBadRequest || !contains(body, tc.errcode) {
			t.Errorf("upgrade %s: %d %s, want 400 %s", tc.body, code, body, tc.errcode)
		}
	}

	if after := len(store.Rooms()); after != before {
		t.Fatalf("невалидный upgrade создал комнату: %d -> %d", before, after)
	}
}
