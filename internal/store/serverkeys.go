package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CachedServerKey возвращает закэшированный публичный ключ сервера.
func (db *DB) CachedServerKey(ctx context.Context, serverName, keyID string) (publicKey string, validUntil int64, ok bool, err error) {
	const query = `
		SELECT verify_key, valid_until_ts FROM server_signing_keys
		WHERE server_name = $1 AND key_id = $2`

	err = db.pool.QueryRow(ctx, query, serverName, keyID).Scan(&publicKey, &validUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, false, nil
		}

		return "", 0, false, fmt.Errorf("store: cached server key: %w", err)
	}

	return publicKey, validUntil, true, nil
}

// SaveServerKeys сохраняет набор ключей сервера (перезапись по (server,key_id)).
func (db *DB) SaveServerKeys(ctx context.Context, serverName string, keys map[string]string, validUntil int64) error {
	if len(keys) == 0 {
		return nil
	}

	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: save server keys begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	const query = `
		INSERT INTO server_signing_keys (server_name, key_id, verify_key, valid_until_ts)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (server_name, key_id)
		DO UPDATE SET verify_key = EXCLUDED.verify_key, valid_until_ts = EXCLUDED.valid_until_ts,
		              fetched_at = now()`

	for keyID, publicKey := range keys {
		if _, err := tx.Exec(ctx, query, serverName, keyID, publicKey, validUntil); err != nil {
			return fmt.Errorf("store: save server key %s/%s: %w", serverName, keyID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: save server keys commit: %w", err)
	}

	return nil
}
