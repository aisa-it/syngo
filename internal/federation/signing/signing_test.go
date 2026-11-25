package signing_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

func TestCanonicalJSONStable(t *testing.T) {
	t.Parallel()

	// Порядок ключей не важен, пробелов нет, не-ASCII не экранируется.
	first := map[string]any{"b": "тест", "a": 1, "c": []any{true, nil}}
	second := map[string]any{"c": []any{true, nil}, "a": 1, "b": "тест"}

	one, err := signing.CanonicalJSON(first)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	two, err := signing.CanonicalJSON(second)
	if err != nil {
		t.Fatalf("canonical 2: %v", err)
	}

	if !bytes.Equal(one, two) {
		t.Fatalf("канонизация нестабильна:\n%s\n%s", one, two)
	}

	want := `{"a":1,"b":"тест","c":[true,null]}`
	if string(one) != want {
		t.Fatalf("canonical = %s, ожидали %s", one, want)
	}

	if strings.Contains(string(one), " ") {
		t.Fatalf("в каноническом JSON есть пробелы: %s", one)
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	message := []byte(`{"method":"GET"}`)
	signature := key.Sign(message)

	if err := signing.VerifyEd25519(key.Public(), string(message), signature); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Подделанное сообщение и подпись — отказ.
	if err := signing.VerifyEd25519(key.Public(), string(message)+"x", signature); err == nil {
		t.Fatal("подпись должна не сойтись на изменённом сообщении")
	}

	if err := signing.VerifyEd25519(key.Public(), string(message), "AAAA"); err == nil {
		t.Fatal("битая подпись должна отвергаться")
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	content, err := signing.EncodeKeyFile(key)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	if !strings.HasPrefix(content, "ed25519 a_2 ") {
		t.Fatalf("формат файла: %q", content)
	}

	if !strings.Contains(key.KeyID, "a_2") {
		t.Fatalf("key id: %s", key.KeyID)
	}
}

// TestLoadSynapseKeyFile - файл ключа Synapse (seed без паддинга,
// возможно несколько строк) читается LoadKeyFile; паддинг тоже принимается.
func TestLoadSynapseKeyFile(t *testing.T) {
	t.Parallel()

	// Нулевой seed: публичный ключ - известный вектор signedjson.
	const (
		seedUnpadded = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		wantPublic   = "O2onvM62pC1io6jQKm8Nc2UyFXcd4kOmOsBIoYtZ2ik"
	)

	dir := t.TempDir()

	cases := map[string]string{
		"synapse":    "ed25519 a_AbCd " + seedUnpadded + "\n",
		"padded":     "ed25519 a_AbCd " + seedUnpadded + "=\n",
		"multi-line": "\ned25519 a_AbCd " + seedUnpadded + "\ned25519 old_1 " + seedUnpadded + "\n",
	}

	for name, content := range cases {
		path := filepath.Join(dir, name+".key")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		key, err := signing.LoadKeyFile(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if key.KeyID != "ed25519:a_AbCd" || key.Public() != wantPublic {
			t.Fatalf("%s: key_id=%s public=%s", name, key.KeyID, key.Public())
		}
	}

	// Свой формат совпадает с Synapse и читается обратно.
	key, err := signing.Generate("a_3")
	if err != nil {
		t.Fatal(err)
	}

	content, err := signing.EncodeKeyFile(key)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(content, "=") {
		t.Fatalf("seed с паддингом, Synapse пишет без: %q", content)
	}

	parsed, err := signing.ParseKeyFile(content)
	if err != nil || parsed.Public() != key.Public() {
		t.Fatalf("round trip: %v", err)
	}

	for _, bad := range []string{"", "ed25519 a_1", "rsa a_1 " + seedUnpadded, "ed25519 a_1 AAAA"} {
		if _, err := signing.ParseKeyFile(bad); err == nil {
			t.Errorf("ParseKeyFile(%q) должен отвергаться", bad)
		}
	}
}

func TestXMatrixHeader(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	body := []byte(`{"pdus":[]}`)

	header, err := signing.MatrixAuthHeader(key, "origin.test", "dest.test", "PUT", "/_matrix/federation/v1/send/1", body)
	if err != nil {
		t.Fatalf("header: %v", err)
	}

	parsed, err := signing.ParseMatrixAuth(header)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.Origin != "origin.test" || parsed.Destination != "dest.test" || parsed.KeyID != key.KeyID {
		t.Fatalf("parsed: %+v", parsed)
	}

	// Проверка запроса с корректной подписью.
	req := httptest.NewRequest("PUT", "/_matrix/federation/v1/send/1", strings.NewReader(string(body)))
	req.Header.Set("Authorization", header)

	auth, err := signing.VerifyRequest(req, body, "dest.test", func(origin, keyID string) (string, int64, error) {
		return key.Public(), time.Now().Add(time.Hour).UnixMilli(), nil
	})
	if err != nil {
		t.Fatalf("verify request: %v", err)
	}

	if auth.Origin != "origin.test" {
		t.Fatalf("auth: %+v", auth)
	}

	// Чужой destination — отказ.
	if _, err := signing.VerifyRequest(req, body, "other.test", func(string, string) (string, int64, error) {
		return key.Public(), 0, nil
	}); err == nil {
		t.Fatal("чужой destination должен отвергаться")
	}

	// Просроченный ключ — отказ.
	if _, err := signing.VerifyRequest(req, body, "dest.test", func(string, string) (string, int64, error) {
		return key.Public(), time.Now().Add(-time.Hour).UnixMilli(), nil
	}); err == nil {
		t.Fatal("просроченный ключ должен отвергаться")
	}

	// Подменённое тело — отказ.
	if _, err := signing.VerifyRequest(req, []byte(`{"pdus":[1]}`), "dest.test", func(string, string) (string, int64, error) {
		return key.Public(), 0, nil
	}); err == nil {
		t.Fatal("подмена тела должна ломать подпись")
	}
}

func TestSignJSON(t *testing.T) {
	t.Parallel()

	key, err := signing.Generate("a_1")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	object := map[string]any{"server_name": "test.local", "valid_until_ts": int64(1)}

	signed, err := signing.SignJSON(key, "test.local", object)
	if err != nil {
		t.Fatalf("sign json: %v", err)
	}

	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	signatures := check["signatures"].(map[string]any)["test.local"].(map[string]any)
	sig, ok := signatures[key.KeyID].(string)
	if !ok || sig == "" {
		t.Fatalf("подписи нет: %v", signatures)
	}

	// Внешняя проверка: канонизируем объект без signatures и сверяем подпись.
	delete(check, "signatures")

	canonical, err := signing.CanonicalJSON(check)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	if err := signing.VerifyEd25519(key.Public(), string(canonical), sig); err != nil {
		t.Fatalf("внешняя проверка подписи: %v", err)
	}
}
