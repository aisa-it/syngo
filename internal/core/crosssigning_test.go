package core_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/coretest"
)

type edKey struct {
	pub  string
	priv ed25519.PrivateKey
}

func newEdKey(t *testing.T) edKey {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	return edKey{pub: base64.RawStdEncoding.EncodeToString(pub), priv: priv}
}

// sign добавляет в объект подпись userID ключом keyID.
func sign(t *testing.T, obj map[string]any, userID, keyID string, key edKey) map[string]any {
	t.Helper()

	payload, err := core.SignedPayload(obj)
	if err != nil {
		t.Fatal(err)
	}

	sigs, _ := obj["signatures"].(map[string]any)
	if sigs == nil {
		sigs = map[string]any{}
	}

	byUser, _ := sigs[userID].(map[string]any)
	if byUser == nil {
		byUser = map[string]any{}
	}

	byUser[keyID] = base64.RawStdEncoding.EncodeToString(ed25519.Sign(key.priv, []byte(payload)))
	sigs[userID] = byUser
	obj["signatures"] = sigs

	return obj
}

func crossKey(userID, usage string, key edKey) map[string]any {
	return map[string]any{
		"user_id": userID, "usage": []any{usage},
		"keys": map[string]any{"ed25519:" + key.pub: key.pub},
	}
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

type crossSigningEnv struct {
	svc    *core.CrossSigningService
	store  *coretest.MemCrossSigningStore
	keys   *core.KeysService
	errs   []error
	master edKey
	self   edKey
	user   edKey
}

func newCrossSigningEnv(t *testing.T) *crossSigningEnv {
	t.Helper()

	env := &crossSigningEnv{
		store:  coretest.NewMemCrossSigningStore(),
		keys:   core.NewKeysService(coretest.NewMemKeysStore(), "test.local"),
		master: newEdKey(t), self: newEdKey(t), user: newEdKey(t),
	}
	env.svc = core.NewCrossSigningService(env.store, env.keys, func(_ context.Context, err error) {
		env.errs = append(env.errs, err)
	})

	return env
}

// upload загружает master, self_signing и user_signing пользователя.
func (env *crossSigningEnv) upload(t *testing.T, userID string) {
	t.Helper()

	masterID := "ed25519:" + env.master.pub

	err := env.svc.UploadKeys(context.Background(), userID, "DEV", core.CrossSigningUpload{
		MasterKey:      rawJSON(t, crossKey(userID, "master", env.master)),
		SelfSigningKey: rawJSON(t, sign(t, crossKey(userID, "self_signing", env.self), userID, masterID, env.master)),
		UserSigningKey: rawJSON(t, sign(t, crossKey(userID, "user_signing", env.user), userID, masterID, env.master)),
	}, func() bool {
		t.Fatal("первая загрузка не требует подтверждения")
		return false
	})
	if err != nil {
		t.Fatalf("загрузка ключей: %v", err)
	}
}

// Загрузка ключей кросс-подписи: проверки ключей, подписи мастер-ключом и
// подтверждения замены мастер-ключа.
func TestCrossSigningUploadKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newCrossSigningEnv(t)
	never := func() bool { return false }

	err := env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{}, never)
	requireKind(t, err, core.ErrMissingParam, "No cross-signing keys provided")

	err = env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{
		SelfSigningKey: rawJSON(t, crossKey(alice, "self_signing", env.self)),
	}, never)
	requireKind(t, err, core.ErrMissingParam, "Must provide master_key")

	err = env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{
		MasterKey: rawJSON(t, crossKey(bob, "master", env.master)),
	}, never)
	requireKind(t, err, core.ErrInvalidParam, "Invalid master_key")

	err = env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{
		MasterKey:      rawJSON(t, crossKey(alice, "master", env.master)),
		SelfSigningKey: rawJSON(t, crossKey(alice, "user_signing", env.self)),
	}, never)
	requireKind(t, err, core.ErrInvalidParam, "Invalid self_signing_key")

	err = env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{
		MasterKey:      rawJSON(t, crossKey(alice, "master", env.master)),
		SelfSigningKey: rawJSON(t, crossKey(alice, "self_signing", env.self)),
	}, never)
	requireKind(t, err, core.ErrInvalidSignature, "self_signing_key is not signed by the master key")

	env.upload(t, alice)

	if len(env.store.Changes) != 1 || env.store.Changes[0] != alice+"|DEV" {
		t.Fatalf("изменение device-листа: %v", env.store.Changes)
	}

	// Повторная загрузка того же мастер-ключа - без подтверждения.
	if err := env.svc.UploadKeys(ctx, alice, "DEV", core.CrossSigningUpload{
		MasterKey: rawJSON(t, crossKey(alice, "master", env.master)),
	}, never); err != nil {
		t.Fatalf("тот же мастер-ключ: %v", err)
	}

	// Новый ключ подписи при настроенной кросс-подписи требует подтверждения
	// (MSC3967); повтор того же ключа - нет.
	masterID := "ed25519:" + env.master.pub
	newSelf := newEdKey(t)
	addSelf := core.CrossSigningUpload{
		SelfSigningKey: rawJSON(t, sign(t, crossKey(alice, "self_signing", newSelf), alice, masterID, env.master)),
	}

	if err := env.svc.UploadKeys(ctx, alice, "DEV", addSelf, never); !errors.Is(err, core.ErrNotConfirmed) {
		t.Fatalf("новый self_signing без подтверждения: %v", err)
	}

	if err := env.svc.UploadKeys(ctx, alice, "DEV", addSelf, func() bool { return true }); err != nil {
		t.Fatalf("новый self_signing с подтверждением: %v", err)
	}

	if err := env.svc.UploadKeys(ctx, alice, "DEV", addSelf, never); err != nil {
		t.Fatalf("повтор того же self_signing: %v", err)
	}

	// Замена мастер-ключа без подтверждения - отказ, с подтверждением - принимается.
	other := newEdKey(t)
	replace := core.CrossSigningUpload{MasterKey: rawJSON(t, crossKey(alice, "master", other))}

	if err := env.svc.UploadKeys(ctx, alice, "DEV", replace, never); !errors.Is(err, core.ErrNotConfirmed) {
		t.Fatalf("замена без подтверждения: %v", err)
	}

	if err := env.svc.UploadKeys(ctx, alice, "DEV", replace, func() bool { return true }); err != nil {
		t.Fatalf("замена с подтверждением: %v", err)
	}

	// Сбой учета изменения не ломает загрузку, а уходит в onRecordError.
	env.store.RecordErr = errors.New("boom")

	if err := env.svc.UploadKeys(ctx, alice, "DEV", replace, never); err != nil || len(env.errs) != 1 {
		t.Fatalf("сбой учета изменения: %v %v", err, env.errs)
	}
}

// Подписи: свое устройство - self_signing, чужой мастер-ключ - user_signing;
// /keys/query подмешивает принятые подписи.
func TestCrossSigningSignaturesAndQuery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newCrossSigningEnv(t)
	env.upload(t, alice)

	bobEnv := newCrossSigningEnv(t)
	bobEnv.store = env.store
	bobEnv.svc = env.svc
	bobEnv.upload(t, bob)

	device := map[string]any{
		"user_id": alice, "device_id": "DEV", "algorithms": []any{"m.olm.v1.curve25519-aes-sha2"},
		"keys": map[string]any{"ed25519:DEV": "devpub"},
	}

	if _, err := env.keys.Upload(ctx, alice, "DEV", core.UploadRequest{DeviceKeys: rawJSON(t, device)}); err != nil {
		t.Fatal(err)
	}

	signedDevice := sign(t, device, alice, "ed25519:"+env.self.pub, env.self)
	bobMaster := sign(t, crossKey(bob, "master", bobEnv.master), alice, "ed25519:"+env.user.pub, env.user)
	tampered := crossKey(bob, "master", bobEnv.master)
	tampered["extra"] = true

	failures, err := env.svc.UploadSignatures(ctx, alice, "DEV", map[string]map[string]json.RawMessage{
		alice: {"DEV": rawJSON(t, signedDevice), "UNKNOWN": rawJSON(t, device), "BAD": json.RawMessage(`[]`)},
		bob:   {bobEnv.master.pub: rawJSON(t, bobMaster)},
	})
	if err != nil {
		t.Fatal(err)
	}

	requireKind(t, failures[alice]["UNKNOWN"], core.ErrNotFound, "Unknown key")
	requireKind(t, failures[alice]["BAD"], core.ErrMalformed, "Invalid signed object")

	if failures[alice]["DEV"] != nil || failures[bob] != nil {
		t.Fatalf("лишние отказы: %v", failures)
	}

	failures, err = env.svc.UploadSignatures(ctx, alice, "DEV", map[string]map[string]json.RawMessage{
		bob: {bobEnv.master.pub: rawJSON(t, tampered)},
	})
	if err != nil {
		t.Fatal(err)
	}

	requireKind(t, failures[bob][bobEnv.master.pub], core.ErrInvalidSignature, "Signed object does not match the stored key")

	// Устройство alice видит подпись self_signing; мастер-ключ bob с подписью
	// alice виден alice, но не постороннему.
	deviceKeys, err := env.keys.Query(ctx, map[string][]string{alice: {}, bob: {}})
	if err != nil {
		t.Fatal(err)
	}

	view, err := env.svc.Query(ctx, alice, deviceKeys, []string{alice, bob})
	if err != nil {
		t.Fatal(err)
	}

	if !hasSignature(t, deviceKeys[alice]["DEV"], alice, "ed25519:"+env.self.pub) {
		t.Fatalf("нет подписи устройства: %s", deviceKeys[alice]["DEV"])
	}

	if !hasSignature(t, view.Master[bob], alice, "ed25519:"+env.user.pub) {
		t.Fatalf("alice не видит своей подписи мастер-ключа bob: %s", view.Master[bob])
	}

	if view.SelfSigning[bob] == nil || view.UserSigning[bob] != nil || view.UserSigning[alice] == nil {
		t.Fatalf("разделы ответа: %+v", view)
	}

	stranger, err := env.svc.Query(ctx, "@carol:test.local", map[string]map[string]json.RawMessage{}, []string{bob})
	if err != nil {
		t.Fatal(err)
	}

	if hasSignature(t, stranger.Master[bob], alice, "ed25519:"+env.user.pub) {
		t.Fatalf("подпись alice видна постороннему: %s", stranger.Master[bob])
	}

	empty, err := env.svc.Query(ctx, alice, nil, nil)
	if err != nil || len(empty.Master) != 0 || empty.SelfSigning == nil || empty.UserSigning == nil {
		t.Fatalf("пустой запрос: %+v %v", empty, err)
	}
}

func hasSignature(t *testing.T, raw json.RawMessage, signer, keyID string) bool {
	t.Helper()

	var obj struct {
		Signatures map[string]map[string]string `json:"signatures"`
	}

	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}

	return obj.Signatures[signer][keyID] != ""
}
