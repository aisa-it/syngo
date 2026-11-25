package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiffState(t *testing.T) {
	t.Parallel()

	ours := []stateRow{
		{Type: "m.room.create", EventID: "$c"},
		{Type: "m.room.topic", EventID: "$t-ours"},
		{Type: "m.room.member", StateKey: "@a:test", EventID: "$m"},
	}

	theirs := []stateRow{
		{Type: "m.room.create", EventID: "$c"},
		{Type: "m.room.topic", EventID: "$t-theirs"},
		{Type: "m.room.power_levels", EventID: "$pl"},
	}

	diff := diffState(ours, theirs)

	if len(diff) != 3 {
		t.Fatalf("расхождений %d, ожидали 3: %v", len(diff), diff)
	}

	if diff["m.room.power_levels\x00"] == "" || diff["m.room.member\x00@a:test"] == "" {
		t.Fatalf("не все расхождения описаны: %v", diff)
	}

	if diff["m.room.create\x00"] != "" {
		t.Fatal("совпадающий ключ попал в расхождения")
	}

	if len(diffLines(diff)) != 3 {
		t.Fatalf("diffLines вернул %d строк", len(diffLines(diff)))
	}
}

func TestDiffStateEqual(t *testing.T) {
	t.Parallel()

	rows := []stateRow{{Type: "m.room.create", EventID: "$c"}, {Type: "m.room.topic", EventID: "$t"}}

	if diff := diffState(rows, rows); len(diff) != 0 {
		t.Fatalf("ожидали совпадение, получили %v", diff)
	}
}

// TestSynapseState — чтение состояния через admin API (включая версию сервера).
func TestSynapseState(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/_synapse/admin/v1/rooms/!room:test/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": []map[string]any{
				{"type": "m.room.topic", "state_key": "", "event_id": "$t"},
				{"type": "m.room.create", "state_key": "", "event_id": "$c"},
			},
		})
	})
	mux.HandleFunc("/_matrix/federation/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"server_version": "1.165.0"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rows, version, err := synapseState(context.Background(), srv.URL, "admin-token", "!room:test")
	if err != nil {
		t.Fatalf("synapse state: %v", err)
	}

	if version != "1.165.0" {
		t.Fatalf("версия %q", version)
	}

	if len(rows) != 2 || rows[0].Type != "m.room.create" {
		t.Fatalf("состояние не отсортировано: %+v", rows)
	}

	if _, _, err := synapseState(context.Background(), srv.URL, "bad", "!room:test"); err == nil {
		t.Fatal("ожидали ошибку при неверном токене")
	}
}
