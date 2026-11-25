package core

import (
	"encoding/json"
	"testing"

	"github.com/aisa-it/syngo/internal/pushrules"
)

// TestBuildPushBatch - разбор порции событий: уведомления только
// участникам после входа и не отправителю; прочитанное по receipt (в том числе
// треда) не считается; свое событие - неявное прочтение своей ленты внутри
// порции; удаленное событие не уведомляет; выход снимает уведомления комнаты.
func TestBuildPushBatch(t *testing.T) {
	t.Parallel()

	const room = "!r:test"

	stateKey := func(s string) *string { return &s }
	msg := func(id, sender string, stream int64, content string) Event {
		return Event{
			EventID: id, RoomID: room, Type: "m.room.message", Sender: sender,
			StreamOrdering: stream, Content: json.RawMessage(content),
		}
	}

	events := []Event{
		msg("$1", "@a:test", 10, `{"body":"1"}`),
		msg("$2", "@a:test", 11, `{"body":"2"}`),
		msg("$t", "@a:test", 12, `{"body":"т","m.relates_to":{"rel_type":"m.thread","event_id":"$1"}}`),
		msg("$3", "@b:test", 13, `{"body":"ответ b"}`),
		{EventID: "$red", RoomID: room, Type: "m.room.message", Sender: "@a:test", StreamOrdering: 14, RedactedBy: "$x"},
		msg("$4", "@a:test", 15, `{"body":"4"}`),
		{
			EventID: "$leave", RoomID: room, Type: "m.room.member", Sender: "@d:test", StateKey: stateKey("@d:test"),
			StreamOrdering: 16, Content: json.RawMessage(`{"membership":"leave"}`),
		},
		{EventID: "$r", RoomID: room, Type: "m.room.redaction", Sender: "@a:test", StreamOrdering: 17},
	}

	recipients := map[string][]PushRecipient{room: {
		{UserID: "@a:test"},
		{UserID: "@b:test"},
		{UserID: "@c:test", JoinStream: 11},
		{UserID: "@d:test"},
		{UserID: "@e:test"},
	}}

	// e прочитал тред до $t и основную ленту до $2.
	marks := []PushReadMark{
		{RoomID: room, UserID: "@e:test", ThreadID: "$1", Stream: 12},
		{RoomID: room, UserID: "@e:test", ThreadID: "main", Stream: 11},
	}

	batch := buildPushBatch(events, recipients, marks, func(userID string, ev Event) pushrules.Result {
		return pushrules.Result{Notify: ev.Type == "m.room.message", Highlight: userID == "@c:test"}
	})

	got := map[string][]string{}
	for _, a := range batch.Actions {
		got[a.UserID] = append(got[a.UserID], a.EventID)

		if a.EventID == "$t" && a.ThreadID != "$1" {
			t.Fatalf("тред не определен: %+v", a)
		}

		if a.UserID == "@c:test" && !a.Highlight {
			t.Fatalf("highlight потерян: %+v", a)
		}
	}

	want := map[string]string{
		"@a:test": "[]",         // свои не считаются, $3 прочитан своим $4
		"@b:test": "[$t $4]",    // $1, $2 основной ленты прочитаны своим $3
		"@c:test": "[$t $3 $4]", // до входа (11) - нет
		"@e:test": "[$3 $4]",    // тред и лента до $2 прочитаны
	}

	for user, ids := range want {
		if s := fmtIDs(got[user]); s != ids {
			t.Fatalf("%s: %s, ожидали %s (все: %v)", user, s, ids, got)
		}
	}

	if _, ok := got["@d:test"]; ok {
		t.Fatalf("вышедший получил уведомления: %v", got["@d:test"])
	}

	if len(batch.ReadUpTo) != 8 || len(batch.Left) != 1 || len(batch.RedactedBy) != 1 {
		t.Fatalf("прочтение %v, выходы %v, redaction %v", batch.ReadUpTo, batch.Left, batch.RedactedBy)
	}

	// Без получателей: только служебное.
	empty := buildPushBatch(events, nil, nil, nil)
	if len(empty.Actions) != 0 || len(empty.RedactedBy) != 1 || len(empty.Left) != 1 {
		t.Fatalf("без получателей: %+v", empty)
	}
}

func fmtIDs(ids []string) string {
	out := "["

	for i, id := range ids {
		if i > 0 {
			out += " "
		}

		out += id
	}

	return out + "]"
}
