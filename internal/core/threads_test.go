package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestThreads — список тредов только участнику; next_batch - позиция
// последнего ответа последнего треда полной страницы, на неполной не отдается;
// скрытые по видимости ответы не поднимают тред.
func TestThreads(t *testing.T) {
	t.Parallel()

	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)
	ctx := context.Background()

	send := func(txn, content string) string {
		t.Helper()

		id, _, err := svc.SendMessage(ctx, alice, "DEV1", roomID, "m.room.message", txn, json.RawMessage(content))
		if err != nil {
			t.Fatalf("SendMessage(%s): %v", txn, err)
		}

		return id
	}

	root := send("root", `{"body":"корень"}`)
	send("reply", `{"body":"ответ","m.relates_to":{"rel_type":"m.thread","event_id":"`+root+`"}}`)

	if _, _, err := svc.Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: bob, Limit: 1}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("посторонний: %v, ожидали ErrForbidden", err)
	}

	events, next, err := svc.Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: alice, Limit: 1})
	if err != nil || len(events) != 1 || events[0].EventID != root || next == "" {
		t.Fatalf("Threads(limit=1) = %v, %q, %v", events, next, err)
	}

	if _, next, _ = svc.Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: alice, Limit: 5}); next != "" {
		t.Fatalf("next_batch на неполной странице: %q", next)
	}

	// Видимо только начало истории: корень и ответ скрыты.
	events, _, err = svc.Threads(ctx, core.ThreadsQuery{RoomID: roomID, UserID: alice, Limit: 5, VisibleBefore: 1, VisibleFrom: 1 << 62})
	if err != nil || len(events) != 0 {
		t.Fatalf("скрытый тред в списке: %v, %v", events, err)
	}
}
