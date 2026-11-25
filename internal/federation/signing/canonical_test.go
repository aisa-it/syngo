package signing_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// TestCanonicalJSONSpecVectors - примеры из приложения спецификации «Canonical JSON»:
// вход - текст JSON, выход - байты канонической формы.
func TestCanonicalJSONSpecVectors(t *testing.T) {
	t.Parallel()

	vectors := []struct{ in, want string }{
		{`{}`, `{}`},
		{`{"one": 1, "two": "Two"}`, `{"one":1,"two":"Two"}`},
		{`{"b": "2", "a": "1"}`, `{"a":"1","b":"2"}`},
		{`{"b":"2","a":"1"}`, `{"a":"1","b":"2"}`},
		{
			`{"auth": {"success": true, "mxid": "@john.doe:example.com", "profile": {"display_name": "John Doe",
			"three_pids": [{"medium": "email", "address": "john.doe@example.org"},
			{"medium": "msisdn", "address": "123456789"}]}}}`,
			`{"auth":{"mxid":"@john.doe:example.com","profile":{"display_name":"John Doe","three_pids":` +
				`[{"address":"john.doe@example.org","medium":"email"},{"address":"123456789","medium":"msisdn"}]},"success":true}}`,
		},
		{`{"a": "日本語"}`, `{"a":"日本語"}`},
		{`{"本": 2, "日": 1}`, `{"日":1,"本":2}`},
		{`{"a": "日"}`, `{"a":"日"}`},
		{`{"a": null}`, `{"a":null}`},
		{`{"a": -0, "b": 1e10}`, `{"a":0,"b":10000000000}`},
	}

	for _, vector := range vectors {
		// Тот же вход - и как текст (Canonicalize), и как разобранное значение (CanonicalJSON).
		got, err := signing.Canonicalize([]byte(vector.in))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", vector.in, err)
		}

		if string(got) != vector.want {
			t.Fatalf("Canonicalize(%s) = %s, ожидали %s", vector.in, got, vector.want)
		}

		viaValue, err := signing.CanonicalJSON(json.RawMessage(vector.in))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s): %v", vector.in, err)
		}

		if string(viaValue) != vector.want {
			t.Fatalf("CanonicalJSON(%s) = %s, ожидали %s", vector.in, viaValue, vector.want)
		}
	}
}

// TestCanonicalJSONStringEscapes - грамматика строк спецификации: экранируются только
// ", \ и управляющие символы; U+2028/U+2029, DEL и HTML-символы - как есть.
func TestCanonicalJSONStringEscapes(t *testing.T) {
	t.Parallel()

	value := map[string]any{"a": "x y z", "b": "\x00\x01\x08\x09\x0a\x0b\x0c\x0d\x1f\x7f", "c": `<&>"\/`}

	got, err := signing.CanonicalJSON(value)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	want := "{\"a\":\"x y z\",\"b\":\"\\u0000\\u0001\\b\\t\\n\\u000b\\f\\r\\u001f\x7f\",\"c\":\"<&>\\\"\\\\/\"}"
	if string(got) != want {
		t.Fatalf("canonical = %q\nожидали     %q", got, want)
	}

	// То же из текста с экранированиями:   и \/ раскрываются.
	fromText, err := signing.Canonicalize([]byte(`{"a":"x y z","b":"\u0000\u0001\b\t\n\u000B\f\r\u001F` + "\x7f" + `","c":"<&>\"\\\/"}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	if string(fromText) != want {
		t.Fatalf("canonicalize = %q\nожидали       %q", fromText, want)
	}

	// Суррогатная пара склеивается в одну кодовую точку.
	pair, err := signing.Canonicalize([]byte(`"😀"`))
	if err != nil || string(pair) != "\"\U0001F600\"" {
		t.Fatalf("surrogate pair = %q, %v", pair, err)
	}
}

// TestCanonicalJSONRejectsNonCanonical - значения вне канонического JSON - ошибка,
// а не молчаливая замена: дробные, большие целые, битый UTF-8, одиночный суррогат.
func TestCanonicalJSONRejectsNonCanonical(t *testing.T) {
	t.Parallel()

	texts := []string{
		`{"a":1.5}`, `{"a":0.1}`, `{"a":1e-3}`, `{"a":9007199254740992}`, `{"a":-9007199254740992}`,
		`{"a":12345678901234567890}`, `{"a":1e16}`, `{"a":1e999999999}`,
		"{\"a\":\"\xff\"}", `{"a":"\ud800"}`, `{"a":"\udc00x"}`, `{"a":"\ud800A"}`,
		`{"a":01}`, `{"a":1,}`, `{"a" 1}`, `[1 2]`, `{"a":tru}`, "{\"a\":\"\x01\"}",
	}

	for _, text := range texts {
		if got, err := signing.Canonicalize([]byte(text)); !errors.Is(err, signing.ErrNotCanonical) {
			t.Fatalf("Canonicalize(%q) = %s, %v; ожидали ErrNotCanonical", text, got, err)
		}
	}

	values := []any{
		map[string]any{"a": 1.5},
		map[string]any{"a": float64(1 << 60)},
		map[string]any{"a": int64(1) << 53},
		map[string]any{"a": "\xff"},
		map[string]any{"\xfe": "a"},
		[]any{"ok", "\xc3"},
		map[string]string{"a": "\xff"},
		json.RawMessage(`{"a":"\ud800"}`),
	}

	for _, value := range values {
		if got, err := signing.CanonicalJSON(value); !errors.Is(err, signing.ErrNotCanonical) {
			t.Fatalf("CanonicalJSON(%#v) = %s, %v; ожидали ErrNotCanonical", value, got, err)
		}
	}

	// Границы диапазона и целые в записи с экспонентой/нулевой дробью допустимы.
	got, err := signing.Canonicalize([]byte(`[9007199254740991,-9007199254740991,1.0,25e-1e0,2.50e1]`))
	if err == nil {
		t.Fatalf("битое число принято: %s", got)
	}

	got, err = signing.Canonicalize([]byte(`[9007199254740991,-9007199254740991,1.0,2.50e1,100e-2]`))
	if err != nil || string(got) != `[9007199254740991,-9007199254740991,1,25,1]` {
		t.Fatalf("границы = %s, %v", got, err)
	}
}

// specSigningKey - ключ тест-векторов подписи спецификации (seed YJDBA9...).
func specSigningKey(t *testing.T) signing.Key {
	t.Helper()

	seed, err := base64.RawStdEncoding.DecodeString("YJDBA9Xnr2sVqXD9Vj7XVUnmFZcZrlw8Md7kMW+3XA1")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	return signing.Key{KeyID: "ed25519:1", Private: ed25519.NewKeyFromSeed(seed)}
}

// TestSigningSpecVectors - тест-векторы подписи JSON и событий из приложения
// спецификации: подпись и content hash считаются по каноническому JSON.
func TestSigningSpecVectors(t *testing.T) {
	t.Parallel()

	key := specSigningKey(t)

	signatureOf := func(object map[string]any) string {
		t.Helper()

		signed, err := signing.SignJSON(key, "domain", object)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}

		sigs, _ := signed["signatures"].(map[string]any)
		byKey, _ := sigs["domain"].(map[string]string)

		return byKey["ed25519:1"]
	}

	if got := signatureOf(map[string]any{}); got != "K8280/U9SSy9IVtjBuVeLr+HpOB4BQFWbg+UZaADMtTdGYI7Geitb76LTrr5QV/7Xg4ahLwYGYZzuHGZKM5ZAQ" {
		t.Fatalf("подпись {} = %s", got)
	}

	if got := signatureOf(map[string]any{"one": 1, "two": "Two"}); got != "KqmLSbO39/Bzb0QIYE82zqLwsA+PDzYIpIRA2sRQ4sL53+sN6/fpNSoqE7BP7vBZhG6kYdD13EIMJpvhJI+6Bw" {
		t.Fatalf("подпись {one,two} = %s", got)
	}

	// Минимальное событие: hashes и подпись по redacted-форме (для v1-v10 origin остается).
	event := map[string]any{
		"room_id": "!x:domain", "sender": "@a:domain", "origin": "domain", "origin_server_ts": 1000000,
		"type": "X", "content": map[string]any{}, "prev_events": []any{}, "auth_events": []any{}, "depth": 3,
		"unsigned": map[string]any{"age_ts": 1000000},
	}

	hash, err := signing.ContentHash(event)
	if err != nil || hash != "5jM4wQpv6lnBo7CLIghJuHdW+s2CMBJPUOGOC89ncos" {
		t.Fatalf("content hash = %s, %v", hash, err)
	}

	event["hashes"] = map[string]any{"sha256": hash}
	if got := signatureOf(event); got != "KxwGjPSDEtvnFgU00fwFz+l6d2pJM6XBIaMEn81SXPTRl16AqLAYqfIReFGZlHi5KLjAWbOoMszkwsQma+lYAg" {
		t.Fatalf("подпись минимального события = %s", got)
	}

	// Событие с redactable content: hash по полному событию, подпись - по redacted
	// (content у m.room.message вырезается целиком).
	message := map[string]any{
		"content": map[string]any{"body": "Here is the message content"}, "event_id": "$0:domain",
		"origin": "domain", "origin_server_ts": 1000000, "type": "m.room.message", "room_id": "!r:domain",
		"sender": "@u:domain", "unsigned": map[string]any{"age_ts": 1000000},
	}

	hash, err = signing.ContentHash(message)
	if err != nil || hash != "onLKD1bGljeBWQhWZ1kaP9SorVmRQNdN5aM2JYU2n/g" {
		t.Fatalf("content hash сообщения = %s, %v", hash, err)
	}

	redacted := map[string]any{}
	for k, v := range message {
		redacted[k] = v
	}

	redacted["content"] = map[string]any{}
	redacted["hashes"] = map[string]any{"sha256": hash}

	if got := signatureOf(redacted); got != "Wm+VzmOUOz08Ds+0NTWb1d4CZrVsJSikkeRxh6aCcUwu6pNC78FunoD7KNWzqFn241eYHYMGCA5McEiVPdhzBA" {
		t.Fatalf("подпись сообщения = %s", got)
	}
}
