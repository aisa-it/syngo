//go:build integration

package api

import (
	"context"
	"net/http"
	"testing"
)

// TestFederationSendRejectsNonObjectPDU — PDU не-объект в транзакции: 400
// M_BAD_JSON, иначе пир получает 200 с пустым телом и событие теряется.
func TestFederationSendRejectsNonObjectPDU(t *testing.T) {
	env := newFedEnv(t)
	env.seedRoom(t, "!badpdu:test.local")

	body := map[string]any{"origin": "remote.test", "pdus": []any{[]int{1, 2}}}
	rec := env.request(t, http.MethodPut, "/_matrix/federation/v1/send/txn-badpdu", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("не-объект PDU: %d, ожидали 400 (%s)", rec.Code, rec.Body.String())
	}

	var seen int
	if err := env.server.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM federation_inbox WHERE txn_id='txn-badpdu'").Scan(&seen); err != nil {
		t.Fatalf("inbox: %v", err)
	}

	if seen != 0 {
		t.Fatalf("битая транзакция помечена в inbox (%d)", seen)
	}
}
