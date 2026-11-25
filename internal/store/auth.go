package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aisa-it/syngo/internal/core"
)

// AuthStore — реализация core.AuthStore поверх PostgreSQL.
type AuthStore struct {
	db *DB
}

// interface guard: AuthStore реализует доменный контракт.
var _ core.AuthStore = (*AuthStore)(nil)

// NewAuthStore создаёт хранилище аутентификации.
func NewAuthStore(db *DB) *AuthStore {
	return &AuthStore{db: db}
}

// CreateUser вставляет пользователя; конфликт localpart отображается в core.ErrUserInUse.
func (s *AuthStore) CreateUser(ctx context.Context, user core.User) (core.User, error) {
	const query = `
		INSERT INTO users (user_id, localpart, password_hash, display_name, avatar_url, is_admin)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING user_id, localpart, password_hash, display_name, coalesce(avatar_url, ''), deactivated, is_admin`

	err := s.db.pool.QueryRow(ctx, query,
		user.UserID, user.Localpart, user.PasswordHash, user.DisplayName, nullable(user.AvatarURL), user.IsAdmin,
	).Scan(&user.UserID, &user.Localpart, &user.PasswordHash, &user.DisplayName, &user.AvatarURL, &user.Deactivated, &user.IsAdmin)
	if err != nil {
		if isUniqueViolation(err) {
			return core.User{}, core.ErrUserInUse
		}

		return core.User{}, fmt.Errorf("store: create user %s: %w", user.UserID, err)
	}

	return user, nil
}

// UserByLocalpart читает пользователя по localpart без учета регистра, как Synapse
// (_find_user_id_and_pwd_hash): одно совпадение - оно, несколько - только точное.
// Раньше искалось только точное совпадение с приведенным к нижнему регистру
// именем, и перенесенные из Synapse учетки со старыми заглавными localpart не
// находились.
func (s *AuthStore) UserByLocalpart(ctx context.Context, localpart string) (core.User, error) {
	const query = `
		SELECT user_id, localpart, password_hash, display_name, coalesce(avatar_url, ''), deactivated, is_admin
		FROM users WHERE lower(localpart) = lower($1)
		ORDER BY (localpart = $1) DESC, localpart
		LIMIT 2`

	rows, err := s.db.pool.Query(ctx, query, localpart)
	if err != nil {
		return core.User{}, fmt.Errorf("store: select user %s: %w", localpart, err)
	}

	defer rows.Close()

	var users []core.User

	for rows.Next() {
		var user core.User
		if err := rows.Scan(&user.UserID, &user.Localpart, &user.PasswordHash, &user.DisplayName,
			&user.AvatarURL, &user.Deactivated, &user.IsAdmin); err != nil {
			return core.User{}, fmt.Errorf("store: scan user %s: %w", localpart, err)
		}

		users = append(users, user)
	}

	if err := rows.Err(); err != nil {
		return core.User{}, fmt.Errorf("store: select user %s: %w", localpart, err)
	}

	switch {
	case len(users) == 1, len(users) > 1 && users[0].Localpart == localpart:
		return users[0], nil
	default:
		return core.User{}, fmt.Errorf("store: user %s: %w", localpart, core.ErrNotFound)
	}
}

// UserByID читает пользователя по полному Matrix user_id.
func (s *AuthStore) UserByID(ctx context.Context, userID string) (core.User, error) {
	return s.user(ctx, "SELECT user_id, localpart, password_hash, display_name, coalesce(avatar_url, ''), deactivated, is_admin "+
		"FROM users WHERE user_id = $1", userID)
}

// user — общий SELECT для одной записи users.
func (s *AuthStore) user(ctx context.Context, query string, arg any) (core.User, error) {
	var user core.User

	err := s.db.pool.QueryRow(ctx, query, arg).
		Scan(&user.UserID, &user.Localpart, &user.PasswordHash, &user.DisplayName, &user.AvatarURL, &user.Deactivated, &user.IsAdmin)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.User{}, fmt.Errorf("store: user %v: %w", arg, core.ErrNotFound)
		}

		return core.User{}, fmt.Errorf("store: select user %v: %w", arg, err)
	}

	return user, nil
}

// UpdatePasswordHash заменяет хеш пароля (миграция bcrypt → argon2id при входе).
func (s *AuthStore) UpdatePasswordHash(ctx context.Context, userID, passwordHash string) error {
	const query = `UPDATE users SET password_hash = $2 WHERE user_id = $1`

	tag, err := s.db.pool.Exec(ctx, query, userID, passwordHash)
	if err != nil {
		return fmt.Errorf("store: update password hash %s: %w", userID, err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}

	return nil
}

// CountPasswordHashes считает пользователей по формату хеша: argon2id (наш) и bcrypt (унаследованный).
func (s *AuthStore) CountPasswordHashes(ctx context.Context) (argon2id, bcrypt int64, err error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE password_hash LIKE '$argon2id$%'),
			count(*) FILTER (WHERE password_hash LIKE '$2%')
		FROM users`

	if err := s.db.pool.QueryRow(ctx, query).Scan(&argon2id, &bcrypt); err != nil {
		return 0, 0, fmt.Errorf("store: count password hashes: %w", err)
	}

	return argon2id, bcrypt, nil
}

// UpsertDevice создаёт устройство или обновляет его отображаемое имя.
func (s *AuthStore) UpsertDevice(ctx context.Context, device core.Device) error {
	const query = `
		INSERT INTO devices (user_id, device_id, display_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, device_id) DO UPDATE
		SET display_name = EXCLUDED.display_name`

	if _, err := s.db.pool.Exec(ctx, query, device.UserID, device.DeviceID, device.DisplayName); err != nil {
		return fmt.Errorf("store: upsert device %s/%s: %w", device.UserID, device.DeviceID, err)
	}

	return nil
}

// CreateToken сохраняет хеши access- и (опционально) refresh-токена.
func (s *AuthStore) CreateToken(ctx context.Context, token core.TokenRecord) error {
	const query = `
		INSERT INTO access_tokens (token_hash, user_id, device_id, refresh_hash, expires_at, scope)
		VALUES ($1, $2, $3, $4, $5, $6)`

	if _, err := s.db.pool.Exec(ctx, query,
		token.TokenHash, token.UserID, token.DeviceID, token.RefreshHash, token.ExpiresAt, token.Scope,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create token: %w", core.ErrInvalidToken)
		}

		return fmt.Errorf("store: create token: %w", err)
	}

	return nil
}

// SessionByTokenHash отдаёт сессию по SHA-256 от access-токена (только активные токены).
func (s *AuthStore) SessionByTokenHash(ctx context.Context, tokenHash []byte) (core.Session, error) {
	const query = `
		SELECT u.user_id, u.localpart, u.display_name, t.device_id, u.deactivated, t.expires_at, t.scope,
		       t.last_validated_at
		FROM access_tokens t
		JOIN users u ON u.user_id = t.user_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())`

	var (
		session       core.Session
		expiresAt     *time.Time
		lastValidated *time.Time
	)

	err := s.db.pool.QueryRow(ctx, query, tokenHash).Scan(
		&session.UserID, &session.Localpart, &session.DisplayName,
		&session.DeviceID, &session.Deactivated, &expiresAt, &session.Scope, &lastValidated,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Session{}, fmt.Errorf("store: session by token: %w", core.ErrNotFound)
		}

		return core.Session{}, fmt.Errorf("store: session by token: %w", err)
	}

	session.ExpiresAt = expiresAt

	s.touchToken(ctx, tokenHash, lastValidated)

	return session, nil
}

// touchToken обновляет access_tokens.last_validated_at не чаще интервала
// прореживания. Метку ничто не читает, кроме этого прореживания (истечение -
// по expires_at, отзыв - по revoked_at), поэтому точности до интервала хватает.
// С включенным накопителем путь запроса не пишет в БД: метка уходит
// пачкой фоновым сбросом. Раньше каждый авторизованный запрос (в том числе
// каждый /sync) делал UPDATE с коммитом и fsync WAL.
func (s *AuthStore) touchToken(ctx context.Context, tokenHash []byte, lastValidated *time.Time) {
	activity := s.db.activity.Load()

	interval := DefaultTokenActivityInterval
	if activity != nil {
		interval = activity.TokenInterval()
	}

	if lastValidated != nil && time.Since(*lastValidated) < interval {
		return
	}

	if activity != nil {
		activity.TouchToken(tokenHash)

		return
	}

	// Без накопителя (утилиты, тесты) - синхронно; ошибка не влияет на авторизацию.
	_, _ = s.db.pool.Exec(ctx,
		"UPDATE access_tokens SET last_validated_at = now() WHERE token_hash = $1", tokenHash)
}

// RevokeToken отзывает один токен. Повторный вызов не считается ошибкой.
// Pushers устройства удаляются вместе с токеном: иначе уведомления
// продолжали уходить на разлогиненное устройство.
func (s *AuthStore) RevokeToken(ctx context.Context, tokenHash []byte) error {
	const query = `UPDATE access_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING user_id, device_id`

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: revoke token begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var userID, deviceID string

	if err := tx.QueryRow(ctx, query, tokenHash).Scan(&userID, &deviceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrInvalidToken
		}

		return fmt.Errorf("store: revoke token: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM pushers WHERE user_id = $1 AND device_id = $2`, userID, deviceID); err != nil {
		return fmt.Errorf("store: revoke token pushers: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: revoke token commit: %w", err)
	}

	return nil
}

// RevokeAllTokens отзывает все активные токены пользователя и удаляет все его
// pushers: logout/all, деактивация, сброс пароля администратором.
func (s *AuthStore) RevokeAllTokens(ctx context.Context, userID string) (int64, error) {
	const query = `UPDATE access_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`

	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: revoke all tokens begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, query, userID)
	if err != nil {
		return 0, fmt.Errorf("store: revoke all tokens %s: %w", userID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM pushers WHERE user_id = $1`, userID); err != nil {
		return 0, fmt.Errorf("store: revoke all tokens pushers %s: %w", userID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: revoke all tokens commit: %w", err)
	}

	return tag.RowsAffected(), nil
}

// nullable превращает пустую строку в NULL.
func nullable(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// isUniqueViolation распознаёт нарушение уникальности PostgreSQL.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation
}

// isForeignKeyViolation распознает нарушение внешнего ключа PostgreSQL.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation
}

// SessionByRefreshHash находит сессию по хешу refresh-токена.
func (s *AuthStore) SessionByRefreshHash(ctx context.Context, refreshHash []byte) (core.Session, error) {
	const query = `
		SELECT u.user_id, u.localpart, u.display_name, t.device_id, u.deactivated, t.created_at
		FROM access_tokens t
		JOIN users u ON u.user_id = t.user_id
		WHERE t.refresh_hash = $1 AND t.revoked_at IS NULL`

	var session core.Session

	err := s.db.pool.QueryRow(ctx, query, refreshHash).Scan(
		&session.UserID, &session.Localpart, &session.DisplayName, &session.DeviceID, &session.Deactivated,
		&session.IssuedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Session{}, fmt.Errorf("store: session by refresh: %w", core.ErrNotFound)
		}

		return core.Session{}, fmt.Errorf("store: session by refresh: %w", err)
	}

	return session, nil
}

// RevokeRefresh отзывает строку access_tokens по хешу refresh-токена.
func (s *AuthStore) RevokeRefresh(ctx context.Context, refreshHash []byte) error {
	const query = `UPDATE access_tokens SET revoked_at = now() WHERE refresh_hash = $1 AND revoked_at IS NULL`

	tag, err := s.db.pool.Exec(ctx, query, refreshHash)
	if err != nil {
		return fmt.Errorf("store: revoke refresh: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrInvalidToken
	}

	return nil
}

// DeactivateUser помечает учётную запись деактивированной.
func (s *AuthStore) DeactivateUser(ctx context.Context, userID string) error {
	const query = `UPDATE users SET deactivated = true WHERE user_id = $1`

	tag, err := s.db.pool.Exec(ctx, query, userID)
	if err != nil {
		return fmt.Errorf("store: deactivate user %s: %w", userID, err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}

	return nil
}

// SearchUsers ищет локальных пользователей по localpart или display_name
// (регистронезависимо). Отключённые аккаунты не попадают в выдачу.
// Используется GET /user_directory/search.
func (s *AuthStore) SearchUsers(ctx context.Context, term string, limit int) ([]core.User, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	const query = `
		SELECT user_id, localpart, password_hash, display_name, coalesce(avatar_url, ''), deactivated, is_admin
		FROM users
		WHERE NOT deactivated
		  AND (localpart ILIKE '%' || $1 || '%' OR display_name ILIKE '%' || $1 || '%')
		ORDER BY localpart
		LIMIT $2`

	rows, err := s.db.pool.Query(ctx, query, term, limit)
	if err != nil {
		return nil, fmt.Errorf("store: search users %q: %w", term, err)
	}

	defer rows.Close()

	var out []core.User

	for rows.Next() {
		var user core.User
		if err := rows.Scan(&user.UserID, &user.Localpart, &user.PasswordHash, &user.DisplayName,
			&user.AvatarURL, &user.Deactivated, &user.IsAdmin); err != nil {
			return nil, fmt.Errorf("store: scan search user: %w", err)
		}

		out = append(out, user)
	}

	return out, rows.Err()
}
