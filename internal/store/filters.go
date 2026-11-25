package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// ErrFilterNotFound — фильтр не найден (404 M_NOT_FOUND).
var ErrFilterNotFound = errors.New("store: filter not found")

var _ core.FilterStore = (*DB)(nil)

// SaveUserFilter сохраняет фильтр пользователя (POST /user/{id}/filter).
func (db *DB) SaveUserFilter(ctx context.Context, userID, filterID string, filter json.RawMessage) error {
	_, err := db.pool.Exec(ctx, `
		INSERT INTO user_filters (user_id, filter_id, content) VALUES ($1,$2,$3)
		ON CONFLICT (user_id, filter_id) DO UPDATE SET content = EXCLUDED.content`,
		userID, filterID, filter)
	if err != nil {
		return fmt.Errorf("store: save filter: %w", err)
	}

	return nil
}

// UserFilter возвращает сохранённый фильтр.
func (db *DB) UserFilter(ctx context.Context, userID, filterID string) (json.RawMessage, error) {
	var raw []byte

	err := db.pool.QueryRow(ctx,
		`SELECT content FROM user_filters WHERE user_id=$1 AND filter_id=$2`, userID, filterID).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFilterNotFound
		}

		return nil, fmt.Errorf("store: get filter: %w", err)
	}

	return raw, nil
}
