//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Регрессионные тесты HTTP-пути /sync: число SQL-запросов не зависит
// от числа комнат (ADR-0005), device_lists только по общим комнатам, счетчики
// непрочитанного тихих комнат не пересчитываются.

type sqlLogKey struct{}

// sqlLog - SQL, выполненные в рамках одного запроса (через контекст запроса).
type sqlLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *sqlLog) add(query string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.queries = append(l.queries, query)
}

// sqlLogTracer пишет запросы в sqlLog из контекста, если он там есть.
type sqlLogTracer struct{}

func (sqlLogTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if log, ok := ctx.Value(sqlLogKey{}).(*sqlLog); ok {
		log.add(data.SQL)
	}

	return ctx
}

func (sqlLogTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type perfEnv struct {
	t   *testing.T
	srv *Server
}

// call выполняет запрос и возвращает ответ и выполненные им SQL.
func (e perfEnv) call(token, method, path, body string) (*httptest.ResponseRecorder, []string) {
	e.t.Helper()

	log := &sqlLog{}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), sqlLogKey{}, log))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}

	return rec, log.queries
}

func (e perfEnv) sync(token, since string) (syncResponse, []string) {
	e.t.Helper()

	path := "/_matrix/client/v3/sync?timeout=0"
	if since != "" {
		path += "&since=" + url.QueryEscape(since)
	}

	rec, queries := e.call(token, http.MethodGet, path, "")

	var resp syncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("decode sync: %v", err)
	}

	return resp, queries
}

func (e perfEnv) room(creator string, members ...string) string {
	e.t.Helper()

	rec, _ := e.call(creator, http.MethodPost, "/_matrix/client/v3/createRoom", `{"preset":"public_chat"}`)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	for _, m := range members {
		e.call(m, http.MethodPost, "/_matrix/client/v3/rooms/"+created.RoomID+"/join", `{}`)
	}

	return created.RoomID
}

func (e perfEnv) send(token, roomID, txn, body string) string {
	e.t.Helper()

	rec, _ := e.call(token, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/"+txn,
		`{"msgtype":"m.text","body":"`+body+`"}`)

	var sent struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sent)

	return sent.EventID
}

// TestSyncHTTPQueryCountIndependentOfRooms - число SQL-запросов на /sync
// (initial, incremental с событиями во всех комнатах, тихий incremental) не
// растет с числом комнат (ADR-0005). В каждой комнате есть удаленное
// событие: до исправления redacted_because читался запросом на комнату.
func TestSyncHTTPQueryCountIndependentOfRooms(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServerWithTracer(t, sqlLogTracer{})}

	alice := registerPusherProbe(t, env.srv, "qcalice", "qc-alice-pass")
	bob := registerPusherProbe(t, env.srv, "qcbob", "qc-bob-pass")

	var rooms []string

	addRooms := func(n int) {
		for i := 0; i < n; i++ {
			roomID := env.room(alice, bob)
			rooms = append(rooms, roomID)

			for k := 0; k < 3; k++ {
				env.send(alice, roomID, fmt.Sprintf("m%d-%d", len(rooms), k), "x")
			}

			removed := env.send(alice, roomID, fmt.Sprintf("r%d", len(rooms)), "удалить")
			env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/redact/"+url.PathEscape(removed)+
				fmt.Sprintf("/rd%d", len(rooms)), `{}`)
		}
	}

	type counts struct{ initial, active, quiet int }

	measure := func() counts {
		initial, initialSQL := env.sync(bob, "")
		if len(initial.Rooms.Join) != len(rooms) {
			t.Fatalf("initial: %d комнат, ожидали %d", len(initial.Rooms.Join), len(rooms))
		}

		for _, roomID := range rooms {
			env.send(alice, roomID, fmt.Sprintf("a%d-%s", len(rooms), roomID), "новое")
		}

		active, activeSQL := env.sync(bob, initial.NextBatch)
		for _, roomID := range rooms {
			if len(active.Rooms.Join[roomID].Timeline.Events) != 1 {
				t.Fatalf("incremental: в %s %d событий, ожидали 1", roomID, len(active.Rooms.Join[roomID].Timeline.Events))
			}
		}

		_, quietSQL := env.sync(bob, active.NextBatch)

		// Тихий incremental не пересчитывает счетчики непрочитанного.
		for _, query := range quietSQL {
			if strings.Contains(query, "AS r(room_id, low)") {
				t.Fatalf("тихий /sync пересчитал unread_notifications:\n%s", query)
			}
		}

		return counts{initial: len(initialSQL), active: len(activeSQL), quiet: len(quietSQL)}
	}

	addRooms(2)
	small := measure()

	addRooms(8)
	large := measure()

	if small != large {
		t.Fatalf("число SQL на /sync зависит от числа комнат: 2 комнаты %+v, 10 комнат %+v", small, large)
	}

	t.Logf("SQL на /sync: initial %d, incremental %d, тихий %d", small.initial, small.active, small.quiet)
}

// TestSyncDeviceListsSharedRoomsOnly - device_lists.changed и /keys/changes
// показывают изменения ключей только тем, с кем есть общая комната, и не
// повторяются в следующем /sync. До исправления окно бралось от
// закладки to-device (0 при пустой очереди): каждый /sync отдавал всех, у кого
// когда-либо менялись ключи, и /keys/changes не фильтровался вовсе.
func TestSyncDeviceListsSharedRoomsOnly(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "dlalice", "dl-alice-pass")
	bob := registerPusherProbe(t, env.srv, "dlbob", "dl-bob-pass")
	carol := registerPusherProbe(t, env.srv, "dlcarol", "dl-carol-pass")

	env.room(alice, bob)
	env.room(carol)

	bobStart, _ := env.sync(bob, "")
	carolStart, _ := env.sync(carol, "")

	env.call(alice, http.MethodPost, "/_matrix/client/v3/keys/upload",
		`{"device_keys":{"user_id":"@dlalice:test.local","device_id":"DEV","algorithms":["m.olm.v1.curve25519-aes-sha2"],"keys":{"curve25519:DEV":"k"}}}`)

	const aliceID = "@dlalice:test.local"

	bobNext, _ := env.sync(bob, bobStart.NextBatch)
	if !slices.Contains(bobNext.DeviceLists.Changed, aliceID) {
		t.Fatalf("bob не увидел изменение ключей alice: %v", bobNext.DeviceLists.Changed)
	}

	bobAgain, _ := env.sync(bob, bobNext.NextBatch)
	if len(bobAgain.DeviceLists.Changed) != 0 {
		t.Fatalf("изменения ключей повторились в следующем /sync: %v", bobAgain.DeviceLists.Changed)
	}

	carolNext, _ := env.sync(carol, carolStart.NextBatch)
	if slices.Contains(carolNext.DeviceLists.Changed, aliceID) {
		t.Fatalf("carol без общей комнаты видит ключи alice: %v", carolNext.DeviceLists.Changed)
	}

	keysChanges := func(token, from string) []string {
		rec, _ := env.call(token, http.MethodGet, "/_matrix/client/v3/keys/changes?from="+url.QueryEscape(from), "")

		var resp keysChangesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode keys/changes: %v", err)
		}

		return resp.Changed
	}

	if changed := keysChanges(bob, bobStart.NextBatch); !slices.Contains(changed, aliceID) {
		t.Fatalf("/keys/changes у bob без alice: %v", changed)
	}

	if changed := keysChanges(carol, carolStart.NextBatch); slices.Contains(changed, aliceID) {
		t.Fatalf("/keys/changes у carol содержит alice без общей комнаты: %v", changed)
	}
}

// TestSyncUnreadCountsFollowReceiptsAndRules - счетчики хранятся:
// тихая комната в incremental не отдается вовсе, receipt возвращает ее с новым
// счетчиком, правила действуют на события после смены (как Synapse).
func TestSyncUnreadCountsFollowReceiptsAndRules(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "ucalice", "uc-alice-pass")
	bob := registerPusherProbe(t, env.srv, "ucbob", "uc-bob-pass")
	carol := registerPusherProbe(t, env.srv, "uccarol", "uc-carol-pass")
	roomID := env.room(alice, bob, carol)

	env.send(alice, roomID, "u1", "один")
	second := env.send(alice, roomID, "u2", "два")
	env.send(alice, roomID, "u3", "три")

	unread := func(resp syncResponse) int {
		room, ok := resp.Rooms.Join[roomID]
		if !ok || room.UnreadNotifications == nil {
			t.Fatalf("нет unread_notifications: %+v", resp.Rooms.Join)
		}

		return room.UnreadNotifications.Notification
	}

	start, _ := env.sync(bob, "")
	if got := unread(start); got != 3 {
		t.Fatalf("initial: %d, ожидали 3", got)
	}

	quiet, _ := env.sync(bob, start.NextBatch)
	if _, ok := quiet.Rooms.Join[roomID]; ok {
		t.Fatalf("тихая комната в incremental: %+v", quiet.Rooms.Join)
	}

	env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read/"+url.PathEscape(second), `{}`)

	afterReceipt, _ := env.sync(bob, quiet.NextBatch)
	if got := unread(afterReceipt); got != 1 {
		t.Fatalf("после receipt: %d, ожидали 1", got)
	}

	// Master-правило выключает уведомления о следующих событиях.
	env.call(bob, http.MethodPut, "/_matrix/client/v3/pushrules/global/override/.m.rule.master/enabled", `{"enabled":true}`)
	env.send(alice, roomID, "u4", "четыре")

	afterRules, _ := env.sync(bob, afterReceipt.NextBatch)
	if got := unread(afterRules); got != 1 {
		t.Fatalf("после смены push-правил: %d, ожидали 1 (новое событие не уведомляет)", got)
	}
}
