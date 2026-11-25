package core_test

import (
	"context"
	"slices"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/state"
)

func message(id, sender string, depth int64, prev []string, auth ...string) state.Event {
	return state.Event{
		ID: id, Type: "m.room.message", Sender: sender, Content: []byte(`{"body":"x"}`),
		Auth: auth, Prev: prev, Depth: depth, OriginTS: depth,
	}
}

// applyNew записывает события и применяет их с опциями (как прием /send).
func applyNew(t *testing.T, room *memRoomState, opts core.ApplyOptions, events ...state.Event) core.RoomStateResult {
	t.Helper()

	for i := range events {
		room.events[events[i].ID] = events[i]
	}

	result, err := core.ApplyRoomEvents(context.Background(), room, "!room:hs1", events, opts)
	if err != nil {
		t.Fatalf("ApplyRoomEvents: %v", err)
	}

	return result
}

// Забаненный пишет сообщение из ветки до бана: по состоянию до события он в
// комнате (событие не отклонено), по текущему состоянию - забанен: soft-fail.
// Состояние после события записано, extremities и room_state не меняются
// (Synapse _check_for_soft_fail).
func TestSoftFailBannedSender(t *testing.T) {
	t.Parallel()

	room := newRoom("public",
		member("$bobjoin", sBob, sBob, "join", 5, 5, []string{"$jr"}, "$create", "$pl", "$jr"),
		member("$bobban", sAlice, sBob, "ban", 6, 6, []string{"$bobjoin"}, "$create", "$pl", "$alice", "$bobjoin"))

	msg := message("$spam", sBob, 6, []string{"$bobjoin"}, "$create", "$pl", "$bobjoin")
	result := applyNew(t, room, core.ApplyOptions{SoftFail: true}, msg)

	if !slices.Equal(result.SoftFailed, []string{"$spam"}) || len(result.Rejected) != 0 {
		t.Fatalf("soft-failed %v, rejected %v; ожидали soft-fail $spam", result.SoftFailed, result.Rejected)
	}

	if !slices.Equal(room.extremities, []string{"$bobban"}) {
		t.Fatalf("extremities %v, ожидали [$bobban]", room.extremities)
	}

	if _, ok := room.after["$spam"]; !ok || room.rejected["$spam"] {
		t.Fatal("состояние после soft-failed события не записано или оно помечено отклоненным")
	}

	// Без проверки soft-fail (догон истории) то же событие принимается.
	room2 := newRoom("public",
		member("$bobjoin", sBob, sBob, "join", 5, 5, []string{"$jr"}, "$create", "$pl", "$jr"),
		member("$bobban", sAlice, sBob, "ban", 6, 6, []string{"$bobjoin"}, "$create", "$pl", "$alice", "$bobjoin"))

	if result := applyNew(t, room2, core.ApplyOptions{}, msg); len(result.SoftFailed) != 0 || len(room2.extremities) != 2 {
		t.Fatalf("без SoftFail: soft-failed %v, extremities %v", result.SoftFailed, room2.extremities)
	}
}

// prev_events равны extremities - текущее состояние совпадает с состоянием до
// события, принятое событие не soft-failed.
func TestSoftFailSkippedOnCurrentPrevs(t *testing.T) {
	t.Parallel()

	room := newRoom("public", member("$bobjoin", sBob, sBob, "join", 5, 5, []string{"$jr"}, "$create", "$pl", "$jr"))

	result := applyNew(t, room, core.ApplyOptions{SoftFail: true},
		message("$hi", sBob, 6, []string{"$bobjoin"}, "$create", "$pl", "$bobjoin"))

	if len(result.SoftFailed) != 0 || !slices.Equal(room.extremities, []string{"$hi"}) {
		t.Fatalf("soft-failed %v, extremities %v", result.SoftFailed, room.extremities)
	}
}

// prev, которого у нас нет: состояние после него - из PrevStates (получено по
// /state_ids), событие проверяется по нему.
func TestPrevStatesForMissingPrev(t *testing.T) {
	t.Parallel()

	room := newRoom("public")

	// У отправителя: bob вошел ($bobjoin у нас нет), затем написал.
	remote := state.StateMap{}
	for k, v := range room.current {
		remote[k] = v
	}

	room.events["$bobjoin"] = member("$bobjoin", sBob, sBob, "join", 5, 5, []string{"$jr"}, "$create", "$pl", "$jr")
	remote[keyBob] = "$bobjoin"

	msg := message("$late", sBob, 6, []string{"$bobjoin"}, "$create", "$pl", "$bobjoin")

	// Без состояния prev опора - текущее состояние: bob не в комнате, отказ.
	room2 := newRoom("public")
	room2.events["$bobjoin"] = room.events["$bobjoin"]

	if result := applyNew(t, room2, core.ApplyOptions{}, msg); !slices.Equal(result.Rejected, []string{"$late"}) {
		t.Fatalf("без PrevStates: rejected %v, ожидали [$late]", result.Rejected)
	}

	result := applyNew(t, room, core.ApplyOptions{PrevStates: map[string]state.StateMap{"$bobjoin": remote}, SoftFail: true}, msg)
	if len(result.Rejected) != 0 || len(result.SoftFailed) != 0 {
		t.Fatalf("с PrevStates: rejected %v, soft-failed %v", result.Rejected, result.SoftFailed)
	}

	if room.after["$late"][keyBob] != "$bobjoin" {
		t.Fatalf("состояние после $late %v, ожидали join bob", room.after["$late"])
	}
}

// Отклоненное заранее (по своим auth_events) событие: состояние до него
// записано, оно помечено отклоненным и в состояние не попадает.
func TestPreRejectedEvent(t *testing.T) {
	t.Parallel()

	room := newRoom("public")
	topic := state.Event{
		ID: "$topic", Type: "m.room.topic", StateKey: strPtr(""), Sender: sAlice,
		Content: []byte(`{"topic":"t"}`), Auth: []string{"$create", "$pl", "$alice"}, Prev: []string{"$jr"}, Depth: 5, OriginTS: 5,
	}

	result := applyNew(t, room, core.ApplyOptions{Rejected: map[string]struct{}{"$topic": {}}}, topic)

	if !slices.Equal(result.Rejected, []string{"$topic"}) || !room.rejected["$topic"] {
		t.Fatalf("rejected %v (%v)", result.Rejected, room.rejected["$topic"])
	}

	if _, ok := room.current[keyTopic]; ok || !slices.Equal(room.extremities, []string{"$jr"}) {
		t.Fatalf("отклоненное событие в состоянии %v или extremities %v", room.current, room.extremities)
	}
}
