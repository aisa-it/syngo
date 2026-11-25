//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/federation/eventjson"
	"github.com/aisa-it/syngo/internal/federation/signing"
)

// stateSetsEnv - комната seedRoom + join_rules public (до миграции 0044:
// без состояния на событие и extremities) и подпись событий alice ключом test.local.
func stateSetsEnv(t *testing.T, roomID string) *fedEnv {
	t.Helper()

	env := newFedEnv(t)
	env.seedRoom(t, roomID)

	ctx := context.Background()

	for _, sql := range []string{
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin, stream_ordering)
		 VALUES ('$jr:test.local',$1,'m.room.join_rules','@alice:test.local','','{"join_rule":"public"}',4,4,'test.local',4)`,
		`INSERT INTO room_state (room_id, type, state_key, event_id) VALUES
		 ($1,'m.room.join_rules','','$jr:test.local')`,
	} {
		if _, err := env.server.db.Pool().Exec(ctx, sql, roomID); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	env.server.keyLookup = func(origin, keyID string) (string, int64, error) {
		if origin == "remote.test" && keyID == env.remoteKey.KeyID {
			return env.remoteKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		if origin == "test.local" && keyID == env.server.federationKey.KeyID {
			return env.server.federationKey.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
		}

		return "", 0, signing.ErrInvalidKey
	}

	return env
}

// memberPDU - подписанное m.room.member без event_id (room v3+): bob подписывает
// remote.test, alice - test.local; возвращает событие и вычисленный id.
func (e *fedEnv) memberPDU(
	t *testing.T, roomID, sender, target, membership string, prev, auth []string, depth int64,
) (map[string]any, string) {
	t.Helper()

	object := map[string]any{
		"room_id": roomID, "type": "m.room.member", "sender": sender, "state_key": target,
		"content": map[string]any{"membership": membership}, "depth": depth, "origin_server_ts": 1_700_000_000_000 + depth,
		"prev_events": prev, "auth_events": auth,
	}

	key, origin := e.remoteKey, "remote.test"
	if sender == "@alice:test.local" {
		key, origin = *e.server.federationKey, "test.local"
	}

	signed, err := eventjson.Sign(key, origin, withContentHash(t, object))
	if err != nil {
		t.Fatalf("sign %s/%s: %v", target, membership, err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	id, err := core.EventID(generic, core.DefaultRoomVersion)
	if err != nil {
		t.Fatalf("event id: %v", err)
	}

	return generic, id
}

func (e *fedEnv) sendPDUs(t *testing.T, txnID string, pdus ...map[string]any) {
	t.Helper()

	list := make([]any, 0, len(pdus))
	for _, pdu := range pdus {
		list = append(list, pdu)
	}

	rec := e.request(t, http.MethodPut, "/_matrix/federation/v1/send/"+txnID, map[string]any{
		"origin": "remote.test", "pdus": list,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("send %s: %d %s", txnID, rec.Code, rec.Body.String())
	}
}

func (e *fedEnv) queryStrings(t *testing.T, sql string, args ...any) []string {
	t.Helper()

	rows, err := e.server.db.Pool().Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}

		out = append(out, s)
	}

	sort.Strings(out)

	return out
}

// TestFederationStateSetsBanVsFork - состояние на событие и
// forward extremities. Bob входит, alice его банит; сервер bob присылает leave и
// новый join из ветки до бана. Каждое проходит проверку по своему состоянию (бана
// в ветке нет), но не по текущему состоянию комнаты (bob забанен): soft-fail, как
// в Synapse - события сохранены, получают состояние после себя, но в extremities,
// текущее состояние и таймлайн не попадают (раньше ветка становилась второй
// extremity, и тест это закреплял). Join поверх бана отклоняется по состоянию до
// него.
func TestFederationStateSetsBanVsFork(t *testing.T) {
	const roomID = "!sets:test.local"

	env := stateSetsEnv(t, roomID)
	base := []string{"$create:test.local", "$pl:test.local"}

	bobJoin, bobJoinID := env.memberPDU(t, roomID, "@bob:remote.test", "@bob:remote.test", "join",
		[]string{"$jr:test.local"}, append(base, "$jr:test.local", "$invite:test.local"), 6)
	env.sendPDUs(t, "sets-1", bobJoin)

	// Ban alice пересылает ее сервер: чужое не-membership событие из /send
	// отбрасывается (Synapse).
	ban, banID := env.memberPDU(t, roomID, "@alice:test.local", "@bob:remote.test", "ban",
		[]string{bobJoinID}, append(base, "$alice-join:test.local", bobJoinID), 7)
	if rec := env.requestAs(t, "test.local", *env.server.federationKey, http.MethodPut,
		"/_matrix/federation/v1/send/sets-2", map[string]any{"origin": "test.local", "pdus": []any{ban}}); rec.Code != http.StatusOK {
		t.Fatalf("ban: %d %s", rec.Code, rec.Body.String())
	}

	leave, leaveID := env.memberPDU(t, roomID, "@bob:remote.test", "@bob:remote.test", "leave",
		[]string{bobJoinID}, append(base, bobJoinID), 7)
	rejoin, rejoinID := env.memberPDU(t, roomID, "@bob:remote.test", "@bob:remote.test", "join",
		[]string{leaveID}, append(base, "$jr:test.local", leaveID), 8)
	env.sendPDUs(t, "sets-3", leave)
	env.sendPDUs(t, "sets-3b", rejoin)

	extremities := env.queryStrings(t, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID)
	if !slices.Equal(extremities, []string{banID}) {
		t.Fatalf("extremities %v, ожидали [ban]", extremities)
	}

	softFailed := env.queryStrings(t, `SELECT event_id FROM events WHERE room_id = $1 AND soft_failed AND outlier`, roomID)
	want := []string{leaveID, rejoinID}
	slices.Sort(want)

	if !slices.Equal(softFailed, want) {
		t.Fatalf("soft-failed %v, ожидали leave и rejoin", softFailed)
	}

	withState := env.queryStrings(t, `SELECT event_id FROM event_state_groups WHERE event_id = ANY($1) AND NOT rejected`,
		[]string{leaveID, rejoinID})
	if len(withState) != 2 {
		t.Fatalf("состояние после soft-failed событий: %v", withState)
	}

	stateBob := env.queryStrings(t, `
		SELECT event_id FROM room_state WHERE room_id = $1 AND type = 'm.room.member' AND state_key = '@bob:remote.test'`, roomID)
	membership := env.queryStrings(t, `
		SELECT membership FROM room_memberships WHERE room_id = $1 AND user_id = '@bob:remote.test'`, roomID)

	if len(stateBob) != 1 || stateBob[0] != banID || len(membership) != 1 || membership[0] != "ban" {
		t.Fatalf("room_state bob %v, room_memberships %v; ожидали ban", stateBob, membership)
	}

	// Join поверх бана: состояние до него содержит ban - отклонен.
	again, againID := env.memberPDU(t, roomID, "@bob:remote.test", "@bob:remote.test", "join",
		[]string{banID}, append(base, "$jr:test.local", leaveID), 8)
	env.sendPDUs(t, "sets-4", again)

	rejected := env.queryStrings(t, `SELECT event_id FROM event_state_groups WHERE room_id = $1 AND rejected`, roomID)
	if len(rejected) != 1 || rejected[0] != againID {
		t.Fatalf("отклонены %v, ожидали [again]", rejected)
	}

	if got := env.queryStrings(t, `SELECT event_id FROM room_forward_extremities WHERE room_id = $1`, roomID); !slices.Equal(got, []string{banID}) {
		t.Fatalf("extremities после отклоненного %v, ожидали [ban]", got)
	}

	// /state_ids на again - состояние ДО события: после его prev (ban), а не
	// «последнее по depth» (leave той же глубины из другой ветки).
	before, found, err := env.server.db.StateIDsBefore(context.Background(), roomID, againID)
	if err != nil || !found {
		t.Fatalf("StateIDsBefore: found=%v err=%v", found, err)
	}

	if !slices.Contains(before, banID) || slices.Contains(before, leaveID) || slices.Contains(before, againID) {
		t.Fatalf("состояние до again %v, ожидали ban без leave и самого события", before)
	}

	// Повтор транзакции и тех же событий ничего не меняет.
	env.sendPDUs(t, "sets-5", leave, rejoin, again)

	if got := env.queryStrings(t, `SELECT event_id FROM room_state WHERE room_id = $1 AND state_key = '@bob:remote.test'`, roomID); len(got) != 1 || got[0] != banID {
		t.Fatalf("после повтора room_state bob %v", got)
	}
}

// TestLocalEventsStateGroups - локальные события записывают состояние
// после себя и forward extremities; состояние последнего события равно room_state,
// прямая запись room_state снимает группу комнаты, и следующее событие пишет снимок.
func TestLocalEventsStateGroups(t *testing.T) {
	server := newIntegrationServer(t)
	tok := registerHTTP(t, server, "sg", "DEV")
	roomID := createTestRoom(t, server, tok)
	ctx := context.Background()

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/state/m.room.topic/",
		`{"topic":"one"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("topic: %d %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/m1",
		`{"msgtype":"m.text","body":"hi"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("message: %d %s", rec.Code, rec.Body.String())
	}

	var missing int
	if err := server.db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM events e
		WHERE e.room_id = $1 AND NOT EXISTS (SELECT 1 FROM event_state_groups g WHERE g.event_id = e.event_id)`,
		roomID).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("событий без состояния: %d (%v)", missing, err)
	}

	var latest, extremity string
	if err := server.db.Pool().QueryRow(ctx, `
		SELECT event_id FROM events WHERE room_id = $1 ORDER BY stream_ordering DESC LIMIT 1`, roomID).Scan(&latest); err != nil {
		t.Fatalf("latest: %v", err)
	}

	if err := server.db.Pool().QueryRow(ctx, `
		SELECT string_agg(event_id, ',') FROM room_forward_extremities WHERE room_id = $1`, roomID).Scan(&extremity); err != nil || extremity != latest {
		t.Fatalf("extremities %q, ожидали последнее событие %s (%v)", extremity, latest, err)
	}

	assertStateMatchesRoomState(t, server, roomID, latest)

	// Прямая запись room_state (импорт, пересчет) снимает группу комнаты.
	if _, err := server.db.Pool().Exec(ctx, `UPDATE room_state SET updated_at = now() WHERE room_id = $1`, roomID); err != nil {
		t.Fatalf("update room_state: %v", err)
	}

	var groups int
	if err := server.db.Pool().QueryRow(ctx, `SELECT count(*) FROM room_state_groups WHERE room_id = $1`, roomID).Scan(&groups); err != nil || groups != 0 {
		t.Fatalf("группа комнаты после прямой записи: %d (%v)", groups, err)
	}

	if rec := do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/state/m.room.topic/",
		`{"topic":"two"}`, tok); rec.Code != http.StatusOK {
		t.Fatalf("topic two: %d %s", rec.Code, rec.Body.String())
	}

	if err := server.db.Pool().QueryRow(ctx, `
		SELECT event_id FROM events WHERE room_id = $1 ORDER BY stream_ordering DESC LIMIT 1`, roomID).Scan(&latest); err != nil {
		t.Fatalf("latest: %v", err)
	}

	assertStateMatchesRoomState(t, server, roomID, latest)
}

// assertStateMatchesRoomState - состояние после события (по цепочке групп) равно room_state.
func assertStateMatchesRoomState(t *testing.T, server *Server, roomID, eventID string) {
	t.Helper()

	var diff int
	if err := server.db.Pool().QueryRow(context.Background(), `
		WITH RECURSIVE chain (id, prev, lvl) AS (
			SELECT g.id, g.prev_group, 0 FROM state_groups g
			JOIN event_state_groups e ON e.state_group = g.id WHERE e.event_id = $2
			UNION ALL
			SELECT g.id, g.prev_group, c.lvl + 1 FROM state_groups g JOIN chain c ON g.id = c.prev
		), st AS (
			SELECT DISTINCT ON (e.type, e.state_key) e.type, e.state_key, e.event_id
			FROM chain c JOIN state_group_entries e ON e.state_group = c.id
			ORDER BY e.type, e.state_key, c.lvl
		)
		SELECT count(*) FROM (
			(SELECT type, state_key, event_id FROM st EXCEPT SELECT type, state_key, event_id FROM room_state WHERE room_id = $1)
			UNION ALL
			(SELECT type, state_key, event_id FROM room_state WHERE room_id = $1 EXCEPT SELECT type, state_key, event_id FROM st)
		) d`, roomID, eventID).Scan(&diff); err != nil || diff != 0 {
		t.Fatalf("состояние после %s расходится с room_state на %d ключей (%v)", eventID, diff, err)
	}
}
