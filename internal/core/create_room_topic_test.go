package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// TestCreateRoomTopicOverridesInitialState - topic из параметра перекрывает
// m.room.topic из initial_state одним событием (раньше два события на один ключ
// давали 500 на сверке состояния) и пишет m.topic (Complement
// TestRoomCreate).
func TestCreateRoomTopicOverridesInitialState(t *testing.T) {
	t.Parallel()

	svc, store := newRoomService(t)

	_, err := svc.CreateRoom(context.Background(), core.CreateRoomRequest{
		Creator: alice,
		Topic:   "Test Room",
		InitialState: []core.StateEventRequest{
			{Type: "m.room.topic", Content: json.RawMessage(`{"topic":"Shenanigans"}`)},
			{Type: "m.room.name", Content: json.RawMessage(`{"name":"first"}`)},
			{Type: "m.room.name", Content: json.RawMessage(`{"name":"second"}`)},
		},
	})
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	topics, names := 0, 0

	for _, ev := range store.Events() {
		switch ev.Type {
		case "m.room.topic":
			topics++

			var content struct {
				Topic  string `json:"topic"`
				MTopic struct {
					Text []struct {
						Body string `json:"body"`
					} `json:"m.text"`
				} `json:"m.topic"`
			}

			if err := json.Unmarshal(ev.Content, &content); err != nil || content.Topic != "Test Room" ||
				len(content.MTopic.Text) != 1 || content.MTopic.Text[0].Body != "Test Room" {
				t.Fatalf("m.room.topic = %s", ev.Content)
			}
		case "m.room.name":
			names++

			if string(ev.Content) != `{"name":"second"}` {
				t.Fatalf("m.room.name = %s, ожидали последнее из initial_state", ev.Content)
			}
		}
	}

	if topics != 1 || names != 1 {
		t.Fatalf("событий topic %d, name %d, ожидали по одному", topics, names)
	}
}
