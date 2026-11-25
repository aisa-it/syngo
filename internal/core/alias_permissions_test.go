package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestAliasPermissions - права на алиасы как в Synapse (Complement
// TestRoomDeleteAlias): алиас заводит любой участник, удаляет создатель алиаса или
// участник с правом на m.room.canonical_alias; удаленный алиас убирается из
// m.room.canonical_alias.
func TestAliasPermissions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, store := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")

	if _, err := svc.Join(ctx, bob, roomID); err != nil {
		t.Fatalf("bob join: %v", err)
	}

	// Участник без power levels заводит и удаляет свой алиас.
	if err := svc.SetAlias(ctx, bob, roomID, "#bob:test.local"); err != nil {
		t.Fatalf("SetAlias участником: %v", err)
	}

	if err := svc.DeleteAlias(ctx, bob, "#bob:test.local"); err != nil {
		t.Fatalf("DeleteAlias создателем алиаса: %v", err)
	}

	// Чужой алиас участник без прав удалить не может.
	if err := svc.SetAlias(ctx, alice, roomID, "#alice:test.local"); err != nil {
		t.Fatalf("SetAlias alice: %v", err)
	}

	if err := svc.DeleteAlias(ctx, bob, "#alice:test.local"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("DeleteAlias чужого алиаса без прав: %v, ожидали ErrForbidden", err)
	}

	// Создатель комнаты (PL 100) удаляет чужой алиас.
	if err := svc.SetAlias(ctx, bob, roomID, "#bob2:test.local"); err != nil {
		t.Fatalf("SetAlias bob2: %v", err)
	}

	if err := svc.DeleteAlias(ctx, alice, "#bob2:test.local"); err != nil {
		t.Fatalf("DeleteAlias с правом на canonical_alias: %v", err)
	}

	// Удаление canonical-алиаса чистит m.room.canonical_alias.
	content := json.RawMessage(`{"alias":"#alice:test.local","alt_aliases":["#alice:test.local"]}`)
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "", content); err != nil {
		t.Fatalf("canonical_alias: %v", err)
	}

	if err := svc.DeleteAlias(ctx, alice, "#alice:test.local"); err != nil {
		t.Fatalf("DeleteAlias canonical: %v", err)
	}

	events, err := store.StateEvents(ctx, roomID)
	if err != nil {
		t.Fatalf("StateEvents: %v", err)
	}

	for i := range events {
		if events[i].Type == "m.room.canonical_alias" && string(events[i].Content) != "{}" {
			t.Fatalf("canonical_alias после удаления алиаса = %s, ожидали {}", events[i].Content)
		}
	}
}
