package transport

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aisa-it/syngo/internal/federation/signing"
)

// Ошибки получения ключей серверов.
var (
	ErrBadKeyResponse = errors.New("transport: bad server keys response")
	ErrKeyNotFound    = errors.New("transport: server key not found")
)

// MaxKeyValidity - спека: ключ считается действительным не дольше valid_until_ts
// и не дольше 7 дней от момента получения (иначе отозвать ключ нельзя).
const MaxKeyValidity = 7 * 24 * time.Hour

// Параметры кэша KeyFetcher по умолчанию.
const (
	defaultKeyNegativeTTL  = 30 * time.Second
	defaultKeyRefetchDelay = 10 * time.Second
)

// ServerKeys - проверенный ответ /_matrix/key/v2/server.
type ServerKeys struct {
	ServerName string
	// ValidUntilTS - срок verify_keys, уже ограниченный MaxKeyValidity.
	ValidUntilTS  int64
	VerifyKeys    map[string]string
	OldVerifyKeys map[string]OldVerifyKey
}

// OldVerifyKey - ключ из old_verify_keys: действителен для подписей до ExpiredTS.
type OldVerifyKey struct {
	Key       string
	ExpiredTS int64
}

// Lookup ищет ключ: verify_keys (срок ValidUntilTS) или old_verify_keys (срок expired_ts).
func (k ServerKeys) Lookup(keyID string) (publicKey string, validUntil int64, ok bool) {
	if key, found := k.VerifyKeys[keyID]; found {
		return key, k.ValidUntilTS, true
	}

	if old, found := k.OldVerifyKeys[keyID]; found {
		return old.Key, old.ExpiredTS, true
	}

	return "", 0, false
}

// TrustedKeyServer - нотарий (trusted_key_servers Synapse). VerifyKeys пуст -
// ключи нотария запрашиваются у него напрямую.
type TrustedKeyServer struct {
	ServerName string
	VerifyKeys map[string]string
}

// ParseServerKeys разбирает и проверяет ответ с ключами сервера serverName:
// server_name совпадает, valid_until_ts задан, каждая подпись serverName ключом
// из verify_keys верна и есть хотя бы одна (как process_v2_response Synapse).
// old_verify_keys разбираются со своим expired_ts.
func ParseServerKeys(raw []byte, serverName string, now time.Time) (ServerKeys, error) {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return ServerKeys{}, fmt.Errorf("%w: %w", ErrBadKeyResponse, err)
	}

	return parseServerKeysObject(raw, object, serverName, now)
}

func parseServerKeysObject(raw []byte, object map[string]any, serverName string, now time.Time) (ServerKeys, error) {
	var parsed struct {
		ServerName   string `json:"server_name"`
		ValidUntilTS int64  `json:"valid_until_ts"`
		VerifyKeys   map[string]struct {
			Key string `json:"key"`
		} `json:"verify_keys"`
		OldVerifyKeys map[string]struct {
			Key       string `json:"key"`
			ExpiredTS int64  `json:"expired_ts"`
		} `json:"old_verify_keys"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ServerKeys{}, fmt.Errorf("%w: %w", ErrBadKeyResponse, err)
	}

	if parsed.ServerName != serverName {
		return ServerKeys{}, fmt.Errorf("%w: server_name %q != %q", ErrBadKeyResponse, parsed.ServerName, serverName)
	}

	if parsed.ValidUntilTS <= 0 {
		return ServerKeys{}, fmt.Errorf("%w: no valid_until_ts", ErrBadKeyResponse)
	}

	keys := ServerKeys{
		ServerName:    serverName,
		ValidUntilTS:  min(parsed.ValidUntilTS, now.Add(MaxKeyValidity).UnixMilli()),
		VerifyKeys:    map[string]string{},
		OldVerifyKeys: map[string]OldVerifyKey{},
	}

	for keyID, value := range parsed.VerifyKeys {
		if validEd25519Key(keyID, value.Key) {
			keys.VerifyKeys[keyID] = value.Key
		}
	}

	for keyID, value := range parsed.OldVerifyKeys {
		if validEd25519Key(keyID, value.Key) && value.ExpiredTS > 0 {
			keys.OldVerifyKeys[keyID] = OldVerifyKey{Key: value.Key, ExpiredTS: value.ExpiredTS}
		}
	}

	if err := verifyAllSignatures(object, serverName, keys.VerifyKeys); err != nil {
		return ServerKeys{}, err
	}

	return keys, nil
}

// validEd25519Key - key_id вида ed25519:<version> и 32 байта ключа.
func validEd25519Key(keyID, key string) bool {
	if !strings.HasPrefix(keyID, "ed25519:") || len(keyID) == len("ed25519:") {
		return false
	}

	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(key, "="))

	return err == nil && len(decoded) == ed25519.PublicKeySize
}

// verifyAllSignatures: каждая подпись signer ключом из keys должна сходиться,
// и хотя бы одна должна быть.
func verifyAllSignatures(object map[string]any, signer string, keys map[string]string) error {
	signatures, ok := object["signatures"].(map[string]any)
	if !ok {
		return signing.ErrNoSignature
	}

	serverSigs, ok := signatures[signer].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: no signature from %s", signing.ErrNoSignature, signer)
	}

	unsigned := make(map[string]any, len(object))

	for k, v := range object {
		if k == "signatures" || k == "unsigned" {
			continue
		}

		unsigned[k] = v
	}

	canonical, err := signing.CanonicalJSON(unsigned)
	if err != nil {
		return err
	}

	verified := false

	for keyID, sigValue := range serverSigs {
		publicKey, found := keys[keyID]
		if !found {
			continue
		}

		sig, ok := sigValue.(string)
		if !ok {
			return signing.ErrInvalidSig
		}

		if err := signing.VerifyEd25519(publicKey, string(canonical), sig); err != nil {
			return err
		}

		verified = true
	}

	if !verified {
		return fmt.Errorf("%w: no signature from %s by its verify_keys", signing.ErrNoSignature, signer)
	}

	return nil
}

// KeyFetcher получает ключи серверов: сначала у нотариев (trusted_key_servers),
// затем у самого сервера через резолвер (well-known/SRV) и защищенный транспорт.
//
// Одновременные запросы одних ключей сливаются (singleflight); неудача
// запоминается на NegativeTTL, успешный ответ - на RefetchDelay: поток запросов
// с неизвестными key_id не превращается в поток исходящих запросов.
type KeyFetcher struct {
	Client            *Client
	TrustedKeyServers []TrustedKeyServer
	// NegativeTTL - сколько помнить неудачу (0 - 30 секунд).
	NegativeTTL time.Duration
	// RefetchDelay - сколько не перезапрашивать после успеха (0 - 10 секунд).
	RefetchDelay time.Duration
	Now          func() time.Time

	group  singleflight.Group
	mu     sync.Mutex
	recent map[string]recentFetch
}

type recentFetch struct {
	keys     ServerKeys
	err      error
	expireAt time.Time
}

// Fetch возвращает ключи serverName, среди которых ищется keyID: нотарий, не
// знающий keyID, пропускается. Слияние и кэш - по serverName: ответ содержит все
// ключи сервера, и перебор key_id не размножает исходящие запросы.
func (f *KeyFetcher) Fetch(ctx context.Context, serverName, keyID string) (ServerKeys, error) {
	cacheKey := serverName

	if cached, ok := f.cached(cacheKey); ok {
		return cached.keys, cached.err
	}

	// Запрос не привязан к отмене первого вызывающего: его результат ждут все.
	fetchCtx := context.WithoutCancel(ctx)

	value, err, _ := f.group.Do(cacheKey, func() (any, error) {
		keys, err := f.fetch(fetchCtx, serverName, keyID)
		f.remember(cacheKey, keys, err)

		return keys, err
	})

	keys, _ := value.(ServerKeys)

	return keys, err
}

func (f *KeyFetcher) fetch(ctx context.Context, serverName, keyID string) (ServerKeys, error) {
	var errs []error

	for _, notary := range f.TrustedKeyServers {
		if notary.ServerName == serverName {
			continue
		}

		keys, err := f.fromNotary(ctx, notary, serverName, keyID)
		if err == nil {
			if _, _, ok := keys.Lookup(keyID); ok || keyID == "" {
				return keys, nil
			}

			err = fmt.Errorf("%w: notary %s: %s %s", ErrKeyNotFound, notary.ServerName, serverName, keyID)
		}

		errs = append(errs, err)
	}

	keys, err := f.Direct(ctx, serverName)
	if err != nil {
		return ServerKeys{}, errors.Join(append(errs, err)...)
	}

	return keys, nil
}

// Direct - GET /_matrix/key/v2/server у самого сервера (адрес - через резолвер).
func (f *KeyFetcher) Direct(ctx context.Context, serverName string) (ServerKeys, error) {
	if f.Client == nil {
		return ServerKeys{}, errors.New("transport: no federation client for keys")
	}

	raw, status, err := f.Client.Do(ctx, serverName, http.MethodGet, "/_matrix/key/v2/server", nil,
		RequestOptions{Unsigned: true, IgnoreBackoff: true})
	if err != nil {
		return ServerKeys{}, fmt.Errorf("transport: keys from %s: %w", serverName, err)
	}

	if status != http.StatusOK {
		return ServerKeys{}, fmt.Errorf("transport: keys from %s: HTTP %d", serverName, status)
	}

	return ParseServerKeys(raw, serverName, f.now())
}

// fromNotary - POST /_matrix/key/v2/query у нотария: каждый объект ответа
// подписан и самим сервером, и нотарием.
func (f *KeyFetcher) fromNotary(ctx context.Context, notary TrustedKeyServer, serverName, keyID string) (ServerKeys, error) {
	if f.Client == nil {
		return ServerKeys{}, errors.New("transport: no federation client for keys")
	}

	notaryKeys := notary.VerifyKeys
	if len(notaryKeys) == 0 {
		direct, err := f.Direct(ctx, notary.ServerName)
		if err != nil {
			return ServerKeys{}, fmt.Errorf("transport: notary %s keys: %w", notary.ServerName, err)
		}

		notaryKeys = direct.VerifyKeys
	}

	criteria := map[string]any{}
	if keyID != "" {
		criteria[keyID] = map[string]any{"minimum_valid_until_ts": f.now().UnixMilli()}
	}

	body, err := json.Marshal(map[string]any{"server_keys": map[string]any{serverName: criteria}})
	if err != nil {
		return ServerKeys{}, err
	}

	raw, status, err := f.Client.Do(ctx, notary.ServerName, http.MethodPost, "/_matrix/key/v2/query", body,
		RequestOptions{Unsigned: true, IgnoreBackoff: true})
	if err != nil {
		return ServerKeys{}, fmt.Errorf("transport: notary %s: %w", notary.ServerName, err)
	}

	if status != http.StatusOK {
		return ServerKeys{}, fmt.Errorf("transport: notary %s: HTTP %d", notary.ServerName, status)
	}

	var parsed struct {
		ServerKeys []json.RawMessage `json:"server_keys"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ServerKeys{}, fmt.Errorf("%w: notary %s: %w", ErrBadKeyResponse, notary.ServerName, err)
	}

	merged := ServerKeys{ServerName: serverName, VerifyKeys: map[string]string{}, OldVerifyKeys: map[string]OldVerifyKey{}}
	found := false

	for _, item := range parsed.ServerKeys {
		var object map[string]any
		if err := json.Unmarshal(item, &object); err != nil {
			continue
		}

		if name, _ := object["server_name"].(string); name != serverName {
			continue
		}

		keys, err := parseServerKeysObject(item, object, serverName, f.now())
		if err != nil {
			return ServerKeys{}, fmt.Errorf("transport: notary %s: %w", notary.ServerName, err)
		}

		if err := verifyAllSignatures(object, notary.ServerName, notaryKeys); err != nil {
			return ServerKeys{}, fmt.Errorf("transport: notary %s signature: %w", notary.ServerName, err)
		}

		found = true
		merged.ValidUntilTS = max(merged.ValidUntilTS, keys.ValidUntilTS)

		for id, key := range keys.VerifyKeys {
			merged.VerifyKeys[id] = key
		}

		for id, key := range keys.OldVerifyKeys {
			merged.OldVerifyKeys[id] = key
		}
	}

	if !found {
		return ServerKeys{}, fmt.Errorf("%w: notary %s has no keys for %s", ErrKeyNotFound, notary.ServerName, serverName)
	}

	return merged, nil
}

func (f *KeyFetcher) cached(cacheKey string) (recentFetch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entry, ok := f.recent[cacheKey]
	if !ok || !f.now().Before(entry.expireAt) {
		return recentFetch{}, false
	}

	return entry, true
}

func (f *KeyFetcher) remember(cacheKey string, keys ServerKeys, err error) {
	ttl := f.RefetchDelay
	if ttl <= 0 {
		ttl = defaultKeyRefetchDelay
	}

	if err != nil {
		ttl = f.NegativeTTL
		if ttl <= 0 {
			ttl = defaultKeyNegativeTTL
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.recent == nil {
		f.recent = map[string]recentFetch{}
	}

	now := f.now()

	// Чистка просроченных: карта не растет от потока разных key_id.
	for key, entry := range f.recent {
		if !now.Before(entry.expireAt) {
			delete(f.recent, key)
		}
	}

	f.recent[cacheKey] = recentFetch{keys: keys, err: err, expireAt: now.Add(ttl)}
}

func (f *KeyFetcher) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}

	return f.Now()
}
