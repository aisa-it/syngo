package store

import (
	"context"
	"fmt"

	"github.com/aisa-it/syngo/internal/core"
)

// CrossSigningKey — ключ кросс-подписи пользователя (master, self_signing, user_signing).
// Типы объявлены в core: store реализует core.CrossSigningStore.
type CrossSigningKey = core.CrossSigningKey

// Signature — подпись ключа target (устройство или кросс-ключ) ключом signer.
type Signature = core.Signature

var _ core.CrossSigningStore = (*DB)(nil)

// CrossSigningKeys возвращает ключи кросс-подписи пользователей: user -> тип -> ключ.
//
// Ключи отслеживаемых пользователей других серверов - из кэша: так
// /keys/query отдает их с подписями наших пользователей, а /keys/signatures/upload
// принимает подпись чужого мастер-ключа своим user_signing.
func (db *DB) CrossSigningKeys(ctx context.Context, userIDs []string) (map[string]map[string]CrossSigningKey, error) {
	out := make(map[string]map[string]CrossSigningKey, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}

	const query = `
		SELECT user_id, key_type, key_id, key_data FROM e2e_cross_signing_keys
		WHERE user_id = ANY($1)
		UNION ALL
		SELECT user_id, key_type, key_id, key_data FROM remote_cross_signing_keys
		WHERE user_id = ANY($1)`

	rows, err := db.pool.Query(ctx, query, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: cross-signing keys: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var key CrossSigningKey

		if err := rows.Scan(&key.UserID, &key.KeyType, &key.KeyID, &key.Data); err != nil {
			return nil, fmt.Errorf("store: scan cross-signing key: %w", err)
		}

		if out[key.UserID] == nil {
			out[key.UserID] = map[string]CrossSigningKey{}
		}

		out[key.UserID][key.KeyType] = key
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate cross-signing keys: %w", err)
	}

	return out, nil
}

// SaveCrossSigningKeys сохраняет ключи кросс-подписи пользователя одной транзакцией.
// Подписи прежними ключами не удаляются: при выдаче в /keys/query учитываются только
// подписи текущими ключами подписанта, устаревшие отбрасываются там.
func (db *DB) SaveCrossSigningKeys(ctx context.Context, keys []CrossSigningKey) error {
	if len(keys) == 0 {
		return nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin cross-signing: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	const query = `
		INSERT INTO e2e_cross_signing_keys (user_id, key_type, key_id, key_data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, key_type) DO UPDATE
		SET key_id = EXCLUDED.key_id, key_data = EXCLUDED.key_data, updated_at = now()`

	for _, key := range keys {
		if _, err := tx.Exec(ctx, query, key.UserID, key.KeyType, key.KeyID, []byte(key.Data)); err != nil {
			return fmt.Errorf("store: save cross-signing key %s: %w", key.KeyType, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit cross-signing: %w", err)
	}

	return nil
}

// SaveSignatures сохраняет проверенные подписи (повторная подпись тем же ключом заменяет прежнюю).
func (db *DB) SaveSignatures(ctx context.Context, signatures []Signature) error {
	const query = `
		INSERT INTO e2e_signatures (target_user_id, target_key_id, signer_user_id, signer_key_id, signature)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (target_user_id, target_key_id, signer_user_id, signer_key_id)
		DO UPDATE SET signature = EXCLUDED.signature, updated_at = now()`

	for _, sig := range signatures {
		if _, err := db.pool.Exec(ctx, query, sig.TargetUserID, sig.TargetKeyID,
			sig.SignerUserID, sig.SignerKeyID, sig.Signature); err != nil {
			return fmt.Errorf("store: save signature: %w", err)
		}
	}

	return nil
}

// SignaturesFor возвращает подписи над ключами указанных пользователей.
func (db *DB) SignaturesFor(ctx context.Context, targetUserIDs []string) ([]Signature, error) {
	if len(targetUserIDs) == 0 {
		return nil, nil
	}

	const query = `
		SELECT target_user_id, target_key_id, signer_user_id, signer_key_id, signature
		FROM e2e_signatures WHERE target_user_id = ANY($1)`

	rows, err := db.pool.Query(ctx, query, targetUserIDs)
	if err != nil {
		return nil, fmt.Errorf("store: signatures: %w", err)
	}

	defer rows.Close()

	var out []Signature

	for rows.Next() {
		var sig Signature
		if err := rows.Scan(&sig.TargetUserID, &sig.TargetKeyID, &sig.SignerUserID, &sig.SignerKeyID, &sig.Signature); err != nil {
			return nil, fmt.Errorf("store: scan signature: %w", err)
		}

		out = append(out, sig)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate signatures: %w", err)
	}

	return out, nil
}
