//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

const (
	redactAlice = "@alice:test.local"
	redactBob   = "@bob:test.local"
)

// redactFixture - комната с одним сообщением alice.
type redactFixture struct {
	rooms  *store.RoomStore
	db     *store.DB
	svc    *core.RoomService
	roomID string
	msgID  string
}

func newRedactFixture(t *testing.T, version int) redactFixture {
	t.Helper()

	_ = freshRoomStore(t) // схема с нуля
	db := openDB(t)
	rooms := store.NewRoomStore(db)
	ctx := context.Background()

	svc := core.NewRoomService(rooms, "test.local").WithClock(func() time.Time {
		return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	})

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: redactAlice, Preset: "public_chat", RoomVersion: version,
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	msgID, _, err := svc.SendMessage(ctx, redactAlice, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"msgtype":"m.text","body":"секретное слово"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	return redactFixture{rooms: rooms, db: db, svc: svc, roomID: roomID, msgID: msgID}
}

// storedContent - content события как он лежит в таблице events.
func storedContent(t *testing.T, db *store.DB, eventID string) map[string]any {
	t.Helper()

	var raw []byte
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT content FROM events WHERE event_id = $1`, eventID).Scan(&raw); err != nil {
		t.Fatalf("content %s: %v", eventID, err)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("content %s: %v", eventID, err)
	}

	return out
}

func redactedBy(t *testing.T, db *store.DB, eventID string) string {
	t.Helper()

	var by string
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT coalesce(redacted_by, '') FROM events WHERE event_id = $1`, eventID).Scan(&by); err != nil {
		t.Fatalf("redacted_by %s: %v", eventID, err)
	}

	return by
}

func countRedactions(t *testing.T, db *store.DB, roomID string) int {
	t.Helper()

	var n int
	if err := db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE room_id = $1 AND type = 'm.room.redaction'`, roomID).Scan(&n); err != nil {
		t.Fatalf("count redactions: %v", err)
	}

	return n
}

// TestRedactAtomicOnFailure - сбой пометки цели откатывает и само
// событие m.room.redaction. Раньше событие коммитилось отдельно, повтор с тем же
// txn отвечал duplicate=true, а цель навсегда оставалась без redacted_by.
func TestRedactAtomicOnFailure(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	// «Обрыв» между вставкой события и пометкой цели: UPDATE redacted_by падает.
	if _, err := f.db.Pool().Exec(ctx, `
		CREATE FUNCTION syngo_test_fail_redact() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'обрыв'; END $$;
		CREATE TRIGGER syngo_test_fail_redact BEFORE UPDATE OF redacted_by ON events
		FOR EACH ROW EXECUTE FUNCTION syngo_test_fail_redact();`); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	dropTrigger := func() error {
		_, err := f.db.Pool().Exec(ctx, `
			DROP TRIGGER IF EXISTS syngo_test_fail_redact ON events;
			DROP FUNCTION IF EXISTS syngo_test_fail_redact();`)

		return err
	}

	t.Cleanup(func() { _ = dropTrigger() })

	if _, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "tx1"); err == nil {
		t.Fatal("Redact при сбое пометки должен вернуть ошибку")
	}

	if n := countRedactions(t, f.db, f.roomID); n != 0 {
		t.Fatalf("событие redaction осталось после сбоя: %d", n)
	}

	if err := dropTrigger(); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	id, dup, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "tx1")
	if err != nil || dup {
		t.Fatalf("повтор Redact: id=%s dup=%v err=%v", id, dup, err)
	}

	if by := redactedBy(t, f.db, f.msgID); by != id {
		t.Fatalf("redacted_by = %q, ожидали %q", by, id)
	}
}

// TestRedactRetryAfterCrash - событие
// redaction записано с txn, процесс «упал» до пометки цели; повтор с тем же txn
// обязан оставить цель удаленной. Пометка теперь в транзакции AppendEvent.
func TestRedactRetryAfterCrash(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	prev, err := f.rooms.LatestEvents(ctx, f.roomID, 1)
	if err != nil {
		t.Fatalf("LatestEvents: %v", err)
	}

	redaction := core.Event{
		EventID: "$crash-redaction:test.local", RoomID: f.roomID, Type: "m.room.redaction",
		Sender: redactAlice, Content: json.RawMessage(`{"redacts":"` + f.msgID + `"}`),
		PrevEvents: []string{prev[0].EventID}, Depth: prev[0].Depth + 1, OriginServerTS: 1, Origin: "test.local",
	}

	txn := &core.TransactionKey{UserID: redactAlice, DeviceID: "DEV", Endpoint: "redact:" + f.roomID, TxnID: "tx1"}
	if _, _, err := f.rooms.AppendEvent(ctx, redaction, txn); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	id, dup, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "tx1")
	if err != nil || !dup || id != redaction.EventID {
		t.Fatalf("повтор Redact: id=%s dup=%v err=%v", id, dup, err)
	}

	if by := redactedBy(t, f.db, f.msgID); by != redaction.EventID {
		t.Fatalf("цель после повтора не удалена: redacted_by = %q", by)
	}
}

// TestRedactKeepsFirstRedaction - повторная redaction уже удаленного события не
// переписывает redacted_by: unsigned.redacted_because не «прыгает».
func TestRedactKeepsFirstRedaction(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	first, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "r1")
	if err != nil {
		t.Fatalf("первая redaction: %v", err)
	}

	if _, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "r2"); err != nil {
		t.Fatalf("вторая redaction: %v", err)
	}

	if by := redactedBy(t, f.db, f.msgID); by != first {
		t.Fatalf("redacted_by = %q, ожидали первую redaction %q", by, first)
	}
}

// TestRedactConcurrent - параллельные redaction одного события: все события
// записаны, цель помечена ровно одной из них и content вырезан.
func TestRedactConcurrent(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	const n = 8

	ids := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			reason := "c" + string(rune('a'+i))
			ids[i], _, errs[i] = f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, reason, reason)
		}()
	}

	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("redaction %d: %v", i, errs[i])
		}
	}

	by := redactedBy(t, f.db, f.msgID)
	found := false

	for _, id := range ids {
		found = found || id == by
	}

	if !found {
		t.Fatalf("redacted_by = %q не из записанных redaction %v", by, ids)
	}

	if _, ok := storedContent(t, f.db, f.msgID)["body"]; ok {
		t.Fatal("body остался в events.content")
	}
}

// TestRedactPrunesStoredContent - после redaction оригинал не лежит в
// events.content и не находится поиском (раньше вырезание было только на чтении,
// а полнотекстовый индекс по body продолжал находить удаленное сообщение).
func TestRedactPrunesStoredContent(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	if _, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "r1"); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if got := storedContent(t, f.db, f.msgID); len(got) != 0 {
		t.Fatalf("events.content после redaction = %v, ожидали {}", got)
	}

	hits, err := f.db.SearchMessages(ctx, map[string]store.Visibility{f.roomID: {}}, "секретное", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}

	if len(hits) != 0 {
		t.Fatalf("поиск нашел удаленное сообщение: %d", len(hits))
	}
}

// TestRedactionOfRedactionV11 - redaction самой redaction (v11): цель первой
// остается удаленной, у первой redaction по правилам v11 сохраняется content.redacts.
// Раньше при чтении применялись правила v10 и redacts терялся.
func TestRedactionOfRedactionV11(t *testing.T) {
	f := newRedactFixture(t, 11)
	ctx := context.Background()

	first, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "причина", "r1")
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	second, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, first, "", "r2")
	if err != nil {
		t.Fatalf("Redact redaction: %v", err)
	}

	if by := redactedBy(t, f.db, f.msgID); by != first {
		t.Fatalf("сообщение: redacted_by = %q, ожидали %q", by, first)
	}

	if by := redactedBy(t, f.db, first); by != second {
		t.Fatalf("первая redaction: redacted_by = %q, ожидали %q", by, second)
	}

	ev, err := f.rooms.EventByID(ctx, f.roomID, first)
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(ev.Content, &got); err != nil || len(got) != 1 || got["redacts"] != f.msgID {
		t.Fatalf("content удаленной redaction v11 = %s", ev.Content)
	}
}

// fedRoom - комната для федеративных PDU: alice (наш сервер) и remote-участники.
func fedRoom(t *testing.T) (*store.DB, *store.RoomStore, string) {
	t.Helper()

	rooms := freshRoomStore(t)
	db := openDB(t)
	ctx := context.Background()

	svc := core.NewRoomService(rooms, "test.local")

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{Creator: redactAlice, Preset: "public_chat"})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	return db, rooms, roomID
}

func fedPDU(roomID, eventID, eventType, sender, content, redacts string) store.FedPDU {
	return store.FedPDU{
		EventID: eventID, RoomID: roomID, Type: eventType, Sender: sender,
		Content: []byte(content), Redacts: redacts, Depth: 10, OriginTS: 1, Origin: "remote.test",
	}
}

func applyTxn(t *testing.T, db *store.DB, txnID string, pdus ...store.FedPDU) {
	t.Helper()

	// Только запись событий и redaction (состояние здесь не проверяется).
	if _, err := db.ApplyFederationEvents(context.Background(), "remote.test", pdus[0].RoomID, pdus,
		func(core.RoomStateTx) (core.RoomStateResult, error) { return core.RoomStateResult{}, nil }); err != nil {
		t.Fatalf("ApplyFederationEvents %s: %v", txnID, err)
	}
}

// assertHidden проверяет, что content цели скрыт в /messages (RoomEvents) и /sync
// (EventsSince) и вырезан в хранилище.
func assertHidden(t *testing.T, db *store.DB, rooms *store.RoomStore, roomID, eventID, wantBy string) {
	t.Helper()

	ctx := context.Background()

	if by := redactedBy(t, db, eventID); by != wantBy {
		t.Fatalf("redacted_by = %q, ожидали %q", by, wantBy)
	}

	check := func(where string, events []core.Event) {
		t.Helper()

		for i := range events {
			if events[i].EventID == eventID {
				if strings.Contains(string(events[i].Content), "body") {
					t.Fatalf("%s: content не скрыт: %s", where, events[i].Content)
				}

				return
			}
		}

		t.Fatalf("%s: события нет в выдаче", where)
	}

	messages, err := rooms.RoomEvents(ctx, roomID, 1<<62, 100, true)
	if err != nil {
		t.Fatalf("RoomEvents: %v", err)
	}

	check("/messages", messages)

	byRoom, err := store.NewSyncStore(db).EventsSince(ctx, []string{roomID}, 0, 100, nil)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}

	check("/sync", byRoom[roomID])

	if _, ok := storedContent(t, db, eventID)["body"]; ok {
		t.Fatal("body остался в events.content")
	}
}

// TestFederationRedactionHidesContent - redaction из федерации (v10,
// цель в top-level redacts) применяется в той же транзакции, что и запись PDU,
// и скрывает content цели в /messages и /sync.
func TestFederationRedactionHidesContent(t *testing.T) {
	db, rooms, roomID := fedRoom(t)

	applyTxn(t, db, "t1", fedPDU(roomID, "$msg:remote.test", "m.room.message", "@carol:remote.test",
		`{"msgtype":"m.text","body":"секрет"}`, ""))
	applyTxn(t, db, "t2", fedPDU(roomID, "$red:remote.test", "m.room.redaction", "@carol:remote.test",
		`{"reason":"x"}`, "$msg:remote.test"))

	assertHidden(t, db, rooms, roomID, "$msg:remote.test", "$red:remote.test")
}

// TestRedactionBeforeTarget - redaction пришла раньше цели: применяется, когда
// цель записывается (как таблица redactions в Synapse).
func TestRedactionBeforeTarget(t *testing.T) {
	db, rooms, roomID := fedRoom(t)

	applyTxn(t, db, "t1", fedPDU(roomID, "$red:remote.test", "m.room.redaction", "@carol:remote.test",
		`{"redacts":"$late:remote.test"}`, ""))
	applyTxn(t, db, "t2", fedPDU(roomID, "$late:remote.test", "m.room.message", "@carol:remote.test",
		`{"msgtype":"m.text","body":"поздно"}`, ""))

	assertHidden(t, db, rooms, roomID, "$late:remote.test", "$red:remote.test")
}

// TestFederationRedactionForeignDomainNeedsPower - чужой сервер без уровня redact
// не может скрыть событие другого сервера (v3+: redaction применяется, если домены
// отправителей совпадают или у отправителя есть уровень redact).
func TestFederationRedactionForeignDomainNeedsPower(t *testing.T) {
	db, _, roomID := fedRoom(t)
	ctx := context.Background()

	applyTxn(t, db, "t1", fedPDU(roomID, "$red:remote.test", "m.room.redaction", "@carol:remote.test",
		`{"redacts":"$own:test.local"}`, ""))

	rooms := store.NewRoomStore(db)

	prev, err := rooms.LatestEvents(ctx, roomID, 1)
	if err != nil {
		t.Fatalf("LatestEvents: %v", err)
	}

	own := core.Event{
		EventID: "$own:test.local", RoomID: roomID, Type: "m.room.message", Sender: redactAlice,
		Content: json.RawMessage(`{"msgtype":"m.text","body":"мое"}`), PrevEvents: []string{prev[0].EventID},
		Depth: prev[0].Depth + 1, OriginServerTS: 1, Origin: "test.local",
	}
	if _, _, err := rooms.AppendEvent(ctx, own, nil); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if by := redactedBy(t, db, own.EventID); by != "" {
		t.Fatalf("redaction чужого сервера без прав применена: %q", by)
	}

	if _, ok := storedContent(t, db, own.EventID)["body"]; !ok {
		t.Fatal("content вырезан без права на redaction")
	}
}

// TestRedactionOriginalRetention - оригинал удаленного события хранится
// отдельно и стирается по redaction.retention_period; при 0 не сохраняется вовсе.
func TestRedactionOriginalRetention(t *testing.T) {
	f := newRedactFixture(t, 10)
	ctx := context.Background()

	original := func(redactionID string) []byte {
		t.Helper()

		var raw []byte
		if err := f.db.Pool().QueryRow(ctx, `SELECT original_content FROM redactions WHERE event_id = $1`,
			redactionID).Scan(&raw); err != nil {
			t.Fatalf("original_content %s: %v", redactionID, err)
		}

		return raw
	}

	f.db.SetRedactionRetention(time.Hour)

	kept, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, f.msgID, "", "r1")
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if !strings.Contains(string(original(kept)), "секретное слово") {
		t.Fatalf("оригинал не сохранен: %s", original(kept))
	}

	// Срок не истек - оригинал остается.
	if n, err := f.db.PurgeRedactedOriginals(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("PurgeRedactedOriginals(1h) = %d, %v", n, err)
	}

	if _, err := f.db.Pool().Exec(ctx, `UPDATE redactions SET received_at = now() - interval '2 hours'`); err != nil {
		t.Fatalf("age: %v", err)
	}

	if n, err := f.db.PurgeRedactedOriginals(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("PurgeRedactedOriginals после срока = %d, %v", n, err)
	}

	if raw := original(kept); raw != nil {
		t.Fatalf("оригинал не стерт: %s", raw)
	}

	// retention_period = 0: оригинал не сохраняется.
	f.db.SetRedactionRetention(0)

	second, _, err := f.svc.SendMessage(ctx, redactAlice, "DEV", f.roomID, "m.room.message", "m2",
		json.RawMessage(`{"msgtype":"m.text","body":"второе"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	dropped, _, err := f.svc.Redact(ctx, redactAlice, "DEV", f.roomID, second, "", "r2")
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}

	if raw := original(dropped); raw != nil {
		t.Fatalf("при retention 0 оригинал сохранен: %s", raw)
	}
}

// TestPruneLegacyRedacted - события, удаленные до миграции 0029 (redacted_by
// есть, content - оригинал), вырезаются при старте; оригинал переносится в redactions.
func TestPruneLegacyRedacted(t *testing.T) {
	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean: %v", err)
	}

	if err := migrator.Migrate(27); err != nil {
		t.Fatalf("migrate 27: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	db := openDB(t)
	ctx := context.Background()

	if _, err := db.Pool().Exec(ctx, `
		INSERT INTO rooms (room_id, room_version, creator) VALUES ('!legacy:test.local', '10', '@alice:test.local');
		INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin, redacted_by)
		VALUES ('$old:test.local', '!legacy:test.local', 'm.room.message', '@alice:test.local',
		        '{"body":"старый секрет"}', 1, 1, 'test.local', '$oldred:test.local'),
		       ('$oldred:test.local', '!legacy:test.local', 'm.room.redaction', '@alice:test.local',
		        '{"redacts":"$old:test.local"}', 2, 2, 'test.local', NULL);`); err != nil {
		t.Fatalf("legacy rows: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	n, err := db.PruneRedactedContent(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PruneRedactedContent = %d, %v", n, err)
	}

	if got := storedContent(t, db, "$old:test.local"); len(got) != 0 {
		t.Fatalf("legacy content не вырезан: %v", got)
	}

	var raw []byte
	if err := db.Pool().QueryRow(ctx, `SELECT original_content FROM redactions WHERE event_id = '$oldred:test.local'`).
		Scan(&raw); err != nil || !strings.Contains(string(raw), "старый секрет") {
		t.Fatalf("оригинал legacy-события: %s, %v", raw, err)
	}

	if n, err := db.PruneRedactedContent(ctx); err != nil || n != 0 {
		t.Fatalf("повторный PruneRedactedContent = %d, %v", n, err)
	}
}

// TestFederationRedactionByV12Creator - в комнате v12 создатель
// (здесь дополнительный, с другого сервера) имеет бесконечный уровень без записи
// в power_levels и может удалить чужое событие. Раньше уровень по федерации
// считался только по power_levels (users_default 0 < redact 50), и redaction
// создателя-пользователя Synapse не применялась.
func TestFederationRedactionByV12Creator(t *testing.T) {
	rooms := freshRoomStore(t)
	db := openDB(t)
	ctx := context.Background()

	svc := core.NewRoomService(rooms, "test.local")

	roomID, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator: redactAlice, Preset: "public_chat", RoomVersion: 12,
		CreationExtras: map[string]any{"additional_creators": []any{"@carol:remote.test"}},
	})
	if err != nil {
		t.Fatalf("CreateRoom v12: %v", err)
	}

	msgID, _, err := svc.SendMessage(ctx, redactAlice, "DEV", roomID, "m.room.message", "m1",
		json.RawMessage(`{"msgtype":"m.text","body":"секрет"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	applyTxn(t, db, "t1", fedPDU(roomID, "$red:remote.test", "m.room.redaction", "@carol:remote.test",
		`{"redacts":"`+msgID+`"}`, ""))

	if by := redactedBy(t, db, msgID); by != "$red:remote.test" {
		t.Fatalf("redaction создателя v12 не применена: redacted_by = %q", by)
	}

	// Не создатель с другого сервера без уровня redact - по-прежнему без прав.
	other, _, err := svc.SendMessage(ctx, redactAlice, "DEV", roomID, "m.room.message", "m2",
		json.RawMessage(`{"msgtype":"m.text","body":"второе"}`))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	applyTxn(t, db, "t2", fedPDU(roomID, "$red2:other.test", "m.room.redaction", "@dave:other.test",
		`{"redacts":"`+other+`"}`, ""))

	if by := redactedBy(t, db, other); by != "" {
		t.Fatalf("redaction не создателя применена: %q", by)
	}
}
