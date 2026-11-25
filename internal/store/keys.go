package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// KeysStore — хранение E2EE-ключей устройств.
//
// Сервер не умеет расшифровывать сообщения и не хранит приватные ключи: только
// публичные device keys, one-time keys, которые выдаются ровно один раз, и fallback
// keys на случай, когда one-time keys кончились.
type KeysStore struct {
	db *DB
}

var (
	_ core.KeysStore     = (*KeysStore)(nil)
	_ core.SyncKeysStore = (*KeysStore)(nil)
)

// NewKeysStore создаёт хранилище ключей.
func NewKeysStore(db *DB) *KeysStore {
	return &KeysStore{db: db}
}

// UpsertDeviceKeys сохраняет ключи устройства (перезапись при повторной загрузке).
func (s *KeysStore) UpsertDeviceKeys(ctx context.Context, userID, deviceID string, keys json.RawMessage) error {
	const query = `
		INSERT INTO device_keys (user_id, device_id, key_data)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, device_id) DO UPDATE
		SET key_data = EXCLUDED.key_data, updated_at = now()`

	if _, err := s.db.pool.Exec(ctx, query, userID, deviceID, string(keys)); err != nil {
		return fmt.Errorf("store: upsert device keys %s/%s: %w", userID, deviceID, err)
	}

	return nil
}

// UploadOneTimeKeys добавляет one-time keys; уже известные key_id не перезаписываются
// (клиент не должен «перетирать» выданный ключ).
func (s *KeysStore) UploadOneTimeKeys(
	ctx context.Context,
	userID, deviceID string,
	keys map[string]json.RawMessage,
) error {
	if len(keys) == 0 {
		return nil
	}

	// Одна вставка с ключами в порядке id: Element шлет /keys/upload параллельно, и при
	// вставке по одному в порядке обхода map две транзакции брали блокировки строк в
	// разном порядке - deadlock (SQLSTATE 40P01) и 500 клиенту.
	ids := make([]string, 0, len(keys))
	for keyID := range keys {
		ids = append(ids, keyID)
	}

	sort.Strings(ids)

	algorithms := make([]string, len(ids))
	data := make([]string, len(ids))

	for i, keyID := range ids {
		algorithm, _, found := strings.Cut(keyID, ":")
		if !found || algorithm == "" {
			return fmt.Errorf("store: bad one-time key id %q: %w", keyID, core.ErrBadJSON)
		}

		algorithms[i] = algorithm
		data[i] = string(keys[keyID])
	}

	const query = `
		INSERT INTO one_time_keys (user_id, device_id, key_id, algorithm, key_data)
		SELECT $1, $2, k.key_id, k.algorithm, k.key_data::jsonb
		FROM unnest($3::text[], $4::text[], $5::text[]) WITH ORDINALITY AS k (key_id, algorithm, key_data, n)
		ORDER BY k.n
		ON CONFLICT (user_id, device_id, key_id) DO NOTHING`

	if _, err := s.db.pool.Exec(ctx, query, userID, deviceID, ids, algorithms, data); err != nil {
		return fmt.Errorf("store: insert one-time keys: %w", err)
	}

	return nil
}

// UploadFallbackKeys сохраняет fallback keys устройства: один ключ на
// алгоритм. Тот же ключ повторно (клиент присылает его при каждой загрузке) не
// снимает пометку used; другой ключ заменяет прежний и считается неиспользованным,
// как в Synapse (set_e2e_fallback_keys).
func (s *KeysStore) UploadFallbackKeys(
	ctx context.Context,
	userID, deviceID string,
	keys map[string]json.RawMessage,
) error {
	if len(keys) == 0 {
		return nil
	}

	// По алгоритму в порядке id: при нескольких ключах одного алгоритма побеждает
	// последний, вставка одним запросом (без deadlock параллельных загрузок).
	ids := make([]string, 0, len(keys))
	for keyID := range keys {
		ids = append(ids, keyID)
	}

	sort.Strings(ids)

	byAlgorithm := map[string]int{}
	algorithms := make([]string, 0, len(ids))
	keyIDs := make([]string, 0, len(ids))
	data := make([]string, 0, len(ids))

	for _, keyID := range ids {
		algorithm, _, found := strings.Cut(keyID, ":")
		if !found || algorithm == "" {
			return fmt.Errorf("store: bad fallback key id %q: %w", keyID, core.ErrBadJSON)
		}

		if !json.Valid(keys[keyID]) {
			return fmt.Errorf("store: bad fallback key %q: %w", keyID, core.ErrBadJSON)
		}

		if i, ok := byAlgorithm[algorithm]; ok {
			keyIDs[i], data[i] = keyID, string(keys[keyID])

			continue
		}

		byAlgorithm[algorithm] = len(algorithms)
		algorithms = append(algorithms, algorithm)
		keyIDs = append(keyIDs, keyID)
		data = append(data, string(keys[keyID]))
	}

	const query = `
		INSERT INTO e2e_fallback_keys (user_id, device_id, algorithm, key_id, key_data)
		SELECT $1, $2, k.algorithm, k.key_id, k.key_data::jsonb
		FROM unnest($3::text[], $4::text[], $5::text[]) WITH ORDINALITY AS k (algorithm, key_id, key_data, n)
		ORDER BY k.n
		ON CONFLICT (user_id, device_id, algorithm) DO UPDATE
		SET key_id = EXCLUDED.key_id, key_data = EXCLUDED.key_data, used = false, updated_at = now()
		WHERE e2e_fallback_keys.key_id <> EXCLUDED.key_id OR e2e_fallback_keys.key_data <> EXCLUDED.key_data`

	if _, err := s.db.pool.Exec(ctx, query, userID, deviceID, algorithms, keyIDs, data); err != nil {
		return fmt.Errorf("store: upsert fallback keys: %w", err)
	}

	return nil
}

// UnusedFallbackKeyTypes - алгоритмы fallback keys устройства, которые еще не
// выдавались (device_unused_fallback_key_types в /sync).
func (s *KeysStore) UnusedFallbackKeyTypes(ctx context.Context, userID, deviceID string) ([]string, error) {
	rows, err := s.db.pool.Query(ctx, `
		SELECT algorithm FROM e2e_fallback_keys
		WHERE user_id = $1 AND device_id = $2 AND NOT used
		ORDER BY algorithm`, userID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: unused fallback keys: %w", err)
	}

	types, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: scan unused fallback keys: %w", err)
	}

	return types, nil
}

// OneTimeKeyCounts возвращает число невыданных key по алгоритмам для устройства.
func (s *KeysStore) OneTimeKeyCounts(
	ctx context.Context,
	userID, deviceID string,
) (map[string]int, error) {
	const query = `
		SELECT algorithm, count(*) FROM one_time_keys
		WHERE user_id = $1 AND device_id = $2 AND claimed_at IS NULL
		GROUP BY algorithm`

	rows, err := s.db.pool.Query(ctx, query, userID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: otk counts: %w", err)
	}

	defer rows.Close()

	counts := map[string]int{}

	for rows.Next() {
		var (
			algorithm string
			count     int
		)

		if err := rows.Scan(&algorithm, &count); err != nil {
			return nil, fmt.Errorf("store: scan otk count: %w", err)
		}

		counts[algorithm] = count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate otk counts: %w", err)
	}

	return counts, nil
}

// QueryDeviceKeys отдаёт ключи запрошенных устройств (одним запросом).
func (s *KeysStore) QueryDeviceKeys(
	ctx context.Context,
	requests map[string][]string,
) (map[string]map[string]json.RawMessage, error) {
	if len(requests) == 0 {
		return map[string]map[string]json.RawMessage{}, nil
	}

	// Пары (пользователь, устройство); пустое устройство - все устройства пользователя.
	// Раньше списки устройств всех пользователей склеивались в один фильтр, и запрос
	// {alice: [], bob: [BDEV]} не возвращал устройства alice.
	users := make([]string, 0, len(requests))
	devices := make([]string, 0, len(requests))

	for userID, deviceIDs := range requests {
		if len(deviceIDs) == 0 {
			users = append(users, userID)
			devices = append(devices, "")

			continue
		}

		for _, deviceID := range deviceIDs {
			users = append(users, userID)
			devices = append(devices, deviceID)
		}
	}

	const query = `
		SELECT DISTINCT d.user_id, d.device_id, d.key_data
		FROM device_keys d
		JOIN unnest($1::text[], $2::text[]) AS q (user_id, device_id)
		  ON d.user_id = q.user_id AND (q.device_id = '' OR d.device_id = q.device_id)`

	rows, err := s.db.pool.Query(ctx, query, users, devices)
	if err != nil {
		return nil, fmt.Errorf("store: query device keys: %w", err)
	}

	defer rows.Close()

	result := make(map[string]map[string]json.RawMessage, len(requests))

	for rows.Next() {
		var (
			userID   string
			deviceID string
			keyData  []byte
		)

		if err := rows.Scan(&userID, &deviceID, &keyData); err != nil {
			return nil, fmt.Errorf("store: scan device keys: %w", err)
		}

		if result[userID] == nil {
			result[userID] = map[string]json.RawMessage{}
		}

		result[userID][deviceID] = keyData
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate device keys: %w", err)
	}

	return result, nil
}

// ClaimOneTimeKeys атомарно выдаёт по одному ключу на (user, device, algorithm);
// если one-time keys устройства кончились - fallback key.
// algorithm "*" означает «любой алгоритм» (wildcard из спеки/MSC): берём самый
// старый невыданный ключ устройства независимо от алгоритма.
// Ключ помечается claimed_at в том же UPDATE, поэтому два параллельных запроса не
// получат один и тот же ключ (FOR UPDATE SKIP LOCKED).
func (s *KeysStore) ClaimOneTimeKeys(
	ctx context.Context,
	requests map[string]map[string]string,
) (map[string]map[string]map[string]json.RawMessage, error) {
	result := make(map[string]map[string]map[string]json.RawMessage, len(requests))

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin claim: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Ключ ищем и помечаем по ПОЛНОМУ ключу строки: key_id у libolm/vodozemac —
	// счётчик, одинаковый у всех устройств, поэтому «WHERE key_id = (SELECT …)»
	// без user_id/device_id помечал выданными ключи чужих устройств и RETURNING
	// отдавал несколько строк, из которых Scan брал первую.
	const query = `
		UPDATE one_time_keys SET claimed_at = now()
		WHERE (user_id, device_id, key_id) = (
			SELECT user_id, device_id, key_id FROM one_time_keys
			WHERE user_id = $1 AND device_id = $2
			  AND ($3 = '*' OR algorithm = $3) AND claimed_at IS NULL
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING key_id, key_data`

	// One-time keys кончились - выдаем fallback key алгоритма и помечаем выданным.
	// Ключ не удаляется: он отдается повторно, пока устройство не загрузит новый
	// (спецификация).
	const fallbackQuery = `
		UPDATE e2e_fallback_keys SET used = true
		WHERE (user_id, device_id, algorithm) = (
			SELECT user_id, device_id, algorithm FROM e2e_fallback_keys
			WHERE user_id = $1 AND device_id = $2 AND ($3 = '*' OR algorithm = $3)
			ORDER BY algorithm
			LIMIT 1
		)
		RETURNING key_id, key_data`

	for userID, devices := range requests {
		for deviceID, algorithm := range devices {
			var (
				keyID   string
				keyData []byte
			)

			err := tx.QueryRow(ctx, query, userID, deviceID, algorithm).Scan(&keyID, &keyData)
			if errors.Is(err, pgx.ErrNoRows) {
				err = tx.QueryRow(ctx, fallbackQuery, userID, deviceID, algorithm).Scan(&keyID, &keyData)
			}

			if err != nil {
				// Нет ключей — штатная ситуация: устройство не отдаём.
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}

				// Остальные ошибки не глотаем: иначе клиент получает «частичный»
				// успех при сломанной БД.
				return nil, fmt.Errorf("store: claim one-time key %s/%s: %w", userID, deviceID, err)
			}

			if result[userID] == nil {
				result[userID] = map[string]map[string]json.RawMessage{}
			}

			if result[userID][deviceID] == nil {
				result[userID][deviceID] = map[string]json.RawMessage{}
			}

			result[userID][deviceID][keyID] = keyData
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit claim: %w", err)
	}

	return result, nil
}
