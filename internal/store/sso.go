package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// SSOStore - привязки внешних учеток и loginToken поверх PostgreSQL
// (миграция 0037).
type SSOStore struct {
	db *DB
}

// interface guard: SSOStore реализует доменный контракт.
var _ core.SSOStore = (*SSOStore)(nil)

// NewSSOStore создает хранилище SSO.
func NewSSOStore(db *DB) *SSOStore {
	return &SSOStore{db: db}
}

// UserIDByExternalID - учетная запись по (auth_provider, external_id).
func (s *SSOStore) UserIDByExternalID(ctx context.Context, provider, externalID string) (string, error) {
	var userID string

	err := s.db.pool.QueryRow(ctx,
		`SELECT user_id FROM user_external_ids WHERE auth_provider = $1 AND external_id = $2`,
		provider, externalID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", core.ErrNotFound
	}

	if err != nil {
		return "", fmt.Errorf("store: external id %s: %w", provider, err)
	}

	return userID, nil
}

// AddExternalID привязывает внешний идентификатор; повтор пары - ErrExternalIDInUse.
func (s *SSOStore) AddExternalID(ctx context.Context, provider, externalID, userID string) error {
	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO user_external_ids (auth_provider, external_id, user_id) VALUES ($1, $2, $3)`,
		provider, externalID, userID)
	if err != nil {
		if isUniqueViolation(err) {
			return core.ErrExternalIDInUse
		}

		if isForeignKeyViolation(err) {
			return core.ErrNotFound
		}

		return fmt.Errorf("store: add external id %s for %s: %w", provider, userID, err)
	}

	return nil
}

// ExternalProviders - провайдеры, к которым привязана учетная запись.
func (s *SSOStore) ExternalProviders(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.pool.Query(ctx,
		`SELECT DISTINCT auth_provider FROM user_external_ids WHERE user_id = $1 ORDER BY auth_provider`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: external providers %s: %w", userID, err)
	}

	providers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: external providers %s: %w", userID, err)
	}

	return providers, nil
}

// CreateLoginToken сохраняет loginToken и попутно чистит давно истекшие.
func (s *SSOStore) CreateLoginToken(ctx context.Context, token core.LoginToken) error {
	if _, err := s.db.pool.Exec(ctx,
		`DELETE FROM login_tokens WHERE expires_at < now() - interval '1 hour'`); err != nil {
		return fmt.Errorf("store: purge login tokens: %w", err)
	}

	_, err := s.db.pool.Exec(ctx,
		`INSERT INTO login_tokens (token_hash, user_id, auth_provider, expires_at) VALUES ($1, $2, $3, $4)`,
		token.TokenHash, token.UserID, token.AuthProvider, token.ExpiresAt)
	if err != nil {
		return fmt.Errorf("store: create login token for %s: %w", token.UserID, err)
	}

	return nil
}

// ConsumeLoginToken гасит токен одним UPDATE: два параллельных обмена одного
// токена не получат две сессии.
func (s *SSOStore) ConsumeLoginToken(ctx context.Context, tokenHash []byte, now time.Time) (core.LoginToken, error) {
	token := core.LoginToken{TokenHash: tokenHash}

	err := s.db.pool.QueryRow(ctx, `
		UPDATE login_tokens SET used_at = $2
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		RETURNING user_id, auth_provider, expires_at`,
		tokenHash, now).Scan(&token.UserID, &token.AuthProvider, &token.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.LoginToken{}, core.ErrNotFound
	}

	if err != nil {
		return core.LoginToken{}, fmt.Errorf("store: consume login token: %w", err)
	}

	return token, nil
}
