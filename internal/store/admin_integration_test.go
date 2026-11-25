//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// freshAdminDB поднимает схему с нуля и открывает DB.
func freshAdminDB(t *testing.T) *store.DB {
	t.Helper()

	migrator := newMigrator(t)

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("clean before up: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("up: %v", err)
	}

	t.Cleanup(func() {
		_ = migrator.Down()
		_, _ = migrator.Close()
	})

	return openDB(t)
}

func adminExec(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Pool().Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func adminCount(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()

	var n int
	if err := db.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}

	return n
}

func adminUser(t *testing.T, db *store.DB, userID, localpart string) {
	t.Helper()
	adminExec(t, db, `INSERT INTO users (user_id, localpart, password_hash) VALUES ($1, $2, 'x')`, userID, localpart)
	adminExec(t, db, `INSERT INTO devices (user_id, device_id) VALUES ($1, 'DEV')`, userID)
}

// TestAdminLastAdminRace - две параллельные операции снятия прав с двух последних
// админов оставляют одного: вторая получает ErrLastAdmin. Раньше guard
// был подзапросом count(*) в UPDATE, и на READ COMMITTED оставалось 0 админов.
// Проверяются пары unadmin+unadmin и deactivate+unadmin.
func TestAdminLastAdminRace(t *testing.T) {
	db := freshAdminDB(t)
	ctx := context.Background()

	adminUser(t, db, "@a:t", "a")
	adminUser(t, db, "@b:t", "b")
	adminUser(t, db, "@root:t", "root")

	ops := map[string][2]func() error{
		"unadmin+unadmin": {
			func() error { return db.AdminSetUserAdmin(ctx, "@root:t", "@a:t", false) },
			func() error { return db.AdminSetUserAdmin(ctx, "@root:t", "@b:t", false) },
		},
		"deactivate+unadmin": {
			func() error { return db.AdminSetUserDeactivated(ctx, "@root:t", "@a:t", true) },
			func() error { return db.AdminSetUserAdmin(ctx, "@root:t", "@b:t", false) },
		},
	}

	for name, pair := range ops {
		for attempt := 1; attempt <= 100; attempt++ {
			adminExec(t, db, `UPDATE users SET is_admin = (user_id IN ('@a:t','@b:t')), deactivated = false`)

			var (
				wg   sync.WaitGroup
				errs [2]error
			)

			start := make(chan struct{})

			for i := range pair {
				wg.Add(1)

				go func(i int) {
					defer wg.Done()
					<-start
					errs[i] = pair[i]()
				}(i)
			}

			close(start)
			wg.Wait()

			active := adminCount(t, db, `SELECT count(*) FROM users WHERE is_admin AND NOT deactivated`)
			if active != 1 {
				t.Fatalf("%s, попытка %d: активных админов %d, ожидали 1 (ошибки: %v, %v)",
					name, attempt, active, errs[0], errs[1])
			}

			lastAdmin := 0

			for _, err := range errs {
				switch {
				case err == nil:
				case errors.Is(err, core.ErrLastAdmin):
					lastAdmin++
				default:
					t.Fatalf("%s, попытка %d: неожиданная ошибка %v", name, attempt, err)
				}
			}

			if lastAdmin != 1 {
				t.Fatalf("%s, попытка %d: ErrLastAdmin у %d операций, ожидали у одной", name, attempt, lastAdmin)
			}
		}
	}
}

// TestAdminUnadminRevokesAdminTokens - снятие роли отзывает admin-токены: иначе при
// повторной выдаче роли старый токен снова заработал бы.
func TestAdminUnadminRevokesAdminTokens(t *testing.T) {
	db := freshAdminDB(t)
	ctx := context.Background()

	adminUser(t, db, "@a:t", "a")
	adminUser(t, db, "@root:t", "root")
	adminExec(t, db, `UPDATE users SET is_admin = true`)
	adminExec(t, db, `INSERT INTO access_tokens (token_hash, user_id, device_id, scope) VALUES ('\x01', '@a:t', 'DEV', 'admin')`)
	adminExec(t, db, `INSERT INTO access_tokens (token_hash, user_id, device_id, scope) VALUES ('\x02', '@a:t', 'DEV', '')`)

	if err := db.AdminSetUserAdmin(ctx, "@root:t", "@a:t", false); err != nil {
		t.Fatalf("unadmin: %v", err)
	}

	if n := adminCount(t, db, `SELECT count(*) FROM access_tokens WHERE user_id = '@a:t' AND scope = 'admin' AND revoked_at IS NULL`); n != 0 {
		t.Fatalf("после unadmin активных admin-токенов: %d", n)
	}

	if n := adminCount(t, db, `SELECT count(*) FROM access_tokens WHERE user_id = '@a:t' AND scope = '' AND revoked_at IS NULL`); n != 1 {
		t.Fatalf("обычный токен тоже отозван: активных %d", n)
	}

	var details json.RawMessage
	if err := db.Pool().QueryRow(ctx, `SELECT details FROM admin_audit WHERE action = 'unadmin'`).Scan(&details); err != nil {
		t.Fatalf("аудит unadmin: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(details, &parsed); err != nil || parsed["result"] != store.AuditResultOK {
		t.Fatalf("details аудита unadmin: %s", details)
	}
}

// TestAdminPurgeRoomOrphans - purge не оставляет строк комнаты в таблицах без
// внешнего ключа на rooms, блокировка переживает purge, повтор txn после purge -
// новое событие, а не 500; BlockRoom идемпотентен и работает для неизвестной
// комнаты.
func TestAdminPurgeRoomOrphans(t *testing.T) {
	db := freshAdminDB(t)
	ctx := context.Background()
	rooms := store.NewRoomStore(db)

	adminUser(t, db, "@alice:t", "alice")
	adminExec(t, db, `INSERT INTO rooms (room_id, creator) VALUES ('!r:t', '@alice:t')`)
	adminExec(t, db, `INSERT INTO rooms (room_id, creator) VALUES ('!keep:t', '@alice:t')`)

	txn := &core.TransactionKey{UserID: "@alice:t", DeviceID: "DEV", Endpoint: "send:!r:t", TxnID: "t1"}
	ev := core.Event{
		EventID: "$purge-1", RoomID: "!r:t", Type: "m.room.message", Sender: "@alice:t",
		Content: json.RawMessage(`{"body":"x"}`), Depth: 1, OriginServerTS: 1700000000000, Origin: "t",
	}

	if _, _, err := rooms.AppendEvent(ctx, ev, txn); err != nil {
		t.Fatalf("append: %v", err)
	}

	keepEv := ev
	keepEv.EventID, keepEv.RoomID = "$keep-1", "!keep:t"

	if _, _, err := rooms.AppendEvent(ctx, keepEv, nil); err != nil {
		t.Fatalf("append keep: %v", err)
	}

	for _, room := range []string{"!r:t", "!keep:t"} {
		eventID := "$purge-1"
		if room == "!keep:t" {
			eventID = "$keep-1"
		}

		adminExec(t, db, `INSERT INTO event_auth (event_id, auth_event_id) VALUES ($1, '$x')`, eventID)
		adminExec(t, db, `INSERT INTO account_data (user_id, room_id, type, content) VALUES ('@alice:t', $1, 'm.tag', '{}')`, room)
		adminExec(t, db, `INSERT INTO destination_rooms (destination, room_id) VALUES ('remote', $1)`, room)
		adminExec(t, db, `INSERT INTO federation_outbox (destination, pdu_json, room_id) VALUES ('remote', json_build_object('room_id', $1::text)::text, $1)`, room)
		adminExec(t, db, `INSERT INTO receipts (room_id, user_id, receipt_type, event_id, ts) VALUES ($1, '@alice:t', 'm.read', $2, 1)`, room, eventID)
	}

	if err := db.BlockRoom(ctx, "!r:t", "@admin:t"); err != nil {
		t.Fatalf("BlockRoom: %v", err)
	}

	if err := db.BlockRoom(ctx, "!r:t", "@admin:t"); err != nil {
		t.Fatalf("повторный BlockRoom: %v, ожидали nil", err)
	}

	if err := db.BlockRoom(ctx, "!unknown:t", "@admin:t"); err != nil {
		t.Fatalf("BlockRoom неизвестной комнаты: %v, ожидали nil", err)
	}

	if err := db.AdminPurgeRoom(ctx, "@admin:t", "!r:t", false, map[string]any{"notified": 0}); err != nil {
		t.Fatalf("purge: %v", err)
	}

	tables := map[string]string{
		"events":            `SELECT count(*) FROM events WHERE room_id = $1`,
		"event_auth":        `SELECT count(*) FROM event_auth WHERE event_id = '$purge-1' AND $1 <> ''`,
		"account_data":      `SELECT count(*) FROM account_data WHERE room_id = $1`,
		"destination_rooms": `SELECT count(*) FROM destination_rooms WHERE room_id = $1`,
		"federation_outbox": `SELECT count(*) FROM federation_outbox WHERE room_id = $1`,
		"transactions":      `SELECT count(*) FROM transactions WHERE room_id = $1`,
		"receipts":          `SELECT count(*) FROM receipts WHERE room_id = $1`,
		"room_memberships":  `SELECT count(*) FROM room_memberships WHERE room_id = $1`,
		"room_state":        `SELECT count(*) FROM room_state WHERE room_id = $1`,
	}

	for name, query := range tables {
		if n := adminCount(t, db, query, "!r:t"); n != 0 {
			t.Errorf("после purge в %s осталось %d строк", name, n)
		}
	}

	// Соседняя комната не затронута.
	if n := adminCount(t, db, `SELECT count(*) FROM event_auth WHERE event_id = '$keep-1'`); n != 1 {
		t.Errorf("purge задел event_auth другой комнаты: %d", n)
	}

	if n := adminCount(t, db, `SELECT count(*) FROM federation_outbox WHERE room_id = '!keep:t'`); n != 1 {
		t.Errorf("purge задел federation_outbox другой комнаты: %d", n)
	}

	blocked, err := db.RoomBlocked(ctx, "!r:t")
	if err != nil || !blocked {
		t.Fatalf("блокировка после purge: blocked=%v err=%v, ожидали true", blocked, err)
	}

	if n := adminCount(t, db, `SELECT count(*) FROM admin_audit WHERE action = 'room_purge' AND target = '!r:t' AND details->>'result' = 'ok'`); n != 1 {
		t.Fatalf("аудит purge: %d записей", n)
	}

	// Повтор того же txn в пересозданной комнате: раньше duplicate key -> 500.
	adminExec(t, db, `INSERT INTO rooms (room_id, creator) VALUES ('!r:t', '@alice:t')`)

	ev.EventID = "$purge-2"

	written, dup, err := rooms.AppendEvent(ctx, ev, txn)
	if err != nil || dup || written.EventID != "$purge-2" {
		t.Fatalf("повтор txn после purge: dup=%v id=%s err=%v", dup, written.EventID, err)
	}

	// purge неизвестной комнаты - ErrNotFound.
	if err := db.AdminPurgeRoom(ctx, "@admin:t", "!nope:t", false, nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("purge неизвестной комнаты: %v", err)
	}
}

// TestAdminAppendAfterPurgeNotFound - запись события в удаленную комнату дает
// ErrNotFound, а не внутреннюю ошибку FK.
func TestAdminAppendAfterPurgeNotFound(t *testing.T) {
	db := freshAdminDB(t)
	ctx := context.Background()

	_, _, err := store.NewRoomStore(db).AppendEvent(ctx, core.Event{
		EventID: "$gone-1", RoomID: "!gone:t", Type: "m.room.message", Sender: "@alice:t",
		Content: json.RawMessage(`{"body":"x"}`), Depth: 1, OriginServerTS: 1, Origin: "t",
	}, nil)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("append в несуществующую комнату: %v, ожидали ErrNotFound", err)
	}
}
