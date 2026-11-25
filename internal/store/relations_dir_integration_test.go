//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// TestRelationsPaginationDir — dir=b листает назад эксклюзивным
// курсором от самых новых, dir=f — вперёд от самых старых; без дублей и пропусков.
func TestRelationsPaginationDir(t *testing.T) {
	_ = freshSyncStore(t)

	db := openDB(t)
	ctx := context.Background()

	const (
		roomID   = "!reldir:test.local"
		parentID = "$parent-dir:test.local"
		sender   = "@reldir:test.local"
		total    = 25
		page     = 10
	)

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO users (user_id, localpart, password_hash) VALUES ($1,'reldir','x')
		 ON CONFLICT (user_id) DO NOTHING`, sender); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO rooms (room_id, room_version, creator, is_public) VALUES ($1,'10',$2,true)`, roomID, sender); err != nil {
		t.Fatalf("insert room: %v", err)
	}

	seedMessage(t, db, roomID, parentID, sender, `{"msgtype":"m.text","body":"parent"}`)

	var firstEventID string

	for i := 0; i < total; i++ {
		eventID := fmt.Sprintf("$rel%02d:test.local", i)
		if i == 0 {
			firstEventID = eventID
		}

		seedMessage(t, db, roomID, eventID, sender,
			fmt.Sprintf(`{"m.relates_to":{"rel_type":"m.annotation","event_id":%q,"key":"%d"}}`, parentID, i))
	}

	roomStore := store.NewRoomStore(db)

	collect := func(dir string) []core.Event {
		var all []core.Event

		cursor := int64(0)

		for {
			events, err := roomStore.Relations(ctx, roomID, parentID, "", "", cursor, dir, page)
			if err != nil {
				t.Fatalf("relations dir=%s: %v", dir, err)
			}

			if len(events) == 0 {
				break
			}

			if dir == "b" {
				cursor = events[len(events)-1].StreamOrdering
			} else {
				cursor = events[len(events)-1].StreamOrdering
			}

			all = append(all, events...)

			if len(events) < page {
				break
			}
		}

		return all
	}

	back := collect("b")
	if len(back) != total {
		t.Fatalf("dir=b: собрано %d событий, ожидали %d", len(back), total)
	}

	seen := map[string]struct{}{}

	for i := range back {
		if _, dup := seen[back[i].EventID]; dup {
			t.Fatalf("dir=b: дубль %s", back[i].EventID)
		}

		seen[back[i].EventID] = struct{}{}

		if i > 0 && back[i].StreamOrdering >= back[i-1].StreamOrdering {
			t.Fatalf("dir=b: порядок не по убыванию на позиции %d", i)
		}
	}

	forward := collect("f")
	if len(forward) != total {
		t.Fatalf("dir=f: собрано %d событий, ожидали %d", len(forward), total)
	}

	if forward[0].EventID != firstEventID {
		t.Fatalf("dir=f: начинается с %s, ожидали %s", forward[0].EventID, firstEventID)
	}

	for i := 1; i < len(forward); i++ {
		if forward[i].StreamOrdering <= forward[i-1].StreamOrdering {
			t.Fatalf("dir=f: порядок не по возрастанию на позиции %d", i)
		}
	}
}
