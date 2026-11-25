//go:build integration

package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
)

// csKey — ключ ed25519 для тестов кросс-подписи.
type csKey struct {
	pub  string
	priv ed25519.PrivateKey
}

func newCSKey(t *testing.T) csKey {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return csKey{pub: base64.RawStdEncoding.EncodeToString(pub), priv: priv}
}

// signObj подписывает объект (без signatures/unsigned) ключом keyID от имени userID.
func signObj(t *testing.T, obj map[string]any, userID, keyID string, key csKey) map[string]any {
	t.Helper()

	payload, err := core.SignedPayload(obj)
	if err != nil {
		t.Fatal(err)
	}

	sig := base64.RawStdEncoding.EncodeToString(ed25519.Sign(key.priv, []byte(payload)))

	sigs, _ := obj["signatures"].(map[string]any)
	if sigs == nil {
		sigs = map[string]any{}
	}

	byUser, _ := sigs[userID].(map[string]any)
	if byUser == nil {
		byUser = map[string]any{}
	}

	byUser[keyID] = sig
	sigs[userID] = byUser
	obj["signatures"] = sigs

	return obj
}

func crossKeyObj(userID, usage string, key csKey) map[string]any {
	return map[string]any{"user_id": userID, "usage": []any{usage}, "keys": map[string]any{"ed25519:" + key.pub: key.pub}}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// uploadDevice загружает device_keys устройства, подписанные его же ключом.
func uploadDevice(t *testing.T, server *Server, token, userID, deviceID string, key csKey) map[string]any {
	t.Helper()

	device := map[string]any{
		"user_id": userID, "device_id": deviceID,
		"algorithms": []any{"m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"},
		"keys":       map[string]any{"ed25519:" + deviceID: key.pub, "curve25519:" + deviceID: key.pub},
	}
	device = signObj(t, device, userID, "ed25519:"+deviceID, key)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/upload", `{"device_keys":`+mustJSON(t, device)+`}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/upload: %d %s", rec.Code, rec.Body.String())
	}

	return device
}

type queryResp struct {
	DeviceKeys      map[string]map[string]map[string]any `json:"device_keys"`
	MasterKeys      map[string]map[string]any            `json:"master_keys"`
	SelfSigningKeys map[string]map[string]any            `json:"self_signing_keys"`
	UserSigningKeys map[string]map[string]any            `json:"user_signing_keys"`
}

func keysQuery(t *testing.T, server *Server, token, body string) queryResp {
	t.Helper()

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/query", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("keys/query: %d %s", rec.Code, rec.Body.String())
	}

	var resp queryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	return resp
}

func hasSig(obj map[string]any, userID, keyID string) bool {
	sigs, _ := obj["signatures"].(map[string]any)
	byUser, _ := sigs[userID].(map[string]any)
	_, ok := byUser[keyID]

	return ok
}

// TestCrossSigningElementFlow — сценарий Element после регистрации:
// загрузка master/self_signing/user_signing без UIA, подпись своего устройства,
// выдача в /keys/query; без этого Element считает свое устройство неподтвержденным.
func TestCrossSigningElementFlow(t *testing.T) {
	server := newIntegrationServer(t)

	const alice, bob = "@alice:test.local", "@bob:test.local"

	aliceTok := registerHTTP(t, server, "alice", "ADEV")
	bobTok := registerHTTP(t, server, "bob", "BDEV")

	aliceDevKey, bobDevKey := newCSKey(t), newCSKey(t)
	aliceDevice := uploadDevice(t, server, aliceTok, alice, "ADEV", aliceDevKey)
	uploadDevice(t, server, bobTok, bob, "BDEV", bobDevKey)

	master, ssk, usk := newCSKey(t), newCSKey(t), newCSKey(t)
	masterObj := crossKeyObj(alice, "master", master)
	sskObj := signObj(t, crossKeyObj(alice, "self_signing", ssk), alice, "ed25519:"+master.pub, master)
	uskObj := signObj(t, crossKeyObj(alice, "user_signing", usk), alice, "ed25519:"+master.pub, master)

	// self_signing без подписи мастером - отказ.
	bad := crossKeyObj(alice, "self_signing", ssk)

	rec := do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_key":`+mustJSON(t, masterObj)+`,"self_signing_key":`+mustJSON(t, bad)+`}`, aliceTok)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "M_INVALID_SIGNATURE") {
		t.Fatalf("неподписанный self_signing: %d %s", rec.Code, rec.Body.String())
	}

	// Первая загрузка - без UIA.
	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_key":`+mustJSON(t, masterObj)+`,"self_signing_key":`+mustJSON(t, sskObj)+
			`,"user_signing_key":`+mustJSON(t, uskObj)+`}`, aliceTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("device_signing/upload: %d %s", rec.Code, rec.Body.String())
	}

	// Element подписывает свое устройство self_signing, а мастер - ключом устройства.
	signedDevice := signObj(t, aliceDevice, alice, "ed25519:"+ssk.pub, ssk)
	signedMaster := signObj(t, crossKeyObj(alice, "master", master), alice, "ed25519:ADEV", aliceDevKey)

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/signatures/upload",
		`{"`+alice+`":{"ADEV":`+mustJSON(t, signedDevice)+`,"`+master.pub+`":`+mustJSON(t, signedMaster)+`}}`, aliceTok)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "errcode") {
		t.Fatalf("signatures/upload: %d %s", rec.Code, rec.Body.String())
	}

	own := keysQuery(t, server, aliceTok, `{"device_keys":{"`+alice+`":[]}}`)
	if own.MasterKeys[alice] == nil || own.SelfSigningKeys[alice] == nil || own.UserSigningKeys[alice] == nil {
		t.Fatalf("свои кросс-ключи не отданы: %+v", own)
	}

	if !hasSig(own.DeviceKeys[alice]["ADEV"], alice, "ed25519:"+ssk.pub) {
		t.Fatalf("подпись устройства self_signing не подмешана: %v", own.DeviceKeys[alice]["ADEV"])
	}

	if !hasSig(own.MasterKeys[alice], alice, "ed25519:ADEV") {
		t.Fatalf("подпись мастера устройством не подмешана: %v", own.MasterKeys[alice])
	}

	// Боб видит master и self_signing Алисы, но не ее user_signing; смешанный запрос
	// {alice: [], bob: [BDEV]} отдает все устройства Алисы.
	seen := keysQuery(t, server, bobTok, `{"device_keys":{"`+alice+`":[],"`+bob+`":["BDEV"]}}`)
	if seen.MasterKeys[alice] == nil || seen.SelfSigningKeys[alice] == nil {
		t.Fatalf("Боб не видит кросс-ключи Алисы: %+v", seen)
	}

	if seen.UserSigningKeys[alice] != nil {
		t.Fatal("user_signing Алисы виден Бобу")
	}

	if seen.DeviceKeys[alice]["ADEV"] == nil || seen.DeviceKeys[bob]["BDEV"] == nil {
		t.Fatalf("смешанный запрос устройств: %v", seen.DeviceKeys)
	}

	// Подмена содержимого подписываемого объекта - отказ по ключу, без 4xx на весь запрос.
	forged := signObj(t, map[string]any{"user_id": alice, "device_id": "ADEV", "keys": map[string]any{"ed25519:ADEV": "x"}},
		alice, "ed25519:"+ssk.pub, ssk)

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/signatures/upload",
		`{"`+alice+`":{"ADEV":`+mustJSON(t, forged)+`}}`, aliceTok)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "M_INVALID_SIGNATURE") {
		t.Fatalf("подменный объект: %d %s", rec.Code, rec.Body.String())
	}

	// Боб подписывает мастер-ключ Алисы своим user_signing: подпись видна Бобу, но не Алисе.
	bobMaster, bobUsk := newCSKey(t), newCSKey(t)
	bobUskObj := signObj(t, crossKeyObj(bob, "user_signing", bobUsk), bob, "ed25519:"+bobMaster.pub, bobMaster)

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_key":`+mustJSON(t, crossKeyObj(bob, "master", bobMaster))+`,"user_signing_key":`+mustJSON(t, bobUskObj)+`}`, bobTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("bob device_signing: %d %s", rec.Code, rec.Body.String())
	}

	aliceMasterByBob := signObj(t, crossKeyObj(alice, "master", master), bob, "ed25519:"+bobUsk.pub, bobUsk)

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/signatures/upload",
		`{"`+alice+`":{"`+master.pub+`":`+mustJSON(t, aliceMasterByBob)+`}}`, bobTok)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "errcode") {
		t.Fatalf("bob signs alice master: %d %s", rec.Code, rec.Body.String())
	}

	if !hasSig(keysQuery(t, server, bobTok, `{"device_keys":{"`+alice+`":[]}}`).MasterKeys[alice], bob, "ed25519:"+bobUsk.pub) {
		t.Fatal("Боб не видит свою подпись мастер-ключа Алисы")
	}

	if hasSig(keysQuery(t, server, aliceTok, `{"device_keys":{"`+alice+`":[]}}`).MasterKeys[alice], bob, "ed25519:"+bobUsk.pub) {
		t.Fatal("подпись Боба видна Алисе")
	}

	// Замена мастер-ключа - только с паролем.
	master2 := newCSKey(t)

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_key":`+mustJSON(t, crossKeyObj(alice, "master", master2))+`}`, aliceTok)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "m.login.password") {
		t.Fatalf("замена мастера без UIA: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, server, http.MethodPost, "/_matrix/client/v3/keys/device_signing/upload",
		`{"master_key":`+mustJSON(t, crossKeyObj(alice, "master", master2))+
			`,"auth":{"type":"m.login.password","password":"1234567890"}}`, aliceTok)
	if rec.Code != http.StatusOK {
		t.Fatalf("замена мастера с паролем: %d %s", rec.Code, rec.Body.String())
	}

	// Старая подпись устройства прежним мастером больше не подмешивается к новому мастеру.
	after := keysQuery(t, server, aliceTok, `{"device_keys":{"`+alice+`":[]}}`)
	if after.MasterKeys[alice]["keys"].(map[string]any)["ed25519:"+master2.pub] == nil {
		t.Fatalf("новый мастер не отдан: %v", after.MasterKeys[alice])
	}
}
