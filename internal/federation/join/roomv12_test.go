package join_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/join"
)

// TestJoinRemoteV12 - вход в комнату v12 сервера Synapse. room_id -
// хеш create без домена: make_join идет на переданный сервер с ver=12, create
// в ответе send_join - без room_id, auth_events событий - без create; наш join
// подписан по v12 и без create в auth_events. Раньше v12 отвергалась до
// send_join (ErrUnsupportedRoomVersion).
func TestJoinRemoteV12(t *testing.T) {
	t.Parallel()

	remote, server := newRemoteServerVersion(t, 12)
	roomID := remote.room.roomID
	createID := remote.room.ids["m.room.create"]

	if roomID != "!"+strings.TrimPrefix(createID, "$") {
		t.Fatalf("фикстура v12: room_id %s, create %s", roomID, createID)
	}

	transport := &routedClient{routes: map[string]string{"remote.test": server.URL}}
	st := &fakeStore{}

	if err := join.New(transport, st, ourKey(t), "ours.test").
		JoinVia(context.Background(), roomID, ourUser, []string{"remote.test"}); err != nil {
		t.Fatalf("join v12: %v", err)
	}

	if !st.applied || st.roomVersion != "12" || len(st.state) != 4 || !st.stateIDs()[createID] {
		t.Fatalf("ApplyRemoteJoin: applied=%v version=%q state=%v", st.applied, st.roomVersion, st.stateIDs())
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()

	if !strings.Contains(remote.requests[0], "ver=12") {
		t.Fatalf("make_join без ver=12: %s", remote.requests[0])
	}

	var sent map[string]any
	if err := json.Unmarshal(remote.sendJoinBody, &sent); err != nil {
		t.Fatalf("send_join body: %v", err)
	}

	if sent["room_id"] != roomID || slices.Contains(stringsOf(sent["auth_events"]), createID) {
		t.Fatalf("join v12: %v", sent)
	}
}

// TestJoinRemoteV12Rejects - ответ send_join комнаты v12 не по
// формату отвергается до записи: create с room_id, create не той комнаты,
// create в auth_events нашего join.
func TestJoinRemoteV12Rejects(t *testing.T) {
	t.Parallel()

	cases := map[string]func(t *testing.T, remote *remoteServer) string{
		"create с room_id": func(t *testing.T, remote *remoteServer) string {
			_, create := roomEvent(t, remote.key, "remote.test", 12, map[string]any{
				"room_id": remote.room.roomID, "type": "m.room.create", "state_key": "", "sender": "@alice:remote.test",
				"content": map[string]any{"room_version": "12"}, "depth": 1, "origin_server_ts": 1_700_000_000_001,
				"prev_events": []any{}, "auth_events": []any{},
			})
			state := append([]map[string]any{create}, remote.room.state[1:]...)
			remote.state, remote.authChain = state, state

			return remote.room.roomID
		},
		"room_id не хеш create": func(t *testing.T, remote *remoteServer) string {
			other := "!" + strings.Repeat("A", 43)
			remote.template = remote.room.template()
			remote.template["room_id"] = other

			return other
		},
		"create в auth_events join": func(t *testing.T, remote *remoteServer) string {
			remote.template = remote.room.template()
			remote.template["auth_events"] = append([]any{remote.room.ids["m.room.create"]},
				remote.template["auth_events"].([]any)...)

			return remote.room.roomID
		},
	}

	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			remote, server := newRemoteServerVersion(t, 12)
			roomID := prepare(t, remote)
			st := &fakeStore{}

			err := join.New(client{base: server.URL}, st, ourKey(t), "ours.test").
				JoinVia(context.Background(), roomID, ourUser, []string{"remote.test"})
			if !errors.Is(err, join.ErrBadState) {
				t.Fatalf("ожидали ErrBadState, получили %v", err)
			}

			if st.applied {
				t.Fatal("состояние применено")
			}
		})
	}
}

// TestLeaveViaV12 - выход из комнаты v12 (room_id без домена) идет
// через переданные серверы по очереди; без серверов - ошибка без запросов.
func TestLeaveViaV12(t *testing.T) {
	t.Parallel()

	_, server := newRemoteServer(t)
	roomID := "!" + strings.Repeat("B", 43)

	transport := &routedClient{routes: map[string]string{"remote.test": server.URL}}
	st := &fakeStore{}
	joiner := join.New(transport, st, ourKey(t), "ours.test")

	if err := joiner.Leave(context.Background(), roomID, ourUser); err == nil || len(transport.dests) != 0 {
		t.Fatalf("leave без серверов: %v, запросы %v", err, transport.dests)
	}

	if err := joiner.LeaveVia(context.Background(), roomID, ourUser, []string{"down.test", "remote.test"}); err != nil {
		t.Fatalf("leave via: %v", err)
	}

	if !st.applied || transport.dests[0] != "down.test" || transport.dests[1] != "remote.test" {
		t.Fatalf("leave: applied=%v destination=%v", st.applied, transport.dests)
	}
}

// stringsOf - []string из []any.
func stringsOf(value any) []string {
	list, _ := value.([]any)
	out := make([]string, 0, len(list))

	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}

	return out
}
