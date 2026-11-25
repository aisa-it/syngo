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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты incremental /sync: отдаются только комнаты, где что-то изменилось,
// и ни одно изменение не теряется; счетчики непрочитанного хранятся.

// joinedRooms - join-комнаты ответа, по алфавиту.
func joinedRooms(resp syncResponse) []string {
	out := make([]string, 0, len(resp.Rooms.Join))
	for roomID := range resp.Rooms.Join {
		out = append(out, roomID)
	}

	sort.Strings(out)

	return out
}

// ephemeralTypes - типы эфемерных событий комнаты.
func ephemeralTypes(room syncJoinedRoom) []string {
	out := make([]string, 0, len(room.Ephemeral.Events))
	for _, ev := range room.Ephemeral.Events {
		out = append(out, ev.Type)
	}

	sort.Strings(out)

	return out
}

// TestSyncQuietRoomsSkippedNoChangeLost - пустой incremental не отдает ни одной
// комнаты; каждое изменение (сообщение, редакция, состояние, typing и его
// остановка, receipt, room account data, приватный receipt со сменой счетчика,
// redaction, приглашение) приходит следующим incremental только в своей комнате.
func TestSyncQuietRoomsSkippedNoChangeLost(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "qralice", "qr-alice-pass")
	bob := registerPusherProbe(t, env.srv, "qrbob", "qr-bob-pass")

	const bobID = "@qrbob:test.local"

	roomA := env.room(alice, bob)
	roomB := env.room(alice, bob)
	roomC := env.room(alice, bob)
	first := env.send(alice, roomA, "a1", "первое")
	inB := env.send(alice, roomB, "b1", "в B")
	inC := env.send(alice, roomC, "c1", "в C")

	initial, _ := env.sync(bob, "")
	if got := joinedRooms(initial); len(got) != 3 {
		t.Fatalf("initial: комнаты %v, ожидали 3", got)
	}

	next := initial.NextBatch

	// step - действие и проверка: в incremental ровно комната want.
	step := func(name, want string, action func()) syncResponse {
		t.Helper()

		action()

		resp, _ := env.sync(bob, next)
		if got := joinedRooms(resp); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s: комнаты %v, ожидали только %s", name, got, want)
		}

		next = resp.NextBatch

		// Повторный incremental после изменения - снова пусто.
		again, _ := env.sync(bob, next)
		if got := joinedRooms(again); len(got) != 0 {
			t.Fatalf("%s: повторный incremental отдал %v", name, got)
		}

		next = again.NextBatch

		return resp
	}

	empty, _ := env.sync(bob, next)
	if got := joinedRooms(empty); len(got) != 0 {
		t.Fatalf("пустой incremental отдал комнаты %v", got)
	}

	next = empty.NextBatch

	resp := step("сообщение", roomA, func() { env.send(alice, roomA, "a2", "второе") })
	if n := len(resp.Rooms.Join[roomA].Timeline.Events); n != 1 {
		t.Fatalf("сообщение: %d событий", n)
	}

	step("редакция", roomB, func() {
		env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomB+"/send/m.room.message/edit1",
			`{"msgtype":"m.text","body":"* новое","m.new_content":{"msgtype":"m.text","body":"новое"},`+
				`"m.relates_to":{"rel_type":"m.replace","event_id":"`+inB+`"}}`)
	})

	resp = step("состояние", roomC, func() {
		env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomC+"/state/m.room.topic/", `{"topic":"тема"}`)
	})
	if len(resp.Rooms.Join[roomC].State.Events)+len(resp.Rooms.Join[roomC].Timeline.Events) == 0 {
		t.Fatal("смена состояния без событий")
	}

	typing := "/_matrix/client/v3/rooms/" + roomA + "/typing/" + url.PathEscape("@qralice:test.local")

	resp = step("typing", roomA, func() { env.call(alice, http.MethodPut, typing, `{"typing":true,"timeout":30000}`) })
	if got := ephemeralTypes(resp.Rooms.Join[roomA]); !slices.Equal(got, []string{"m.typing"}) {
		t.Fatalf("typing: эфемерные %v, ожидали только m.typing (receipts не менялись)", got)
	}

	resp = step("остановка typing", roomA, func() { env.call(alice, http.MethodPut, typing, `{"typing":false}`) })
	if raw, _ := json.Marshal(resp.Rooms.Join[roomA].Ephemeral); string(raw) != `{"events":[{"type":"m.typing","content":{"user_ids":[]}}]}` {
		t.Fatalf("остановка typing: %s", raw)
	}

	resp = step("receipt", roomB, func() {
		env.call(alice, http.MethodPost, "/_matrix/client/v3/rooms/"+roomB+"/receipt/m.read/"+url.PathEscape(inB), `{}`)
	})
	if got := ephemeralTypes(resp.Rooms.Join[roomB]); !slices.Equal(got, []string{"m.receipt"}) {
		t.Fatalf("receipt: эфемерные %v", got)
	}

	resp = step("room account data", roomC, func() {
		env.call(bob, http.MethodPut, "/_matrix/client/v3/user/"+url.PathEscape(bobID)+"/rooms/"+roomC+
			"/account_data/org.example.note", `{"x":1}`)
	})
	if n := len(resp.Rooms.Join[roomC].AccountData.Events); n != 1 {
		t.Fatalf("room account data: %d событий", n)
	}

	// Счетчик тихой иначе комнаты: приватный receipt bob сбрасывает его. Ни
	// событий, ни публичного receipt нет, комната обязана прийти с новым счетчиком.
	before := initial.Rooms.Join[roomA].UnreadNotifications
	if before == nil || before.Notification != 1 {
		t.Fatalf("initial: счетчик комнаты A %+v, ожидали 1", before)
	}

	resp = step("m.read.private", roomA, func() {
		env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomA+"/receipt/m.read.private/"+
			url.PathEscape(first), `{}`)
	})
	if c := resp.Rooms.Join[roomA].UnreadNotifications; c == nil || c.Notification != 1 {
		// После second (a2) непрочитанным остается одно сообщение.
		t.Fatalf("m.read.private: счетчик %+v, ожидали 1", c)
	}

	// Приватный receipt alice bob не видит - и комната ради него не отдается.
	env.call(alice, http.MethodPost, "/_matrix/client/v3/rooms/"+roomC+"/receipt/m.read.private/"+url.PathEscape(inC), `{}`)

	if quiet, _ := env.sync(bob, next); len(quiet.Rooms.Join) != 0 {
		t.Fatalf("чужой приватный receipt отдал комнаты %v", joinedRooms(quiet))
	} else {
		next = quiet.NextBatch
	}

	step("redaction", roomB, func() {
		env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomB+"/redact/"+url.PathEscape(inB)+"/rd1", `{}`)
	})

	// Приглашение отдается один раз.
	rec, _ := env.call(alice, http.MethodPost, "/_matrix/client/v3/createRoom",
		`{"preset":"private_chat","invite":["`+bobID+`"]}`)

	var created struct {
		RoomID string `json:"room_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	invited, _ := env.sync(bob, next)
	if _, ok := invited.Rooms.Invite[created.RoomID]; !ok {
		t.Fatalf("приглашение не пришло: %+v", invited.Rooms.Invite)
	}

	again, _ := env.sync(bob, invited.NextBatch)
	if len(again.Rooms.Invite) != 0 || len(again.Rooms.Join) != 0 {
		t.Fatalf("приглашение повторилось: invite %v, join %v", again.Rooms.Invite, joinedRooms(again))
	}

	// Токен без версии эфемерных событий (старого формата) читается и отдает все
	// комнаты, как раньше.
	position, _, _, err := env.srv.syncTokens.Decode3(again.NextBatch)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	legacy, _ := env.sync(bob, env.srv.syncTokens.Encode2(position, 0))
	if got := joinedRooms(legacy); len(got) != 3 {
		t.Fatalf("старый токен: комнаты %v, ожидали все 3", got)
	}

	if len(legacy.Rooms.Invite) != 0 {
		t.Fatalf("старый токен повторил приглашение: %v", legacy.Rooms.Invite)
	}
}

// TestSyncUnreadThreadNotifications - счетчики тредов отдельно, если фильтр
// просит unread_thread_notifications; receipt треда сбрасывает только тред.
func TestSyncUnreadThreadNotifications(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "thalice", "th-alice-pass")
	bob := registerPusherProbe(t, env.srv, "thbob", "th-bob-pass")
	roomID := env.room(alice, bob)

	root := env.send(alice, roomID, "t0", "корень")

	thread := func(txn string) string {
		rec, _ := env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/send/m.room.message/"+txn,
			`{"msgtype":"m.text","body":"в треде","m.relates_to":{"rel_type":"m.thread","event_id":"`+root+`"}}`)

		var sent struct {
			EventID string `json:"event_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &sent)

		return sent.EventID
	}

	thread("t1")
	inThread := thread("t2")

	filter := url.QueryEscape(`{"room":{"timeline":{"unread_thread_notifications":true}}}`)

	syncThreads := func() syncJoinedRoom {
		rec, _ := env.call(bob, http.MethodGet, "/_matrix/client/v3/sync?timeout=0&filter="+filter, "")

		var resp syncResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		return resp.Rooms.Join[roomID]
	}

	room := syncThreads()
	if room.UnreadNotifications == nil || room.UnreadNotifications.Notification != 1 {
		t.Fatalf("основная лента: %+v, ожидали 1", room.UnreadNotifications)
	}

	if got := room.UnreadThreadNotifications[root]; got.Notification != 2 {
		t.Fatalf("тред: %+v, ожидали 2", room.UnreadThreadNotifications)
	}

	// Без фильтра - общий счетчик.
	all, _ := env.sync(bob, "")
	if c := all.Rooms.Join[roomID].UnreadNotifications; c == nil || c.Notification != 3 {
		t.Fatalf("без фильтра: %+v, ожидали 3", c)
	}

	env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read/"+url.PathEscape(inThread),
		`{"thread_id":"`+root+`"}`)

	room = syncThreads()
	if room.UnreadNotifications.Notification != 1 || len(room.UnreadThreadNotifications) != 0 {
		t.Fatalf("после receipt треда: лента %+v, треды %+v", room.UnreadNotifications, room.UnreadThreadNotifications)
	}
}

// stopPushActions останавливает фоновый разбор push-действий сервера.
func (e perfEnv) stopPushActions() {
	e.t.Helper()

	if err := e.srv.pushActions.Close(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// restartPushActions - сервис хранимых действий как в новом процессе.
func (e perfEnv) restartPushActions() {
	e.t.Helper()

	e.stopPushActions()
	e.srv.pushActions = e.srv.newPushActions(e.srv.db)
}

// TestPushActionsReceiptBeforeProcessing - receipt пришел раньше, чем событие
// разобрано (клиент увидел его через /messages): прочитанное не считается.
// Затем досчет старых событий после обновления (миграция 0039) восстанавливает
// счетчики, как они считались раньше.
func TestPushActionsReceiptBeforeProcessing(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}
	ctx := context.Background()

	alice := registerPusherProbe(t, env.srv, "rbalice", "rb-alice-pass")
	bob := registerPusherProbe(t, env.srv, "rbbob", "rb-bob-pass")
	roomID := env.room(alice, bob)

	env.send(alice, roomID, "p1", "один")
	read := env.send(alice, roomID, "p2", "два")
	env.send(alice, roomID, "p3", "три")

	// Откатываем обработку: события еще не разобраны. Фоновый разбор сначала
	// останавливаем, иначе он допишет строки после отката.
	env.stopPushActions()

	pool := env.srv.db.Pool()
	if _, err := pool.Exec(ctx, `DELETE FROM event_push_actions; DELETE FROM event_push_summary`); err != nil {
		t.Fatal(err)
	}

	var before int64
	if err := pool.QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = $1`, read).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE event_push_stream SET processed = $1`, before-10); err != nil {
		t.Fatal(err)
	}

	env.restartPushActions()

	env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/receipt/m.read/"+url.PathEscape(read), `{}`)

	resp, _ := env.sync(bob, "")
	if c := resp.Rooms.Join[roomID].UnreadNotifications; c == nil || c.Notification != 1 {
		t.Fatalf("после receipt до разбора: %+v, ожидали 1", c)
	}

	// Досчет: база как сразу после миграции - действий нет, граница досчета на
	// текущей позиции.
	env.stopPushActions()

	if _, err := pool.Exec(ctx, `DELETE FROM event_push_actions; DELETE FROM event_push_summary`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE event_push_stream SET backfill_upto = processed`); err != nil {
		t.Fatal(err)
	}

	env.restartPushActions()

	// Досчет идет в фоне при старте, без /sync.
	env.srv.pushActions.Signal()

	for deadline := time.Now().Add(10 * time.Second); ; {
		var done bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_push_backfill WHERE user_id = '@rbbob:test.local')`).
			Scan(&done); err != nil {
			t.Fatal(err)
		}

		if done {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("досчет при старте не выполнен")
		}

		time.Sleep(20 * time.Millisecond)
	}

	resp, _ = env.sync(bob, "")
	if c := resp.Rooms.Join[roomID].UnreadNotifications; c == nil || c.Notification != 1 {
		t.Fatalf("после досчета: %+v, ожидали 1", c)
	}

	// Досчет разовый: повторный (новый процесс) ничего не дублирует.
	env.restartPushActions()

	resp, _ = env.sync(bob, "")
	if c := resp.Rooms.Join[roomID].UnreadNotifications; c == nil || c.Notification != 1 {
		t.Fatalf("повторно: %+v, ожидали 1", c)
	}
}

// TestPushActionsRedactionAndLeave - удаленное событие перестает считаться;
// после выхода и возвращения старые уведомления не всплывают.
func TestPushActionsRedactionAndLeave(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "rlalice", "rl-alice-pass")
	bob := registerPusherProbe(t, env.srv, "rlbob", "rl-bob-pass")
	roomID := env.room(alice, bob)

	removed := env.send(alice, roomID, "r1", "удалю")
	env.send(alice, roomID, "r2", "останется")

	count := func() int {
		resp, _ := env.sync(bob, "")

		room, ok := resp.Rooms.Join[roomID]
		if !ok || room.UnreadNotifications == nil {
			t.Fatalf("нет комнаты или счетчика: %+v", resp.Rooms.Join)
		}

		return room.UnreadNotifications.Notification
	}

	if got := count(); got != 2 {
		t.Fatalf("до redaction: %d", got)
	}

	env.call(alice, http.MethodPut, "/_matrix/client/v3/rooms/"+roomID+"/redact/"+url.PathEscape(removed)+"/x1", `{}`)

	if got := count(); got != 1 {
		t.Fatalf("после redaction: %d, ожидали 1", got)
	}

	env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/leave", `{}`)
	env.call(bob, http.MethodPost, "/_matrix/client/v3/rooms/"+roomID+"/join", `{}`)

	if got := count(); got != 0 {
		t.Fatalf("после выхода и входа: %d, ожидали 0", got)
	}

	env.send(alice, roomID, "r3", "новое")

	if got := count(); got != 1 {
		t.Fatalf("новое после входа: %d, ожидали 1", got)
	}
}

// TestPushActionsConcurrentSends - одновременные отправки и /sync
// не ждут друг друга на разборе push-действий, и после них счетчики полные:
// каждое сообщение посчитано получателю ровно один раз.
func TestPushActionsConcurrentSends(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "csalice", "cs-alice-pass")
	carol := registerPusherProbe(t, env.srv, "cscarol", "cs-carol-pass")
	bob := registerPusherProbe(t, env.srv, "csbob", "cs-bob-pass")
	roomID := env.room(alice, carol, bob)

	const n = 20

	request := func(token, method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)

		return rec.Code, rec.Body.String()
	}

	var wg sync.WaitGroup

	for i := range n {
		sender := alice
		if i%2 == 1 {
			sender = carol
		}

		wg.Add(2)

		go func() {
			defer wg.Done()

			path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/cs%d", roomID, i)
			// Тело уникально: одинаковые события одной миллисекунды совпали бы по event_id.
			msg := fmt.Sprintf(`{"msgtype":"m.text","body":"x%d"}`, i)
			if code, body := request(sender, http.MethodPut, path, msg); code != http.StatusOK {
				t.Errorf("send %d: %d %s", i, code, body)
			}
		}()

		go func() {
			defer wg.Done()

			if code, body := request(bob, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", ""); code != http.StatusOK {
				t.Errorf("sync %d: %d %s", i, code, body)
			}
		}()
	}

	wg.Wait()

	resp, _ := env.sync(bob, "")
	if c := resp.Rooms.Join[roomID].UnreadNotifications; c == nil || c.Notification != n {
		t.Fatalf("счетчик bob после %d отправок: %+v", n, c)
	}
}
