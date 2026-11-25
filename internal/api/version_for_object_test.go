package api

import (
	"context"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestVersionForObjectInvite — версия комнаты для входящего приглашения берётся
// из unsigned.invite_room_state (m.room.create), иначе invite v11-комнаты
// проверялся бы по правилам v10 и подпись была бы «invalid».
func TestVersionForObjectInvite(t *testing.T) {
	t.Parallel()

	srv := &Server{}

	invite := map[string]any{
		"type":    "m.room.member",
		"room_id": "!unknown:remote.test",
		"content": map[string]any{"membership": "invite"},
		"unsigned": map[string]any{"invite_room_state": []any{
			map[string]any{"type": "m.room.create", "content": map[string]any{"room_version": "11"}},
		}},
	}

	if got := srv.versionForObject(context.Background(), invite); got != 11 {
		t.Fatalf("версия приглашения = %d, ожидали 11", got)
	}

	noState := map[string]any{"type": "m.room.member", "room_id": "!unknown2:remote.test"}
	if got := srv.versionForObject(context.Background(), noState); got != core.DefaultRoomVersion {
		t.Fatalf("версия без invite_room_state = %d, ожидали %d", got, core.DefaultRoomVersion)
	}
}
