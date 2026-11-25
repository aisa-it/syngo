package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestCanonicalAliasValidation — canonical_alias принимает только алиасы
// этой комнаты; несуществующие и чужие - ErrBadAlias (400 M_BAD_ALIAS, как в
// Synapse и Complement TestRoomCanonicalAlias), невалидные - ErrInvalidAliasContent
// (400 M_INVALID_PARAM).
func TestCanonicalAliasValidation(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)
	roomID := createRoomWithJoinRules(t, svc, "public")

	ctx := context.Background()

	// Алиас комнаты.
	if err := store.CreateAlias(ctx, "#room:test.local", roomID, alice); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}

	// Валидный canonical_alias + alt_aliases.
	content := json.RawMessage(`{"alias":"#room:test.local","alt_aliases":["#room:test.local"]}`)
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "ca1", content); err != nil {
		t.Fatalf("валидный canonical_alias: %v", err)
	}

	// Несуществующий алиас — ErrBadAlias.
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "ca2",
		json.RawMessage(`{"alias":"#missing:test.local"}`)); !errors.Is(err, core.ErrBadAlias) {
		t.Fatalf("несуществующий: %v", err)
	}

	// Алиас другой комнаты — ErrBadAlias.
	otherRoom := createRoomWithJoinRules(t, svc, "public")
	if err := store.CreateAlias(ctx, "#other:test.local", otherRoom, alice); err != nil {
		t.Fatalf("CreateAlias other: %v", err)
	}

	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "ca3",
		json.RawMessage(`{"alias":"#other:test.local"}`)); !errors.Is(err, core.ErrBadAlias) {
		t.Fatalf("чужая комната: %v", err)
	}

	// Невалидный синтаксис — ErrInvalidAliasContent.
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "ca4",
		json.RawMessage(`{"alias":"not-an-alias"}`)); !errors.Is(err, core.ErrInvalidAliasContent) {
		t.Fatalf("невалидный: %v", err)
	}

	// alt_aliases тоже проверяются.
	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "m.room.canonical_alias", "", "ca5",
		json.RawMessage(`{"alias":"#room:test.local","alt_aliases":["#other:test.local"]}`)); !errors.Is(err, core.ErrBadAlias) {
		t.Fatalf("alt_aliases чужой комнаты: %v", err)
	}
}
