package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Кросс-подписи (CS API «Cross-signing»). Поведение - как в Synapse:
// ключи хранятся по типам, подписи проверяются по ed25519 и хранятся по подписанту,
// /keys/query отдает master/self_signing/user_signing и подмешивает подписи в ключи.
// Логика перенесена из HTTP-обработчиков.

// Шаги, на которых сервис ключей обращается к хранилищу. Транспорт выбирает по
// ним текст ответа 500; исходная ошибка оборачивается через %w.
var (
	// ErrKeysRead - не удалось прочитать ключи кросс-подписи.
	ErrKeysRead = errors.New("core: read keys")
	// ErrDeviceKeysRead - не удалось прочитать ключи устройств.
	ErrDeviceKeysRead = errors.New("core: read device keys")
	// ErrKeysWrite - не удалось сохранить ключи или подписи.
	ErrKeysWrite = errors.New("core: write keys")
	// ErrNotConfirmed - действие требует подтверждения (UIA), и его не дали;
	// ответ клиенту уже отдал тот, кто подтверждал.
	ErrNotConfirmed = errors.New("core: action is not confirmed")
)

// CrossSigningKey - ключ кросс-подписи пользователя (master, self_signing, user_signing).
type CrossSigningKey struct {
	UserID  string
	KeyType string
	// KeyID - открытый ключ ed25519 (base64 без паддинга).
	KeyID string
	Data  json.RawMessage
}

// Signature - подпись ключа target (устройство или кросс-ключ) ключом signer.
type Signature struct {
	TargetUserID string
	TargetKeyID  string
	SignerUserID string
	// SignerKeyID - полный id ключа подписанта, например "ed25519:<base64>" или "ed25519:DEVICE".
	SignerKeyID string
	Signature   string
}

// DeviceListRecorder фиксирует изменение ключей устройства (для /keys/changes и
// device_lists в /sync).
type DeviceListRecorder interface {
	RecordDeviceKeysChange(ctx context.Context, userID, deviceID string) error
}

// CrossSigningStore - хранение ключей кросс-подписи и подписей.
type CrossSigningStore interface {
	DeviceListRecorder
	// CrossSigningKeys - ключи пользователей: user -> тип -> ключ.
	CrossSigningKeys(ctx context.Context, userIDs []string) (map[string]map[string]CrossSigningKey, error)
	// SaveCrossSigningKeys сохраняет ключи одной транзакцией.
	SaveCrossSigningKeys(ctx context.Context, keys []CrossSigningKey) error
	// SaveSignatures сохраняет проверенные подписи.
	SaveSignatures(ctx context.Context, signatures []Signature) error
	// SignaturesFor - подписи над ключами указанных пользователей.
	SignaturesFor(ctx context.Context, targetUserIDs []string) ([]Signature, error)
}

// CrossSigningService - доменный сервис кросс-подписей.
type CrossSigningService struct {
	store CrossSigningStore
	keys  *KeysService
	// onRecordError получает сбой записи изменения device-листа: он не влияет на
	// ответ, ключи уже сохранены.
	onRecordError func(ctx context.Context, err error)
}

// NewCrossSigningService создает сервис. keys нужен для подписей своих устройств;
// onRecordError может быть nil.
func NewCrossSigningService(
	store CrossSigningStore, keys *KeysService, onRecordError func(ctx context.Context, err error),
) *CrossSigningService {
	return &CrossSigningService{store: store, keys: keys, onRecordError: onRecordError}
}

// CrossSigningUpload - тело POST /keys/device_signing/upload (без auth).
type CrossSigningUpload struct {
	MasterKey      json.RawMessage
	SelfSigningKey json.RawMessage
	UserSigningKey json.RawMessage
}

// UploadKeys проверяет и сохраняет ключи кросс-подписи пользователя. Первая
// загрузка мастер-ключа - без подтверждения (MSC3967), замена существующего -
// только если confirmReplace вернул true: иначе украденный токен подменит
// личность. confirmReplace == false - ErrNotConfirmed.
func (s *CrossSigningService) UploadKeys(
	ctx context.Context, userID, deviceID string, req CrossSigningUpload, confirmReplace func() bool,
) error {
	existing, err := s.store.CrossSigningKeys(ctx, []string{userID})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrKeysRead, err)
	}

	current := existing[userID]

	var save []CrossSigningKey

	masterPub := current["master"].KeyID
	// MSC3967 (спека v1.11): ключи уже есть - любое изменение (замена мастера или
	// новый/другой ключ подписи) требует UIA; повтор тех же ключей - нет
	// (Complement msc3967). Раньше UIA требовала только замена мастера.
	setUp := masterPub != ""
	changed := false

	if present(req.MasterKey) {
		_, pub, err := parseCrossKey(req.MasterKey, userID, "master")
		if err != nil {
			return requestError(ErrInvalidParam, "Invalid master_key")
		}

		changed = changed || masterPub != pub
		masterPub = pub
		save = append(save, CrossSigningKey{UserID: userID, KeyType: "master", KeyID: pub, Data: req.MasterKey})
	}

	for _, sub := range []struct {
		raw   json.RawMessage
		usage string
	}{{req.SelfSigningKey, "self_signing"}, {req.UserSigningKey, "user_signing"}} {
		if !present(sub.raw) {
			continue
		}

		if masterPub == "" {
			return requestError(ErrMissingParam, "Must provide master_key")
		}

		obj, pub, err := parseCrossKey(sub.raw, userID, sub.usage)
		if err != nil {
			return requestError(ErrInvalidParam, "Invalid "+sub.usage+"_key")
		}

		if _, valid := verifySignedBy(obj, userID, "ed25519:"+masterPub, masterPub); !valid {
			return requestError(ErrInvalidSignature, sub.usage+"_key is not signed by the master key")
		}

		changed = changed || current[sub.usage].KeyID != pub
		save = append(save, CrossSigningKey{UserID: userID, KeyType: sub.usage, KeyID: pub, Data: sub.raw})
	}

	if len(save) == 0 {
		return requestError(ErrMissingParam, "No cross-signing keys provided")
	}

	if setUp && changed && !confirmReplace() {
		return ErrNotConfirmed
	}

	if err := s.store.SaveCrossSigningKeys(ctx, save); err != nil {
		return fmt.Errorf("%w: %w", ErrKeysWrite, err)
	}

	s.recordChange(ctx, userID, deviceID)

	return nil
}

// UploadSignatures проверяет и сохраняет подписи пользователя me над ключами из
// body (user -> key_id -> подписанный объект). Допустимые подписи (как в Synapse):
// свое устройство - своим self_signing; свой мастер-ключ - ключом своего
// устройства; чужой мастер-ключ - своим user_signing. Подписанный объект должен
// совпадать с сохраненным ключом, подпись - проверяться. Отказы по отдельным
// ключам возвращаются в failures (user -> key_id -> *RequestError).
func (s *CrossSigningService) UploadSignatures(
	ctx context.Context, me, deviceID string, body map[string]map[string]json.RawMessage,
) (failures map[string]map[string]error, err error) {
	users := []string{me}

	for userID := range body {
		if userID != me {
			users = append(users, userID)
		}
	}

	crossKeys, err := s.store.CrossSigningKeys(ctx, users)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeysRead, err)
	}

	devices, err := s.keys.Query(ctx, map[string][]string{me: {}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDeviceKeysRead, err)
	}

	failures = map[string]map[string]error{}
	fail := func(userID, keyID string, kind error, msg string) {
		if failures[userID] == nil {
			failures[userID] = map[string]error{}
		}

		failures[userID][keyID] = requestError(kind, msg)
	}

	var verified []Signature

	for userID, byKey := range body {
		for keyID, raw := range byKey {
			var uploaded map[string]any
			if err := json.Unmarshal(raw, &uploaded); err != nil {
				fail(userID, keyID, ErrMalformed, "Invalid signed object")

				continue
			}

			stored, signerKeys := signatureTarget(userID, keyID, me, crossKeys, devices)
			if stored == nil {
				fail(userID, keyID, ErrNotFound, "Unknown key")

				continue
			}

			// Подписывают ровно сохраненный объект: подмена содержимого - отказ.
			want, err1 := SignedPayload(stored)
			got, err2 := SignedPayload(uploaded)

			if err1 != nil || err2 != nil || want != got {
				fail(userID, keyID, ErrInvalidSignature, "Signed object does not match the stored key")

				continue
			}

			found := 0

			for signerKeyID, pub := range signerKeys {
				sig, valid := verifySignedBy(uploaded, me, signerKeyID, pub)
				if sig == "" {
					continue
				}

				if !valid {
					fail(userID, keyID, ErrInvalidSignature, "Invalid signature by "+signerKeyID)
					found = -1

					break
				}

				verified = append(verified, Signature{
					TargetUserID: userID, TargetKeyID: keyID,
					SignerUserID: me, SignerKeyID: signerKeyID, Signature: sig,
				})
				found++
			}

			if found == 0 {
				fail(userID, keyID, ErrInvalidSignature, "No valid signature from an allowed key")
			}
		}
	}

	if len(verified) > 0 {
		if err := s.store.SaveSignatures(ctx, verified); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrKeysWrite, err)
		}

		s.recordChange(ctx, me, deviceID)
	}

	return failures, nil
}

// CrossSigningKeysView - ключи кросс-подписи для ответа /keys/query.
type CrossSigningKeysView struct {
	Master      map[string]json.RawMessage
	SelfSigning map[string]json.RawMessage
	UserSigning map[string]json.RawMessage
}

// Query дополняет ответ /keys/query ключами кросс-подписи и подписями: подписи
// подмешиваются в deviceKeys на месте. Учитываются только подписи текущими
// ключами подписанта; подписи чужим user_signing над мастер-ключом видны только
// самому подписанту (как в Synapse).
func (s *CrossSigningService) Query(
	ctx context.Context, requester string, deviceKeys map[string]map[string]json.RawMessage, users []string,
) (CrossSigningKeysView, error) {
	view := CrossSigningKeysView{
		Master: map[string]json.RawMessage{}, SelfSigning: map[string]json.RawMessage{},
		UserSigning: map[string]json.RawMessage{},
	}

	if len(users) == 0 {
		return view, nil
	}

	cross, err := s.store.CrossSigningKeys(ctx, append(slices.Clone(users), requester))
	if err != nil {
		return CrossSigningKeysView{}, err
	}

	sigs, err := s.store.SignaturesFor(ctx, users)
	if err != nil {
		return CrossSigningKeysView{}, err
	}

	// Действующие ключи подписантов: устройства из ответа и кросс-ключи.
	validSigner := func(sig Signature) bool {
		pub := strings.TrimPrefix(sig.SignerKeyID, "ed25519:")
		for _, key := range cross[sig.SignerUserID] {
			if key.KeyID == pub {
				return true
			}
		}

		_, isDevice := deviceKeys[sig.SignerUserID][pub]

		return isDevice
	}

	byTarget := map[string][]Signature{}

	for _, sig := range sigs {
		if validSigner(sig) {
			byTarget[sig.TargetUserID+"|"+sig.TargetKeyID] = append(byTarget[sig.TargetUserID+"|"+sig.TargetKeyID], sig)
		}
	}

	merge := func(raw json.RawMessage, userID, keyID string, visible func(Signature) bool) json.RawMessage {
		return mergeSignatures(raw, byTarget[userID+"|"+keyID], visible)
	}

	for userID, devices := range deviceKeys {
		for deviceID, raw := range devices {
			devices[deviceID] = merge(raw, userID, deviceID, nil)
		}
	}

	for _, userID := range users {
		keys := cross[userID]

		if k, ok := keys["master"]; ok {
			view.Master[userID] = merge(k.Data, userID, k.KeyID, func(sig Signature) bool {
				return sig.SignerUserID == userID || sig.SignerUserID == requester
			})
		}

		if k, ok := keys["self_signing"]; ok {
			view.SelfSigning[userID] = k.Data
		}

		if k, ok := keys["user_signing"]; ok && userID == requester {
			view.UserSigning[userID] = k.Data
		}
	}

	return view, nil
}

// recordChange фиксирует изменение ключей; сбой отдается onRecordError.
func (s *CrossSigningService) recordChange(ctx context.Context, userID, deviceID string) {
	if err := s.store.RecordDeviceKeysChange(ctx, userID, deviceID); err != nil && s.onRecordError != nil {
		s.onRecordError(ctx, err)
	}
}

// mergeSignatures добавляет подписи extra (с фильтром visible) в signatures объекта.
func mergeSignatures(raw json.RawMessage, extra []Signature, visible func(Signature) bool) json.RawMessage {
	if len(extra) == 0 {
		return raw
	}

	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}

	all, _ := obj["signatures"].(map[string]any)
	if all == nil {
		all = map[string]any{}
	}

	for _, sig := range extra {
		if visible != nil && !visible(sig) {
			continue
		}

		byUser, _ := all[sig.SignerUserID].(map[string]any)
		if byUser == nil {
			byUser = map[string]any{}
		}

		byUser[sig.SignerKeyID] = sig.Signature
		all[sig.SignerUserID] = byUser
	}

	obj["signatures"] = all

	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}

	return out
}

// present - ключ передан (не пустой и не null).
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

var errBadCrossKey = errors.New("core: invalid cross-signing key")

// parseCrossKey проверяет ключ кросс-подписи: владелец, назначение и ровно один
// ключ ed25519. Возвращает объект и открытый ключ (base64).
func parseCrossKey(raw json.RawMessage, userID, usage string) (obj map[string]any, pub string, err error) {
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "", errBadCrossKey
	}

	if owner, _ := obj["user_id"].(string); owner != userID {
		return nil, "", errBadCrossKey
	}

	usages, _ := obj["usage"].([]any)
	if !slices.ContainsFunc(usages, func(u any) bool { return u == usage }) {
		return nil, "", errBadCrossKey
	}

	keys, _ := obj["keys"].(map[string]any)
	if len(keys) != 1 {
		return nil, "", errBadCrossKey
	}

	for keyID, value := range keys {
		pub, _ = value.(string)
		if !strings.HasPrefix(keyID, "ed25519:") || pub == "" || strings.TrimPrefix(keyID, "ed25519:") != pub {
			return nil, "", errBadCrossKey
		}

		return obj, pub, nil
	}

	return nil, "", errBadCrossKey
}

// SignedPayload - канонический JSON объекта без signatures и unsigned (то, что подписывают).
func SignedPayload(obj map[string]any) (string, error) {
	clean := make(map[string]any, len(obj))

	for k, v := range obj {
		if k != "signatures" && k != "unsigned" {
			clean[k] = v
		}
	}

	raw, err := signing.CanonicalJSON(clean)

	return string(raw), err
}

// signatureOf достает подпись signerUser ключом keyID из объекта.
func signatureOf(obj map[string]any, signerUser, keyID string) string {
	sigs, _ := obj["signatures"].(map[string]any)
	byUser, _ := sigs[signerUser].(map[string]any)
	sig, _ := byUser[keyID].(string)

	return sig
}

// verifySignedBy проверяет подпись объекта ключом signerUser/ed25519:pub.
func verifySignedBy(obj map[string]any, signerUser, keyID, pub string) (string, bool) {
	sig := signatureOf(obj, signerUser, keyID)
	if sig == "" {
		return "", false
	}

	payload, err := SignedPayload(obj)
	if err != nil {
		return "", false
	}

	return sig, signing.VerifyEd25519(pub, payload, sig) == nil
}

// signatureTarget находит сохраненный объект, который подписывают, и ключи,
// которыми его вправе подписать текущий пользователь (id ключа -> открытый ключ).
func signatureTarget(
	userID, keyID, me string,
	crossKeys map[string]map[string]CrossSigningKey,
	myDevices map[string]map[string]json.RawMessage,
) (stored map[string]any, signers map[string]string) {
	decode := func(raw json.RawMessage) map[string]any {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return nil
		}

		return obj
	}

	if userID != me {
		master, usk := crossKeys[userID]["master"], crossKeys[me]["user_signing"]
		if master.KeyID != keyID || usk.KeyID == "" {
			return nil, nil
		}

		return decode(master.Data), map[string]string{"ed25519:" + usk.KeyID: usk.KeyID}
	}

	if device, ok := myDevices[me][keyID]; ok {
		ssk := crossKeys[me]["self_signing"]
		if ssk.KeyID == "" {
			return nil, nil
		}

		return decode(device), map[string]string{"ed25519:" + ssk.KeyID: ssk.KeyID}
	}

	if master := crossKeys[me]["master"]; master.KeyID == keyID {
		signers = map[string]string{}

		for deviceID, raw := range myDevices[me] {
			if pub := deviceEd25519(decode(raw), deviceID); pub != "" {
				signers["ed25519:"+deviceID] = pub
			}
		}

		return decode(master.Data), signers
	}

	return nil, nil
}

// deviceEd25519 - открытый ключ ed25519 устройства из его device_keys.
func deviceEd25519(obj map[string]any, deviceID string) string {
	keys, _ := obj["keys"].(map[string]any)
	pub, _ := keys["ed25519:"+deviceID].(string)

	return pub
}
