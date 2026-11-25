package signing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSigningInputCanonicalJSON фиксирует формат входа подписи HTTP-запроса:
// канонический JSON {destination,method,origin,uri} (+content при наличии тела) —
// как в Synapse (не склейка строк: с ней Synapse отвечает 401 BadSignature).
func TestSigningInputCanonicalJSON(t *testing.T) {
	got, err := SigningInput("a.test", "b.test", "GET", "/_matrix/federation/v1/version", nil)
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}

	want := `{"destination":"b.test","method":"GET","origin":"a.test","uri":"/_matrix/federation/v1/version"}`
	if string(got) != want {
		t.Fatalf("вход подписи = %s, ожидали %s", got, want)
	}

	body, err := SigningInput("a.test", "b.test", "PUT", "/_matrix/federation/v1/send/1", []byte(`{"pdus":[]}`))
	if err != nil {
		t.Fatalf("SigningInput(body): %v", err)
	}

	wantBody := `{"content":{"pdus":[]},"destination":"b.test","method":"PUT","origin":"a.test","uri":"/_matrix/federation/v1/send/1"}`
	if string(body) != wantBody {
		t.Fatalf("вход подписи с телом = %s, ожидали %s", body, wantBody)
	}
}

// TestContentHash проверяет хеш события: канонический JSON без hashes/signatures/unsigned,
// sha256 и base64 raw. Без поля hashes Synapse отвергает invite («Malformed invite event»).
func TestContentHash(t *testing.T) {
	object := map[string]any{
		"type": "m.room.member", "room_id": "!r:test",
		"content":    map[string]any{"membership": "invite"},
		"signatures": map[string]any{"x": map[string]string{"k": "sig"}},
		"unsigned":   map[string]any{"age": 1},
	}

	got, err := ContentHash(object)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}

	if got == "" || len(got) != 43 {
		t.Fatalf("хеш = %q (len %d), ожидали 43 символа base64 raw", got, len(got))
	}

	// hashes не должен влиять на собственный хеш (поле исключается).
	object["hashes"] = map[string]string{"sha256": got}
	again, err := ContentHash(object)
	if err != nil || again != got {
		t.Fatalf("хеш зависит от hashes: %q != %q (%v)", again, got, err)
	}
}

// TestReferenceHash — event_id для room v4+ считается из канонического JSON
// с hashes, но без signatures/unsigned/event_id (origin решает core.EventID по версии комнаты).
func TestReferenceHash(t *testing.T) {
	t.Parallel()

	object := map[string]any{
		"room_id": "!r:test.local", "type": "m.room.member", "sender": "@alice:test.local",
		"state_key": "@bob:remote.test", "depth": 4,
		"content": map[string]any{"membership": "invite"},
		"hashes":  map[string]string{"sha256": "aaa"},
	}

	first, err := ReferenceHash(object)
	if err != nil {
		t.Fatalf("ReferenceHash() error: %v", err)
	}

	if first == "" || first[0] != '$' {
		t.Fatalf("ReferenceHash() = %q, ожидали $…", first)
	}

	// signatures/unsigned/event_id не влияют на id.
	withExtra := map[string]any{}
	for k, v := range object {
		withExtra[k] = v
	}

	withExtra["signatures"] = map[string]any{"test.local": map[string]string{"ed25519:a": "sig"}}
	withExtra["unsigned"] = map[string]any{"age": 5}
	withExtra["event_id"] = "$old"

	second, err := ReferenceHash(withExtra)
	if err != nil {
		t.Fatalf("ReferenceHash(withExtra) error: %v", err)
	}

	if first != second {
		t.Fatalf("id зависит от signatures/unsigned/event_id: %q != %q", first, second)
	}

	// Изменение hashes меняет id (он входит в канонический JSON).
	object["hashes"] = map[string]string{"sha256": "bbb"}

	third, err := ReferenceHash(object)
	if err != nil {
		t.Fatalf("ReferenceHash() error: %v", err)
	}

	if third == first {
		t.Fatal("id не изменился после смены hashes")
	}
}

// TestVerifyRequestRawURI фиксирует: подпись проверяется по request.uri — ровно тому
// URI, что пришёл на провод (с %-эскейпингом и query), как это делает Synapse.
// Для GET без тела в подписываемом объекте нет ключа content.
func TestVerifyRequestRawURI(t *testing.T) {
	key, err := Generate("a_1")
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	rawPath := "/_matrix/federation/v1/make_join/%21r%3Atest/%40u%3Aremote.test"
	req := httptest.NewRequest(http.MethodGet,
		"/_matrix/federation/v1/make_join/%21r%3Atest/%40u%3Aremote.test?ver=10", http.NoBody)

	header, err := MatrixAuthHeader(key, "remote.test", "test", http.MethodGet,
		rawPath+"?ver=10", nil)
	if err != nil {
		t.Fatalf("MatrixAuthHeader: %v", err)
	}

	req.Header.Set("Authorization", header)

	if _, err := VerifyRequest(req, nil, "test", func(_, _ string) (string, int64, error) {
		return key.Public(), 0, nil
	}); err != nil {
		t.Fatalf("VerifyRequest: %v", err)
	}
}

// TestCanonicalJSONQueryAmpersand фиксирует: канонический JSON не экранирует & (\\u0026),
// иначе подпись запросов с query (make_join?ver=1&ver=2…) не сходится с Synapse.
func TestCanonicalJSONQueryAmpersand(t *testing.T) {
	got, err := SigningInput("a.test", "b.test", "GET", "/_matrix/federation/v1/make_join/!r:b.test/@u:a.test?ver=1&ver=2", nil)
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}

	if bytes.Contains(got, []byte(`\u0026`)) {
		t.Fatalf("& экранирован в каноническом JSON: %s", got)
	}

	if !bytes.Contains(got, []byte(`?ver=1&ver=2`)) {
		t.Fatalf("query потерян: %s", got)
	}
}

// TestVerifyJSONBigNumbers — подпись события с большими целыми проверяется и после
// пересборки через json.Unmarshal (float64): CanonicalJSON приводит такие числа
// к целому виду, как Synapse.
func TestVerifyJSONBigNumbers(t *testing.T) {
	key, err := Generate("a_1")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	object := map[string]any{
		"type":             "m.room.member",
		"origin_server_ts": json.Number("1794990961041"),
		"depth":            json.Number("8"),
		"unsigned":         map[string]any{"age": 12},
	}

	signed, err := SignJSON(key, "a.test", object)
	if err != nil {
		t.Fatalf("SignJSON: %v", err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var reparsed map[string]any
	if err := json.Unmarshal(raw, &reparsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := VerifyJSON(reparsed, "a.test", func(_, _ string) (string, int64, error) {
		return key.Public(), 0, nil
	}); err != nil {
		t.Fatalf("VerifyJSON: %v", err)
	}
}

// TestSigningInputMatchesSynapse — байт-в-байт сверка входа подписи с тем, что подписывает
// Synapse (`synapse/http/matrixfederationclient.py`, build_auth_headers + canonicaljson:
// sort_keys, separators=(',',':'), ensure_ascii=False). Эталоны сняты с Python.
func TestSigningInputMatchesSynapse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		method string
		uri    string
		body   []byte
		want   string
	}{
		{
			name:   "GET без тела",
			method: "GET",
			uri:    "/_matrix/federation/v1/make_join/%21r%3Alocalhost/%40bob%3Asynapse.test?ver=10",
			want: `{"destination":"a.test","method":"GET","origin":"synapse.test",` +
				`"uri":"/_matrix/federation/v1/make_join/%21r%3Alocalhost/%40bob%3Asynapse.test?ver=10"}`,
		},
		{
			name:   "тело с HTML, юникодом и большими числами",
			method: "PUT",
			uri:    "/_matrix/federation/v1/invite/%21r%3Alocalhost/$abc",
			body: []byte(`{"type":"m.room.member","content":{"body":"<b>&\"тест\"</b>"},` +
				`"depth":9007199254740993,"origin_server_ts":1700000000000,"flag":true,"none":null,"pi":3.5}`),
			want: `{"content":{"content":{"body":"<b>&\"тест\"</b>"},"depth":9007199254740993,` +
				`"flag":true,"none":null,"origin_server_ts":1700000000000,"pi":3.5,"type":"m.room.member"},` +
				`"destination":"a.test","method":"PUT","origin":"synapse.test",` +
				`"uri":"/_matrix/federation/v1/invite/%21r%3Alocalhost/$abc"}`,
		},
		{
			// Числа - как json.dumps(json.loads(...)) Python; U+2028 - без экранирования
			// (эталон снят с Python 3).
			name:   "числа и разделители строк как в Python",
			method: "PUT",
			uri:    "/x",
			body: []byte(`{"a":1e2,"b":1.0,"c":3.50,"d":1e16,"e":1e-05,"f":0.0001,` +
				`"g":123456789012345678901234,"h":-0,"i":-0.0,"j":2.5e-7,"s":"a b"}`),
			want: `{"content":{"a":100.0,"b":1.0,"c":3.5,"d":1e+16,"e":1e-05,"f":0.0001,` +
				`"g":123456789012345678901234,"h":0,"i":-0.0,"j":2.5e-07,"s":"a` + " " + `b"},` +
				`"destination":"a.test","method":"PUT","origin":"synapse.test","uri":"/x"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := SigningInput("synapse.test", "a.test", tc.method, tc.uri, tc.body)
			if err != nil {
				t.Fatalf("SigningInput() error: %v", err)
			}

			if string(got) != tc.want {
				t.Fatalf("вход подписи не совпал с Synapse:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestVerifyJSONKeyValidityAtOriginServerTS - срок ключа сверяется с
// origin_server_ts события, а не с текущим временем.
func TestVerifyJSONKeyValidityAtOriginServerTS(t *testing.T) {
	t.Parallel()

	key, err := Generate("a_1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	const validUntil = int64(2_000)

	lookup := func(string, string) (string, int64, error) { return key.Public(), validUntil, nil }

	sign := func(ts any) map[string]any {
		object := map[string]any{"type": "m.room.message", "origin_server_ts": ts}

		signed, err := SignJSON(key, "remote.test", object)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}

		// Как после json.Unmarshal: числа - float64, подписи - map[string]any.
		raw, _ := json.Marshal(signed)

		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		return decoded
	}

	// Событие подписано, пока ключ действовал: принимается, хотя срок уже истек.
	if err := VerifyJSON(sign(1_000), "remote.test", lookup); err != nil {
		t.Fatalf("событие до valid_until_ts: %v", err)
	}

	// Событие позже срока ключа - отказ.
	if err := VerifyJSON(sign(3_000), "remote.test", lookup); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("событие после valid_until_ts: %v, ожидали ErrKeyExpired", err)
	}
}
