//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// TestRedactedEventCarriesRedactedBecause — удаленное событие отдается клиенту с
// unsigned.redacted_because, а событие удаления - с верхнеуровневым redacts (v10).
// Без этого Element показывал удаленное сообщение как «Unable to decrypt».
func TestRedactedEventCarriesRedactedBecause(t *testing.T) {
	server := newIntegrationServer(t)
	alice := registerHTTP(t, server, "alice", "ADEV")

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/createRoom", `{"name":"r"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("createRoom: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		RoomID string `json:"room_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	room := url.PathEscape(created.RoomID)

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/send/m.room.message/t1",
		`{"msgtype":"m.text","body":"секрет"}`, alice)

	var sent struct {
		EventID string `json:"event_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &sent)

	rec = do(t, server, http.MethodPut, "/_matrix/client/v3/rooms/"+room+"/redact/"+url.PathEscape(sent.EventID)+"/r1",
		`{"reason":"тест"}`, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}

	var redaction struct {
		EventID string `json:"event_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &redaction)

	type view struct {
		EventID  string         `json:"event_id"`
		Type     string         `json:"type"`
		Redacts  string         `json:"redacts"`
		Content  map[string]any `json:"content"`
		Unsigned struct {
			RedactedBecause *view `json:"redacted_because"`
		} `json:"unsigned"`
	}

	check := func(where string, events []view) {
		t.Helper()

		var target, red *view

		for i := range events {
			switch events[i].EventID {
			case sent.EventID:
				target = &events[i]
			case redaction.EventID:
				red = &events[i]
			}
		}

		if target == nil || red == nil {
			t.Fatalf("%s: нет события или удаления в выдаче", where)
		}

		if target.Content["body"] != nil {
			t.Fatalf("%s: content удаленного события не вырезан: %v", where, target.Content)
		}

		if target.Unsigned.RedactedBecause == nil || target.Unsigned.RedactedBecause.EventID != redaction.EventID {
			t.Fatalf("%s: нет unsigned.redacted_because: %+v", where, target.Unsigned)
		}

		if red.Redacts != sent.EventID {
			t.Fatalf("%s: у m.room.redaction нет верхнеуровневого redacts: %+v", where, red)
		}
	}

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/messages?dir=b&limit=50", "", alice)

	var msgs struct {
		Chunk []view `json:"chunk"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &msgs)
	check("/messages", msgs.Chunk)

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/sync", "", alice)

	var sync struct {
		Rooms struct {
			Join map[string]struct {
				Timeline struct {
					Events []view `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &sync)
	check("/sync", sync.Rooms.Join[created.RoomID].Timeline.Events)

	rec = do(t, server, http.MethodGet, "/_matrix/client/v3/rooms/"+room+"/event/"+url.PathEscape(sent.EventID), "", alice)

	var single view

	_ = json.Unmarshal(rec.Body.Bytes(), &single)

	if single.Unsigned.RedactedBecause == nil {
		t.Fatalf("/event: нет unsigned.redacted_because: %s", rec.Body.String())
	}
}
