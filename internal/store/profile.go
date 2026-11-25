package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// ProfileStore — реализация core.ProfileStore поверх PostgreSQL.
type ProfileStore struct {
	db *DB
}

var _ core.ProfileStore = (*ProfileStore)(nil)

// NewProfileStore создаёт хранилище профилей и устройств.
func NewProfileStore(db *DB) *ProfileStore {
	return &ProfileStore{db: db}
}

// Profile читает профиль пользователя.
func (s *ProfileStore) Profile(ctx context.Context, userID string) (core.Profile, error) {
	const query = `SELECT user_id, display_name, coalesce(avatar_url, '') FROM users WHERE user_id = $1`

	var profile core.Profile

	err := s.db.pool.QueryRow(ctx, query, userID).
		Scan(&profile.UserID, &profile.DisplayName, &profile.AvatarURL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.Profile{}, fmt.Errorf("store: profile %s: %w", userID, core.ErrNotFound)
		}

		return core.Profile{}, fmt.Errorf("store: profile %s: %w", userID, err)
	}

	return profile, nil
}

// UpdateDisplayName меняет displayname пользователя (ErrNotFound, если пользователя нет).
func (s *ProfileStore) UpdateDisplayName(ctx context.Context, userID, displayName string) error {
	return s.updateUserField(ctx, "UPDATE users SET display_name = $2 WHERE user_id = $1", userID, displayName)
}

// UpdateAvatarURL меняет avatar_url пользователя.
func (s *ProfileStore) UpdateAvatarURL(ctx context.Context, userID, avatarURL string) error {
	// Колонки users.updated_at в схеме нет (ни одной миграцией) — иначе PUT
	// avatar_url всегда 500.
	return s.updateUserField(ctx, "UPDATE users SET avatar_url = $2 WHERE user_id = $1", userID, avatarURL)
}

// updateUserField — общий UPDATE одного поля профиля.
func (s *ProfileStore) updateUserField(ctx context.Context, query, userID, value string) error {
	tag, err := s.db.pool.Exec(ctx, query, userID, value)
	if err != nil {
		return fmt.Errorf("store: update profile %s: %w", userID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: profile %s: %w", userID, core.ErrNotFound)
	}

	return nil
}

// visibleDevice - условие видимости устройства в CS-API ($2 - AdminDeviceID):
// служебное устройство admin-токена скрыто, пока на нем нет обычных (пустой scope)
// активных токенов. Раньше устройство ADMIN появлялось в списке устройств
// пользователя после входа в admin-контур.
const visibleDevice = `NOT (d.device_id = $2 AND NOT EXISTS (
		SELECT 1 FROM access_tokens t
		WHERE t.user_id = d.user_id AND t.device_id = d.device_id
		  AND t.scope = '' AND t.revoked_at IS NULL))`

// Devices возвращает устройства пользователя.
func (s *ProfileStore) Devices(ctx context.Context, userID string) ([]core.DeviceInfo, error) {
	const query = `
		SELECT device_id, coalesce(display_name, ''), coalesce(last_seen_ip, ''), last_seen_at
		FROM devices d WHERE user_id = $1 AND ` + visibleDevice + `
		ORDER BY device_id`

	rows, err := s.db.pool.Query(ctx, query, userID, core.AdminDeviceID)
	if err != nil {
		return nil, fmt.Errorf("store: devices %s: %w", userID, err)
	}

	defer rows.Close()

	var devices []core.DeviceInfo

	for rows.Next() {
		device, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}

		devices = append(devices, device)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate devices: %w", err)
	}

	return devices, nil
}

// Device возвращает одно устройство.
func (s *ProfileStore) Device(ctx context.Context, userID, deviceID string) (core.DeviceInfo, error) {
	const query = `
		SELECT device_id, coalesce(display_name, ''), coalesce(last_seen_ip, ''), last_seen_at
		FROM devices d WHERE user_id = $1 AND device_id = $3 AND ` + visibleDevice

	rows, err := s.db.pool.Query(ctx, query, userID, core.AdminDeviceID, deviceID)
	if err != nil {
		return core.DeviceInfo{}, fmt.Errorf("store: device %s/%s: %w", userID, deviceID, err)
	}

	defer rows.Close()

	if !rows.Next() {
		return core.DeviceInfo{}, fmt.Errorf("store: device %s/%s: %w", userID, deviceID, core.ErrNotFound)
	}

	return scanDevice(rows)
}

// scanDevice читает устройство из строки (last_seen_at хранится как timestamptz,
// наружу отдаём миллисекунды, как в CS-API).
func scanDevice(rows pgx.Rows) (core.DeviceInfo, error) {
	var (
		device   core.DeviceInfo
		lastSeen *time.Time
	)

	if err := rows.Scan(&device.DeviceID, &device.DisplayName, &device.LastSeenIP, &lastSeen); err != nil {
		return core.DeviceInfo{}, fmt.Errorf("store: scan device: %w", err)
	}

	if lastSeen != nil {
		ms := lastSeen.UnixMilli()
		device.LastSeenTS = &ms
	}

	return device, nil
}

// UpdateDeviceDisplayName переименовывает устройство.
func (s *ProfileStore) UpdateDeviceDisplayName(ctx context.Context, userID, deviceID, displayName string) error {
	const query = `UPDATE devices SET display_name = $3 WHERE user_id = $1 AND device_id = $2`

	tag, err := s.db.pool.Exec(ctx, query, userID, deviceID, displayName)
	if err != nil {
		return fmt.Errorf("store: rename device %s/%s: %w", userID, deviceID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: device %s/%s: %w", userID, deviceID, core.ErrNotFound)
	}

	return nil
}

// DeleteDevice удаляет устройство, его токены и ключи (E2EE) одной транзакцией.
func (s *ProfileStore) DeleteDevice(ctx context.Context, userID, deviceID string) error {
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin delete device: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Порядок: ключи, токены, почта устройства, устройство (иначе FK не даст удалить).
	// device_inbox чистим обязательно: иначе новое устройство с тем же id получит
	// сообщения, адресованные удалённому.
	for _, query := range []string{
		`DELETE FROM one_time_keys WHERE user_id = $1 AND device_id = $2`,
		`DELETE FROM e2e_fallback_keys WHERE user_id = $1 AND device_id = $2`,
		`DELETE FROM device_keys WHERE user_id = $1 AND device_id = $2`,
		`DELETE FROM access_tokens WHERE user_id = $1 AND device_id = $2`,
		`DELETE FROM device_inbox WHERE user_id = $1 AND device_id = $2`,
		// Pushers устройства: удаленное устройство не получает push.
		`DELETE FROM pushers WHERE user_id = $1 AND device_id = $2`,
	} {
		if _, err := tx.Exec(ctx, query, userID, deviceID); err != nil {
			return fmt.Errorf("store: delete device data %s/%s: %w", userID, deviceID, err)
		}
	}

	tag, err := tx.Exec(ctx, `DELETE FROM devices WHERE user_id = $1 AND device_id = $2`, userID, deviceID)
	if err != nil {
		return fmt.Errorf("store: delete device %s/%s: %w", userID, deviceID, err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: device %s/%s: %w", userID, deviceID, core.ErrNotFound)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit delete device: %w", err)
	}

	return nil
}

// SharesRoom сообщает, есть ли у пользователей общая комната с join.
func (s *ProfileStore) SharesRoom(ctx context.Context, first, second string) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			FROM room_memberships a
			JOIN room_memberships b ON b.room_id = a.room_id
			WHERE a.user_id = $1 AND a.membership = 'join'
			  AND b.user_id = $2 AND b.membership = 'join'
		)`

	var shares bool
	if err := s.db.pool.QueryRow(ctx, query, first, second).Scan(&shares); err != nil {
		return false, fmt.Errorf("store: shares room %s/%s: %w", first, second, err)
	}

	return shares, nil
}

// UsersSharingRoom - те из candidates, у кого с userID есть общая комната с join:
// аудитория presence в /sync. Один запрос на любое число кандидатов.
func (s *ProfileStore) UsersSharingRoom(ctx context.Context, userID string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	const query = `
		SELECT DISTINCT b.user_id
		FROM room_memberships a
		JOIN room_memberships b ON b.room_id = a.room_id
		WHERE a.user_id = $1 AND a.membership = 'join'
		  AND b.membership = 'join' AND b.user_id = ANY($2::text[])`

	rows, err := s.db.pool.Query(ctx, query, userID, candidates)
	if err != nil {
		return nil, fmt.Errorf("store: users sharing room with %s: %w", userID, err)
	}

	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: users sharing room with %s: %w", userID, err)
	}

	return users, nil
}

// RoomMates - все, с кем у userID есть общая комната с join (кроме него самого):
// кого будить при смене presence.
func (s *ProfileStore) RoomMates(ctx context.Context, userID string) ([]string, error) {
	const query = `
		SELECT DISTINCT b.user_id
		FROM room_memberships a
		JOIN room_memberships b ON b.room_id = a.room_id
		WHERE a.user_id = $1 AND a.membership = 'join'
		  AND b.membership = 'join' AND b.user_id <> $1`

	rows, err := s.db.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("store: room mates of %s: %w", userID, err)
	}

	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: room mates of %s: %w", userID, err)
	}

	return users, nil
}
