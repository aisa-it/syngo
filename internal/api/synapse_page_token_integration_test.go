//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/aisa-it/syngo/internal/store"
)

// roomPageEvents - события комнаты из БД в порядке stream_ordering.
func roomPageEvents(t *testing.T, server *Server, roomID string) []pageEvent {
	t.Helper()

	rows, err := server.db.Pool().Query(context.Background(),
		`SELECT event_id, depth, stream_ordering FROM events WHERE room_id = $1 AND NOT outlier ORDER BY stream_ordering`, roomID)
	if err != nil {
		t.Fatalf("события комнаты: %v", err)
	}
	defer rows.Close()

	var out []pageEvent

	for rows.Next() {
		var ev pageEvent
		if err := rows.Scan(&ev.ID, &ev.Depth, &ev.Stream); err != nil {
			t.Fatalf("scan: %v", err)
		}

		out = append(out, ev)
	}

	return out
}

// TestMessagesSynapsePageTokenImported - история как после syngo-import:
// события backfill Synapse с отрицательным stream_ordering и сохраненной depth,
// ветки DAG с равной depth; пагинация /messages от токена Synapse в обе стороны
// без повторов и пропусков, live-токен - от конца, мусор - M_INVALID_PARAM.
func TestMessagesSynapsePageTokenImported(t *testing.T) {
	server := newIntegrationServer(t)
	token := registerHTTP(t, server, "pagetok", "PTDEV")
	roomID := createRoomFor(t, server, token)
	ctx := context.Background()

	// Импортированная часть истории: старше всего, stream_ordering < 0 (backfill).
	var imported []store.EventRow

	for i, depth := range []int64{1, 2, 2, 3} {
		imported = append(imported, store.EventRow{
			EventID: fmt.Sprintf("$imported%d", i), RoomID: roomID, Type: "m.room.message",
			Sender: "@pagetok:test.local", Content: []byte(fmt.Sprintf(`{"msgtype":"m.text","body":"old-%d"}`, i)),
			PrevEvents: []string{}, AuthEvents: []string{}, Depth: depth,
			OriginServerTS: 1_700_000_000_000 + int64(i), Origin: "test.local", StreamOrdering: int64(i - 10),
		})
	}

	if _, err := server.db.InsertEvents(ctx, imported); err != nil {
		t.Fatalf("импорт событий: %v", err)
	}

	for i := range 5 {
		sendMessage(t, server, token, roomID, fmt.Sprintf("pt-%d", i), fmt.Sprintf(`{"msgtype":"m.text","body":"new-%d"}`, i))
	}

	// depth событий после импорта - выше импортированных, с ветками равной depth.
	events := roomPageEvents(t, server, roomID)
	depth := int64(3)

	for i := range events {
		if events[i].Stream < 0 {
			continue
		}

		if i%3 != 0 {
			depth++
		}

		events[i].Depth = depth

		if _, err := server.db.Pool().Exec(ctx, `UPDATE events SET depth = $2 WHERE event_id = $1`, events[i].ID, depth); err != nil {
			t.Fatalf("depth: %v", err)
		}
	}

	checkSynapseTokenPagination(t, server, token, roomID, events)
	checkSynapseLiveAndInvalid(t, server, token, roomID, events)

	// Порядок по depth и по stream расходится (позднее пришедшее событие
	// федерации глубоко в прошлом): от токена dir=b приходят все события до
	// границы - пропусков нет, повторы уже виденного допустимы.
	last := events[len(events)-1]
	if _, err := server.db.Pool().Exec(ctx, `UPDATE events SET depth = 2 WHERE event_id = $1`, last.ID); err != nil {
		t.Fatalf("depth: %v", err)
	}

	pivot := events[len(events)-3]
	got := collectMessages(t, server, token, roomID, synapseTopoToken(pivot.Depth, pivot.Stream), "b")

	for _, ev := range events {
		before := ev.Depth < pivot.Depth || (ev.Depth == pivot.Depth && ev.Stream <= pivot.Stream)
		if ev.ID == last.ID {
			before = true
		}

		if before && !slices.Contains(got, ev.ID) {
			t.Fatalf("dir=b от t%d-%d: пропущено %s (%v)", pivot.Depth, pivot.Stream, ev.ID, got)
		}
	}
}

// TestKeysChangesSynapseToken - /keys/changes с from/to Synapse (sync-токен до
// переезда): раньше 400 M_UNKNOWN_POS. Позицию не перевести, поэтому from Synapse
// - с начала (клиент перезапросит ключи всех, у кого они менялись), to - текущая
// позиция.
func TestKeysChangesSynapseToken(t *testing.T) {
	env := perfEnv{t: t, srv: newIntegrationServer(t)}

	alice := registerPusherProbe(t, env.srv, "kcalice", "kc-alice-pass")
	bob := registerPusherProbe(t, env.srv, "kcbob", "kc-bob-pass")
	env.room(alice, bob)

	env.call(alice, http.MethodPost, "/_matrix/client/v3/keys/upload",
		`{"device_keys":{"user_id":"@kcalice:test.local","device_id":"DEV","algorithms":["m.olm.v1.curve25519-aes-sha2"],"keys":{"curve25519:DEV":"k"}}}`)

	for _, query := range []string{
		"from=" + url.QueryEscape("s123_4_5_6_7_8_9_m10~1.10_0_0_0_0_0_0"),
		"from=s123&to=" + url.QueryEscape("s999_1_2_3_4_5_6_7_0_0_0_0_0_0"),
	} {
		rec, _ := env.call(bob, http.MethodGet, "/_matrix/client/v3/keys/changes?"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("keys/changes?%s: %d %s", query, rec.Code, rec.Body.String())
		}

		var resp keysChangesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode keys/changes: %v", err)
		}

		if !slices.Contains(resp.Changed, "@kcalice:test.local") {
			t.Fatalf("keys/changes?%s без alice: %v", query, resp.Changed)
		}
	}
}
