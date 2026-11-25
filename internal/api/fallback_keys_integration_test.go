//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestFallbackKeys - fallback key из /keys/upload хранится, его тип
// виден в device_unused_fallback_key_types, после исчерпания one-time keys
// /keys/claim выдает fallback (повторно, пока устройство не заменит его), ключ
// помечается использованным; повторная загрузка того же ключа пометку не снимает,
// новый ключ - снимает. Раньше fallback_keys молча терялись, и после исчерпания
// one-time keys устройству нельзя было открыть Olm-сессию.
func TestFallbackKeys(t *testing.T) {
	server := newIntegrationServer(t)

	alice := registerHTTP(t, server, "fbalice", "ALICE")
	bob := registerHTTP(t, server, "fbbob", "BOB")

	unusedTypes := func() []string {
		t.Helper()

		rec := do(t, server, http.MethodGet, "/_matrix/client/v3/sync?timeout=0", "", bob)
		if rec.Code != http.StatusOK {
			t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Unused *[]string `json:"device_unused_fallback_key_types"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode sync: %v", err)
		}

		if resp.Unused == nil {
			t.Fatalf("в /sync нет device_unused_fallback_key_types: %s", rec.Body.String())
		}

		return *resp.Unused
	}

	upload := func(body string) {
		t.Helper()

		if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload", body, bob); rec.Code != http.StatusOK {
			t.Fatalf("keys/upload: %d %s", rec.Code, rec.Body.String())
		}
	}

	claim := func() map[string]json.RawMessage {
		t.Helper()

		rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/claim",
			`{"one_time_keys":{"@fbbob:test.local":{"BOB":"signed_curve25519"}}}`, alice)
		if rec.Code != http.StatusOK {
			t.Fatalf("keys/claim: %d %s", rec.Code, rec.Body.String())
		}

		var resp keysClaimResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode claim: %v", err)
		}

		return resp.OneTimeKeys["@fbbob:test.local"]["BOB"]
	}

	if got := unusedTypes(); len(got) != 0 {
		t.Fatalf("до загрузки: %v, ожидали пусто", got)
	}

	upload(`{"one_time_keys":{"signed_curve25519:OTK1":{"key":"otk"}},
		"fallback_keys":{"signed_curve25519:FB1":{"key":"fb1","fallback":true}}}`)

	if got := unusedTypes(); !slices.Equal(got, []string{"signed_curve25519"}) {
		t.Fatalf("после загрузки fallback: %v", got)
	}

	// Сначала выдается one-time key, fallback не трогается.
	if keys := claim(); keys["signed_curve25519:OTK1"] == nil {
		t.Fatalf("первый claim: %v, ожидали OTK1", keys)
	}

	if got := unusedTypes(); len(got) != 1 {
		t.Fatalf("fallback помечен использованным после выдачи OTK: %v", got)
	}

	// One-time keys кончились - выдается fallback, и он остается доступен.
	for i := range 2 {
		if keys := claim(); keys["signed_curve25519:FB1"] == nil {
			t.Fatalf("claim %d после исчерпания OTK: %v, ожидали FB1", i, keys)
		}
	}

	if got := unusedTypes(); len(got) != 0 {
		t.Fatalf("после выдачи fallback: %v, ожидали пусто", got)
	}

	// Тот же ключ повторно (клиент шлет его при каждой загрузке) - пометка остается.
	upload(`{"fallback_keys":{"signed_curve25519:FB1":{"key":"fb1","fallback":true}}}`)

	if got := unusedTypes(); len(got) != 0 {
		t.Fatalf("повторная загрузка того же ключа сняла пометку: %v", got)
	}

	// Новый fallback заменяет старый и снова не использован.
	upload(`{"fallback_keys":{"signed_curve25519:FB2":{"key":"fb2","fallback":true}}}`)

	if got := unusedTypes(); !slices.Equal(got, []string{"signed_curve25519"}) {
		t.Fatalf("после замены fallback: %v", got)
	}

	keys := claim()
	if keys["signed_curve25519:FB2"] == nil || keys["signed_curve25519:FB1"] != nil {
		t.Fatalf("claim после замены: %v, ожидали только FB2", keys)
	}

	// Идентификатор ключа без алгоритма - 400, а не молчаливая потеря.
	if rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload",
		`{"fallback_keys":{"bad_key_id":{"key":"x"}}}`, bob); rec.Code != http.StatusBadRequest {
		t.Fatalf("fallback без алгоритма в id: %d, ожидали 400", rec.Code)
	}
}
