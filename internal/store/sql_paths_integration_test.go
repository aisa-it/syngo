//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/aisa-it/syngo/internal/store"
)

// seedMessage пишет m.room.message с заданным content и возвращает событие.
func seedMessage(t *testing.T, db *store.DB, roomID, eventID, sender, content string) {
	t.Helper()

	if _, err := db.Pool().Exec(context.Background(),
		`INSERT INTO events (event_id, room_id, type, sender, content, depth, origin_server_ts, origin)
		 VALUES ($1,$2,'m.room.message',$3,$4,1,1700000000000,'test.local')`,
		eventID, roomID, sender, content); err != nil {
		t.Fatalf("insert event %s: %v", eventID, err)
	}
}

// TestRelationsSQL — /relations через настоящий SQL. Раньше запрос
// выбирал 12 колонок, а scanEventRows ждал 13 — любой непустой результат падал.
func TestRelationsSQL(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	const (
		roomID   = "!rel:test.local"
		parentID = "$parent:test.local"
		sender   = "@alice:test.local"
	)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,'reluser','x')
		 ON CONFLICT (user_id) DO NOTHING`, sender); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ($1,'10',$2,true)`, roomID, sender); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	seedMessage(t, db, roomID, parentID, sender, `{"msgtype":"m.text","body":"parent"}`)

	for i, rel := range []string{
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"$parent:test.local","key":"1"}}`,
		`{"m.relates_to":{"rel_type":"m.annotation","event_id":"$parent:test.local","key":"2"}}`,
		`{"m.relates_to":{"rel_type":"m.thread","event_id":"$parent:test.local"}}`,
	} {
		seedMessage(t, db, roomID, "$rel"+string(rune('a'+i))+":test.local", sender, rel)
	}

	roomStore := store.NewRoomStore(db)

	relations, err := roomStore.Relations(ctx, roomID, parentID, "", "", 0, "f", 10)
	if err != nil {
		t.Fatalf("Relations() error: %v", err)
	}

	if len(relations) != 3 {
		t.Fatalf("связей %d, ожидали 3", len(relations))
	}

	// Пагинация: первая страница из двух и продолжение после курсора.
	first, err := roomStore.Relations(ctx, roomID, parentID, "", "", 0, "f", 2)
	if err != nil {
		t.Fatalf("Relations() page 1: %v", err)
	}

	if len(first) != 2 {
		t.Fatalf("страница 1: %d событий, ожидали 2", len(first))
	}

	// Курсор эксклюзивный: берём позицию последнего события страницы.
	next := first[len(first)-1].StreamOrdering

	second, err := roomStore.Relations(ctx, roomID, parentID, "", "", next, "f", 10)
	if err != nil {
		t.Fatalf("Relations() page 2: %v", err)
	}

	if len(second) != 1 {
		t.Fatalf("страница 2: %d событий, ожидали 1", len(second))
	}

	if second[0].StreamOrdering <= first[len(first)-1].StreamOrdering {
		t.Fatal("вторая страница пересекается с первой")
	}
}

// TestAvatarURLAndJoinedMembersSQL — UPDATE users.updated_at не
// существовал (PUT avatar_url всегда 500), а /joined_members читал display_name
// вместо displayname (имена пустые).
func TestAvatarURLAndJoinedMembersSQL(t *testing.T) {
	_ = freshSyncStore(t)
	db := openDB(t)
	ctx := context.Background()

	const (
		roomID = "!members:test.local"
		userID = "@bob:test.local"
	)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,'bob','x')`, userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	profiles := store.NewProfileStore(db)

	if err := profiles.UpdateAvatarURL(ctx, userID, "mxc://test.local/avatar"); err != nil {
		t.Fatalf("UpdateAvatarURL() error: %v", err)
	}

	var avatar string

	if err := db.Pool().QueryRow(ctx,
		`SELECT coalesce(avatar_url,'') FROM users WHERE user_id = $1`, userID).Scan(&avatar); err != nil {
		t.Fatalf("select avatar: %v", err)
	}

	if avatar != "mxc://test.local/avatar" {
		t.Fatalf("avatar_url = %q", avatar)
	}

	// joined_members: имя берётся из content.displayname.
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ($1,'10',$2,true)`, roomID, userID); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO events (event_id, room_id, type, sender, state_key, content, depth, origin_server_ts, origin)
		 VALUES ('$member:test.local',$1,'m.room.member',$2,$2,'{"membership":"join","displayname":"Боб"}',1,1700000000000,'test.local')`,
		roomID, userID); err != nil {
		t.Fatalf("insert member event: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO room_state (room_id, type, state_key, event_id)
		 VALUES ($1,'m.room.member',$2,'$member:test.local')`, roomID, userID); err != nil {
		t.Fatalf("insert room_state: %v", err)
	}

	joined, err := store.NewRoomStore(db).JoinedMembers(ctx, roomID)
	if err != nil {
		t.Fatalf("JoinedMembers() error: %v", err)
	}

	profile, ok := joined[userID]
	if !ok {
		t.Fatalf("участник не найден: %+v", joined)
	}

	if profile.DisplayName != "Боб" {
		t.Fatalf("display_name = %q, ожидали «Боб»", profile.DisplayName)
	}
}
