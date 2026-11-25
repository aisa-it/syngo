package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestEventValidation — тип с любым пространством имен, content-объект, лимит размера.
func TestEventValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newRoomService(t)
	roomID := createRoom(t, svc)

	if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "im.vector.modular.widgets", "w1", "",
		json.RawMessage(`{"type":"jitsi"}`)); err != nil {
		t.Fatalf("виджет: %v", err)
	}

	if _, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "com.example.bot", "b1", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatalf("кастомный тип: %v", err)
	}

	for _, bad := range []string{`[1,2]`, `"str"`, `42`, `null`} {
		if _, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "bad"+bad, json.RawMessage(bad)); !errors.Is(err, core.ErrBadJSON) {
			t.Fatalf("content %s: %v, ожидали ErrBadJSON", bad, err)
		}
	}

	// Канонический JSON спецификации: дробные и большие числа, одиночный суррогат -
	// 400 M_BAD_JSON, как у Synapse для комнат v6+.
	for _, bad := range []string{`{"x":1.5}`, `{"x":9007199254740992}`, `{"body":"\ud800"}`} {
		if _, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "nc"+bad, json.RawMessage(bad)); !errors.Is(err, core.ErrBadJSON) {
			t.Fatalf("content %s: %v, ожидали ErrBadJSON", bad, err)
		}

		if _, _, err := svc.SendState(ctx, alice, "DEV", roomID, "com.example.state", "k", "", json.RawMessage(bad)); !errors.Is(err, core.ErrBadJSON) {
			t.Fatalf("state %s: %v, ожидали ErrBadJSON", bad, err)
		}
	}

	if _, err := svc.CreateRoom(ctx, core.CreateRoomRequest{
		Creator:        alice,
		CreationExtras: map[string]any{"x": 0.5},
	}); !errors.Is(err, core.ErrBadJSON) {
		t.Fatalf("creation_content с дробным числом: %v, ожидали ErrBadJSON", err)
	}

	big := `{"body":"` + strings.Repeat("x", 70000) + `"}`
	if _, _, err := svc.SendMessage(ctx, alice, "DEV", roomID, "m.room.message", "big", json.RawMessage(big)); !errors.Is(err, core.ErrTooLarge) {
		t.Fatalf("событие 70 КБ: %v, ожидали ErrTooLarge", err)
	}
}
