//go:build integration

package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/store"
)

// TestApplyFederationEventsExcluded - отклоненные и soft-failed события
// записываются в той же транзакции, что и состояние, и остаются вне таймлайна
// (/sync, /messages); redaction от отклоненного события не применяется.
func TestApplyFederationEventsExcluded(t *testing.T) {
	db, rooms, roomID := fedRoom(t)
	ctx := context.Background()

	pdus := []store.FedPDU{
		fedPDU(roomID, "$ok:remote.test", "m.room.message", "@carol:remote.test", `{"body":"ok"}`, ""),
		fedPDU(roomID, "$bad:remote.test", "m.room.redaction", "@carol:remote.test", `{}`, "$ok:remote.test"),
		fedPDU(roomID, "$soft:remote.test", "m.room.message", "@carol:remote.test", `{"body":"soft"}`, ""),
	}

	result, err := db.ApplyFederationEvents(ctx, "remote.test", roomID, pdus,
		func(core.RoomStateTx) (core.RoomStateResult, error) {
			return core.RoomStateResult{Rejected: []string{"$bad:remote.test"}, SoftFailed: []string{"$soft:remote.test"}}, nil
		})
	if err != nil || len(result.Rejected) != 1 {
		t.Fatalf("ApplyFederationEvents: %v %+v", err, result)
	}

	events, err := db.RoomEventsSince(ctx, roomID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for i := range events {
		ids = append(ids, events[i].EventID)
	}

	if !slices.Contains(ids, "$ok:remote.test") || slices.Contains(ids, "$bad:remote.test") || slices.Contains(ids, "$soft:remote.test") {
		t.Fatalf("таймлайн %v", ids)
	}

	var reason string

	var soft bool
	if err := db.Pool().QueryRow(ctx, `
		SELECT (SELECT rejected_reason FROM events WHERE event_id = '$bad:remote.test'),
		       (SELECT soft_failed FROM events WHERE event_id = '$soft:remote.test')`).Scan(&reason, &soft); err != nil {
		t.Fatal(err)
	}

	if reason != store.RejectedAuthError || !soft {
		t.Fatalf("rejected_reason %q, soft_failed %v", reason, soft)
	}

	ev, err := rooms.EventByID(ctx, roomID, "$ok:remote.test")
	if err != nil || ev.RedactedBy != "" {
		t.Fatalf("отклоненная redaction применена: %+v %v", ev, err)
	}
}

// TestApplyFederationEventsRevivesOutlier - outlier без состояния (auth-событие,
// догруженное по /event), принятый затем как событие таймлайна, получает новый
// stream_ordering и виден в /sync с токеном позже его записи.
func TestApplyFederationEventsRevivesOutlier(t *testing.T) {
	db, _, roomID := fedRoom(t)
	ctx := context.Background()

	pdu := fedPDU(roomID, "$early:remote.test", "m.room.message", "@carol:remote.test", `{"body":"x"}`, "")
	if err := db.InsertFederationOutliers(ctx, "remote.test", []store.FedPDU{pdu}, nil); err != nil {
		t.Fatal(err)
	}

	_, latest, err := store.NewRoomStore(db).MinMaxStream(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}

	var before int64
	if err := db.Pool().QueryRow(ctx, `SELECT stream_ordering FROM events WHERE event_id = '$early:remote.test'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ApplyFederationEvents(ctx, "remote.test", roomID, []store.FedPDU{pdu},
		func(core.RoomStateTx) (core.RoomStateResult, error) { return core.RoomStateResult{}, nil }); err != nil {
		t.Fatal(err)
	}

	events, err := db.RoomEventsSince(ctx, roomID, max(latest, before), 100)
	if err != nil || len(events) != 1 || events[0].EventID != "$early:remote.test" {
		t.Fatalf("после перевода в таймлайн: %+v %v", events, err)
	}
}

// TestAppendEventNulContent - \u0000 в content локального события не дает 500 (jsonb его не принимает):
// в jsonb NUL заменяется на U+FFFD, исходный PDU не меняется.
func TestAppendEventNulContent(t *testing.T) {
	_, rooms, roomID := fedRoom(t)
	ctx := context.Background()

	event := core.Event{
		EventID: "$nul:test.local", RoomID: roomID, Type: "m.room.message", Sender: redactAlice,
		Content: []byte(`{"body":"a\u0000b"}`), Depth: 100, OriginServerTS: 1, Origin: "test.local",
		PDU: []byte(`{"content":{"body":"a\u0000b"}}`),
	}

	if _, _, err := rooms.AppendEvent(ctx, event, nil); err != nil {
		t.Fatalf("AppendEvent с \\u0000: %v", err)
	}

	got, err := rooms.EventByID(ctx, roomID, "$nul:test.local")
	if err != nil || string(got.Content) != `{"body": "a`+"�"+`b"}` {
		t.Fatalf("content %s (%v)", got.Content, err)
	}
}
