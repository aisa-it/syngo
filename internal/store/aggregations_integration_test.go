//go:build integration

package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// TestBundledAggregationsSQL — агрегации через настоящий SQL. Правка
// действует только того же отправителя и типа, у незашифрованной нужен
// m.new_content; последняя - по origin_server_ts, при равенстве - больший
// event_id; удаленные и скрытые по видимости связи не учитываются; правки
// событий состояния нет.
func TestBundledAggregationsSQL(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	const (
		roomID = "!agg:test.local"
		alice  = "@aggalice:test.local"
		bob    = "@aggbob:test.local"
	)

	for _, user := range []string{alice, bob} {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,$2,'x') ON CONFLICT (user_id) DO NOTHING`,
			user, strings.TrimPrefix(strings.Split(user, ":")[0], "@")); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ($1,'10',$2,true)`, roomID, alice); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	insert := func(id, typ, sender string, stateKey *string, ts int64, content string) int64 {
		t.Helper()

		var pos int64
		if err := db.Pool().QueryRow(ctx,
			`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
			 VALUES ($1,$2,$3,$4,$5,$6,1,$7,'test.local') RETURNING stream_ordering`,
			id, roomID, typ, sender, stateKey, content, ts).Scan(&pos); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}

		return pos
	}

	edit := func(target, body string) string {
		return fmt.Sprintf(`{"m.new_content":{"body":%q},"m.relates_to":{"rel_type":"m.replace","event_id":%q}}`, body, target)
	}

	empty := ""

	insert("$p", "m.room.message", alice, nil, 1, `{"body":"p"}`)
	insert("$e1", "m.room.message", alice, nil, 100, edit("$p", "1"))
	insert("$e2", "m.room.message", alice, nil, 200, edit("$p", "2"))
	insert("$e3", "m.room.message", alice, nil, 200, edit("$p", "3"))
	insert("$e4", "m.room.message", bob, nil, 300, edit("$p", "чужая"))
	insert("$e5", "m.sticker", alice, nil, 400, edit("$p", "другой тип"))
	insert("$e6", "m.room.message", alice, nil, 500, `{"m.relates_to":{"rel_type":"m.replace","event_id":"$p"}}`)
	insert("$q", "m.room.encrypted", alice, nil, 1, `{"ciphertext":"x"}`)
	insert("$f1", "m.room.encrypted", alice, nil, 100, `{"ciphertext":"y","m.relates_to":{"rel_type":"m.replace","event_id":"$q"}}`)
	insert("$s", "m.room.topic", alice, &empty, 1, `{"topic":"t"}`)
	insert("$s1", "m.room.topic", alice, nil, 100, edit("$s", "тема"))
	t1 := insert("$t1", "m.room.message", bob, nil, 10, `{"body":"t1","m.relates_to":{"rel_type":"m.thread","event_id":"$p"}}`)
	insert("$t2", "m.room.message", alice, nil, 11, `{"body":"t2","m.relates_to":{"rel_type":"m.thread","event_id":"$p"}}`)
	insert("$r1", "m.room.message", bob, nil, 12, `{"m.relates_to":{"rel_type":"m.reference","event_id":"$p"}}`)
	insert("$r2", "m.room.message", bob, nil, 13, `{"m.relates_to":{"rel_type":"m.reference","event_id":"$p"}}`)

	if _, err := db.Pool().Exec(ctx, `UPDATE events SET redacted_by = '$red' WHERE event_id = '$t2'`); err != nil {
		t.Fatalf("redact: %v", err)
	}

	aggs, err := db.BundledAggregations(ctx, bob, []string{"$p", "$q", "$s"}, nil)
	if err != nil {
		t.Fatalf("BundledAggregations: %v", err)
	}

	if aggs.Edits["$p"] != "$e3" || aggs.Edits["$q"] != "$f1" || aggs.Edits["$s"] != "" {
		t.Fatalf("правки: %v", aggs.Edits)
	}

	if th := aggs.Threads["$p"]; th.Count != 1 || th.LatestEventID != "$t1" || !th.Participated {
		t.Fatalf("тред: %+v", th)
	}

	if fmt.Sprint(aggs.References["$p"]) != "[$r1 $r2]" {
		t.Fatalf("ссылки: %v", aggs.References)
	}

	if ev := aggs.Events["$e3"]; ev.Sender != alice || ev.Type != "m.room.message" || len(ev.Content) == 0 ||
		aggs.Events["$t1"].StreamOrdering != t1 {
		t.Fatalf("события агрегаций: %+v", aggs.Events)
	}

	if _, ok := aggs.Events["$r1"]; ok {
		t.Fatalf("ссылки подгружены целиком: %+v", aggs.Events["$r1"])
	}

	// Ответ из скрытого промежутка не считается.
	hidden, err := db.BundledAggregations(ctx, bob, []string{"$p"},
		map[string]store.Visibility{roomID: {Before: t1, From: 1 << 62}})
	if err != nil {
		t.Fatalf("BundledAggregations(vis): %v", err)
	}

	if _, ok := hidden.Threads["$p"]; ok || len(hidden.References["$p"]) != 0 {
		t.Fatalf("скрытые связи учтены: %+v", hidden)
	}

	if empty, err := db.BundledAggregations(ctx, bob, nil, nil); err != nil || len(empty.Threads) != 0 {
		t.Fatalf("пустой список: %+v %v", empty, err)
	}

	// Список тредов: удаленный ответ не поднимает тред, participated - по ответам.
	threads, latest, err := store.NewRoomStore(db).Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: bob, Participated: true})
	if err != nil || len(threads) != 1 || threads[0].EventID != "$p" || latest[0] != t1 {
		t.Fatalf("Threads: %v %v %v", threads, latest, err)
	}

	threads, _, err = store.NewRoomStore(db).Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: bob, From: t1})
	if err != nil || len(threads) != 0 {
		t.Fatalf("Threads после курсора: %v %v", threads, err)
	}
}
