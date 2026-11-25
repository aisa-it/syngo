//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

const (
	summaryRoom = "!summary:test.local"
	summaryBob  = "@bob:test.local"
)

// seedSummaryRoom - комната, пользователь bob (участник) и сообщения ids.
// Возвращает позиции событий.
func seedSummaryRoom(t *testing.T, db *store.DB, ids ...string) map[string]int64 {
	t.Helper()

	ctx := context.Background()

	for _, q := range []string{
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ('@bob:test.local','bob','x')`,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ('!summary:test.local','10','@bob:test.local',true)`,
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		 VALUES ('$join','!summary:test.local','m.room.member','@bob:test.local','@bob:test.local',
		         '{"membership":"join"}',1,1700000000000,'test.local')`,
		`INSERT INTO room_memberships (room_id, user_id, membership, event_id, sender) VALUES ('!summary:test.local','@bob:test.local','join','$join','@bob:test.local')`,
	} {
		if _, err := db.Pool().Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	out := map[string]int64{}

	for _, id := range ids {
		seedMessage(t, db, summaryRoom, id, "@alice:test.local", `{"msgtype":"m.text","body":"x"}`)

		var stream int64
		if err := db.Pool().QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = $1`, id).Scan(&stream); err != nil {
			t.Fatal(err)
		}

		out[id] = stream
	}

	return out
}

func summaryCounts(t *testing.T, db *store.DB) map[string]core.PushCounts {
	t.Helper()

	counts, err := db.PushCountsFor(context.Background(), summaryBob, []string{summaryRoom})
	if err != nil {
		t.Fatalf("PushCountsFor: %v", err)
	}

	return counts[summaryRoom]
}

func pushRows(t *testing.T, db *store.DB) int {
	t.Helper()

	var n int
	if err := db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM event_push_actions WHERE user_id = '@bob:test.local'`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

// Сводка event_push_summary после серии сообщений, receipts
// основной ленты, треда и без треда, своего события, redaction и выхода;
// фоновая чистка удаляет только прочитанные строки, счетчики не меняются.
func TestPushSummaryLifecycle(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	pos := seedSummaryRoom(t, db, "$s1", "$s2", "$s3", "$s4", "$s5", "$s6")
	act := func(id, thread string, highlight bool) core.PushAction {
		return core.PushAction{UserID: summaryBob, RoomID: summaryRoom, EventID: id, Stream: pos[id], ThreadID: thread, Highlight: highlight}
	}

	batch := core.PushBatch{Actions: []core.PushAction{
		act("$s1", "", false), act("$s2", "", true), act("$s3", "$s1", false),
		act("$s4", "", false), act("$s5", "$s1", true),
	}, Processed: pos["$s5"]}

	for range 2 { // повтор порции счетчики не увеличивает
		if err := db.ApplyPushBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}

	want := func(step string, main, thread core.PushCounts) {
		t.Helper()

		got := summaryCounts(t, db)
		if got[""] != main || got["$s1"] != thread {
			t.Fatalf("%s: %v, ожидали основная %v, тред %v", step, got, main, thread)
		}
	}

	want("после порции", core.PushCounts{Notification: 3, Highlight: 1}, core.PushCounts{Notification: 2, Highlight: 1})

	// Receipt основной ленты: только она.
	if err := db.MarkPushRead(ctx, summaryBob, summaryRoom, "", "$s2", false, false); err != nil {
		t.Fatal(err)
	}

	want("receipt основной ленты", core.PushCounts{Notification: 1}, core.PushCounts{Notification: 2, Highlight: 1})

	// Receipt без треда: все ленты до $s3; повтор и более старый - без изменений.
	for _, id := range []string{"$s3", "$s3", "$s1"} {
		if err := db.MarkPushRead(ctx, summaryBob, summaryRoom, "", id, true, false); err != nil {
			t.Fatal(err)
		}
	}

	want("receipt без треда", core.PushCounts{Notification: 1}, core.PushCounts{Notification: 1, Highlight: 1})

	if err := db.PrunePushActions(ctx, []core.PushReadUpTo{{UserID: summaryBob, RoomID: summaryRoom}}); err != nil {
		t.Fatal(err)
	}

	if n := pushRows(t, db); n != 2 {
		t.Fatalf("после чистки строк %d, ожидали 2 непрочитанные", n)
	}

	want("после чистки", core.PushCounts{Notification: 1}, core.PushCounts{Notification: 1, Highlight: 1})

	// Свое событие в треде - прочтение треда; redaction непрочитанного.
	if _, err := db.Pool().Exec(ctx, `UPDATE events SET redacted_by = '$red' WHERE event_id = '$s4'`); err != nil {
		t.Fatal(err)
	}

	if err := db.ApplyPushBatch(ctx, core.PushBatch{
		Actions:    []core.PushAction{act("$s6", "", false)},
		ReadUpTo:   []core.PushReadUpTo{{UserID: summaryBob, RoomID: summaryRoom, ThreadID: "$s1", Stream: pos["$s5"]}},
		RedactedBy: []string{"$red"},
		Processed:  pos["$s6"],
	}); err != nil {
		t.Fatal(err)
	}

	want("свое событие и redaction", core.PushCounts{Notification: 1}, core.PushCounts{})

	if n, err := db.PushUnreadRooms(ctx, summaryBob); err != nil || n != 1 {
		t.Fatalf("бейдж: %d %v", n, err)
	}

	if err := db.ApplyPushBatch(ctx, core.PushBatch{
		Left: []core.PushReadUpTo{{UserID: summaryBob, RoomID: summaryRoom}}, Processed: pos["$s6"],
	}); err != nil {
		t.Fatal(err)
	}

	if got := summaryCounts(t, db); len(got) != 0 || pushRows(t, db) != 0 {
		t.Fatalf("после выхода: %v, строк %d", got, pushRows(t, db))
	}
}

// Счетчики /sync - один запрос к сводке, он не читает строки
// действий: 1000 непрочитанных отдаются и после удаления всех строк, и запрос
// по-прежнему один.
func TestPushCountsDoNotScanActions(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	_ = seedSummaryRoom(t, db)

	const n = 1000

	actions := make([]core.PushAction, 0, n)
	for i := range n {
		actions = append(actions, core.PushAction{
			UserID: summaryBob, RoomID: summaryRoom, EventID: fmt.Sprintf("$u%d", i), Stream: int64(100 + i), Highlight: i%10 == 0,
		})
	}

	if err := db.ApplyPushBatch(ctx, core.PushBatch{Actions: actions, Processed: 100 + n}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Pool().Exec(ctx, `DELETE FROM event_push_actions`); err != nil {
		t.Fatal(err)
	}

	var recorded []recordedQuery

	counts, err := db.PushCountsFor(context.WithValue(ctx, recordKey{}, &recorded), summaryBob, []string{summaryRoom, "!other:test.local"})
	if err != nil {
		t.Fatal(err)
	}

	if counts[summaryRoom][""] != (core.PushCounts{Notification: n, Highlight: n / 10}) || len(counts) != 1 {
		t.Fatalf("счетчики: %v", counts)
	}

	if len(recorded) != 1 || strings.Contains(recorded[0].SQL, "event_push_actions") {
		t.Fatalf("запросы счетчиков: %+v", recorded)
	}
}

// summaryFixture - комната alice и bob через RoomService (настоящие события,
// членство и состояние для push-правил).
type summaryFixture struct {
	db     *store.DB
	rooms  *core.RoomService
	roomID string
}

func newSummaryFixture(t *testing.T) summaryFixture {
	t.Helper()

	_ = freshRoomStore(t)
	db := openDB(t)
	ctx := context.Background()

	for _, u := range []string{"alice", "bob"} {
		if _, err := db.Pool().Exec(ctx, `INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,$2,'x')`,
			"@"+u+":test.local", u); err != nil {
			t.Fatal(err)
		}
	}

	rooms := core.NewRoomService(store.NewRoomStore(db), "test.local")

	roomID, err := rooms.CreateRoom(ctx, core.CreateRoomRequest{Creator: redactAlice, Preset: "public_chat"})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if _, err := rooms.Join(ctx, redactBob, roomID); err != nil {
		t.Fatalf("Join: %v", err)
	}

	return summaryFixture{db: db, rooms: rooms, roomID: roomID}
}

// Гонка receipts и порций разбора на настоящей БД. alice пишет в
// основную ленту и тред (часть - с упоминанием bob), bob иногда отвечает сам и
// параллельно шлет receipts (без треда, основной ленты, треда) на только что
// отправленные события, разбор идет в фоне. Итоговая сводка совпадает со
// счетом по событиям и отметкам, а после чистки строк остаются только
// непрочитанные.
func TestPushSummaryReceiptRace(t *testing.T) {
	f := newSummaryFixture(t)
	ctx := context.Background()

	svc := core.NewPushActionsService(f.db, core.PushActionsOptions{
		Members: store.NewSyncStore(f.db), SyncWait: 5 * time.Second,
		Warn: func(_ context.Context, msg string, err error, _ ...any) {
			t.Errorf("%s: %v", msg, err)
		},
	})
	t.Cleanup(func() { _ = svc.Close(context.Background()) })

	if err := svc.CatchUpNow(ctx); err != nil {
		t.Fatal(err)
	}

	type sent struct {
		id, sender, thread string
		stream             int64
		mention            bool
	}

	var (
		mu   sync.Mutex
		msgs []sent
	)

	send := func(sender, txn, thread string, mention bool) sent {
		content := map[string]any{"msgtype": "m.text", "body": txn}
		if thread != "" {
			content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": thread}
		}

		if mention {
			content["m.mentions"] = map[string]any{"user_ids": []string{redactBob}}
		}

		raw, _ := json.Marshal(content)

		id, _, err := f.rooms.SendMessage(ctx, sender, "DEV", f.roomID, "m.room.message", txn, raw)
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}

		var stream int64
		if err := f.db.Pool().QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = $1`, id).Scan(&stream); err != nil {
			t.Fatal(err)
		}

		s := sent{id: id, sender: sender, thread: thread, stream: stream, mention: mention}

		mu.Lock()
		msgs = append(msgs, s)
		mu.Unlock()
		svc.EventsWritten(ctx)

		return s
	}

	root := send(redactAlice, "root", "", false)

	// Отметки bob: без треда, основная лента и треды.
	marks := map[string]int64{}

	var wg sync.WaitGroup

	stop := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()

		rnd := rand.New(rand.NewPCG(1, 2))
		// Receipts отстают от последнего события, но по каждой ленте не идут назад.
		sentUpTo := map[string]int64{}

		for {
			select {
			case <-stop:
				return
			default:
			}

			threadID := ""
			switch rnd.IntN(3) {
			case 1:
				threadID = "main"
			case 2:
				threadID = root.id
			}

			mu.Lock()
			last := msgs[len(msgs)-1-rnd.IntN(min(len(msgs), 8))]
			mu.Unlock()

			if last.stream <= sentUpTo[threadID] {
				continue
			}

			sentUpTo[threadID] = last.stream

			r := core.ReceiptEvent{
				RoomID: f.roomID, UserID: redactBob, EventID: last.id, Type: core.ReceiptRead,
				ThreadID: threadID, TS: time.Now().UnixMilli(),
			}
			if err := f.db.UpsertReceipt(ctx, r); err != nil {
				t.Error(err)

				return
			}

			if err := svc.ApplyReceipt(ctx, r); err != nil {
				t.Error(err)

				return
			}

			time.Sleep(time.Duration(rnd.IntN(3)) * time.Millisecond)
		}
	}()

	for i := range 120 {
		thread := ""
		if i%3 == 0 {
			thread = root.id
		}

		if i%17 == 5 {
			send(redactBob, fmt.Sprintf("b%d", i), thread, false)

			continue
		}

		send(redactAlice, fmt.Sprintf("a%d", i), thread, i%5 == 0)
	}

	close(stop)
	wg.Wait()

	if err := svc.CatchUpNow(ctx); err != nil {
		t.Fatal(err)
	}

	// Итоговые отметки bob - из таблицы receipts (последняя по каждому треду).
	rows, err := f.db.Pool().Query(ctx, `
		SELECT thread_id, event_stream FROM receipts
		WHERE user_id = $1 AND room_id = $2 AND receipt_type = 'm.read'`, redactBob, f.roomID)
	if err != nil {
		t.Fatal(err)
	}

	for rows.Next() {
		var (
			thread string
			stream int64
		)

		if err := rows.Scan(&thread, &stream); err != nil {
			t.Fatal(err)
		}

		marks[thread] = stream
	}

	rows.Close()

	// Свои события bob - неявная отметка своей ленты.
	own := map[string]int64{}

	for _, m := range msgs {
		if m.sender == redactBob {
			own[m.thread] = max(own[m.thread], m.stream)
		}
	}

	want := map[string]core.PushCounts{}

	for _, m := range msgs {
		if m.sender != redactAlice {
			continue
		}

		read := max(marks[""], own[m.thread])
		if m.thread == "" {
			read = max(read, marks["main"])
		} else {
			read = max(read, marks[m.thread])
		}

		if m.stream <= read {
			continue
		}

		c := want[m.thread]
		c.Notification++

		if m.mention {
			c.Highlight++
		}

		want[m.thread] = c
	}

	counts, ok := svc.UnreadCounts(ctx, redactBob, msgs[len(msgs)-1].stream, []string{f.roomID})
	if !ok {
		t.Fatal("UnreadCounts не отдал счетчики")
	}

	for _, thread := range []string{"", root.id} {
		if counts[f.roomID][thread] != want[thread] {
			t.Fatalf("тред %q: сводка %v, по событиям %v (отметки %v, свои %v)",
				thread, counts[f.roomID][thread], want[thread], marks, own)
		}
	}

	t.Logf("непрочитано %v, отметки %v, свои %v", want, marks, own)

	if err := svc.PruneNow(ctx); err != nil {
		t.Fatal(err)
	}

	unread := want[""].Notification + want[root.id].Notification
	if n := pushRows(t, f.db); n != unread {
		t.Fatalf("после чистки строк %d, непрочитанных %d", n, unread)
	}
}
