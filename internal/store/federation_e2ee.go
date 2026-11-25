package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/aisa-it/syngo/internal/core"
)

// Кэш устройств и ключей пользователей других серверов, поток своих device
// lists и дедуп входящих to-device (миграция 0044).

var _ core.RemoteKeysStore = (*DB)(nil)

// RemoteDeviceListStates - отслеживаемые пользователи из userIDs.
func (db *DB) RemoteDeviceListStates(ctx context.Context, userIDs []string) (map[string]core.RemoteListState, error) {
	out := make(map[string]core.RemoteListState, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}

	rows, err := db.pool.Query(ctx,
		`SELECT user_id, stream_id, stale FROM remote_device_lists WHERE user_id = ANY($1)`, userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: remote device lists: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			userID string
			state  core.RemoteListState
		)

		if err := rows.Scan(&userID, &state.StreamID, &state.Stale); err != nil {
			return nil, fmt.Errorf("store: scan remote device list: %w", err)
		}

		out[userID] = state
	}

	return out, rows.Err()
}

// recordDeviceListChangeTx - строка device_lists_changes: /sync и /keys/changes
// наших пользователей увидят изменение.
func recordDeviceListChangeTx(ctx context.Context, tx pgx.Tx, userID, deviceID string) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO device_lists_changes (user_id, device_id) VALUES ($1, $2)`, userID, deviceID); err != nil {
		return fmt.Errorf("store: record remote device change: %w", err)
	}

	return nil
}

// upsertRemoteListTx - строка отслеживания пользователя.
func upsertRemoteListTx(ctx context.Context, tx pgx.Tx, userID string, streamID int64) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO remote_device_lists (user_id, stream_id, stale) VALUES ($1, $2, false)
		ON CONFLICT (user_id) DO UPDATE SET stream_id = EXCLUDED.stream_id, stale = false, updated_at = now()`,
		userID, streamID); err != nil {
		return fmt.Errorf("store: remote device list %s: %w", userID, err)
	}

	return nil
}

// saveRemoteSigningKeysTx заменяет ключи кросс-подписи пользователя (nil - ключа нет).
func saveRemoteSigningKeysTx(ctx context.Context, tx pgx.Tx, userID string, master, selfSigning json.RawMessage) error {
	for _, key := range []struct {
		keyType string
		raw     json.RawMessage
	}{{"master", master}, {"self_signing", selfSigning}} {
		if len(key.raw) == 0 || string(key.raw) == "null" {
			continue
		}

		keyID := core.CrossSigningKeyID(key.raw)
		if keyID == "" {
			continue
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO remote_cross_signing_keys (user_id, key_type, key_id, key_data) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, key_type) DO UPDATE
			SET key_id = EXCLUDED.key_id, key_data = EXCLUDED.key_data, updated_at = now()`,
			userID, key.keyType, keyID, string(key.raw)); err != nil {
			return fmt.Errorf("store: remote %s key %s: %w", key.keyType, userID, err)
		}
	}

	return nil
}

// ReplaceRemoteDeviceList заменяет кэш пользователя ответом /user/devices.
func (db *DB) ReplaceRemoteDeviceList(ctx context.Context, list core.RemoteDeviceList) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: replace remote devices begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertRemoteListTx(ctx, tx, list.UserID, list.StreamID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM remote_devices WHERE user_id = $1`, list.UserID); err != nil {
		return fmt.Errorf("store: clear remote devices %s: %w", list.UserID, err)
	}

	for _, device := range list.Devices {
		if err := upsertRemoteDeviceTx(ctx, tx, list.UserID, device.DeviceID, device.DisplayName, device.Keys); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM remote_cross_signing_keys WHERE user_id = $1`, list.UserID); err != nil {
		return fmt.Errorf("store: clear remote signing keys %s: %w", list.UserID, err)
	}

	if err := saveRemoteSigningKeysTx(ctx, tx, list.UserID, list.Master, list.SelfSigning); err != nil {
		return err
	}

	if err := recordDeviceListChangeTx(ctx, tx, list.UserID, ""); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: replace remote devices commit: %w", err)
	}

	return nil
}

// upsertRemoteDeviceTx пишет устройство кэша (keys nil - устройство без ключей).
func upsertRemoteDeviceTx(ctx context.Context, tx pgx.Tx, userID, deviceID, displayName string, keys json.RawMessage) error {
	var keyData any
	if len(keys) > 0 && string(keys) != "null" {
		keyData = string(keys)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO remote_devices (user_id, device_id, display_name, key_data) VALUES ($1, $2, nullif($3, ''), $4::jsonb)
		ON CONFLICT (user_id, device_id) DO UPDATE
		SET display_name = EXCLUDED.display_name, key_data = EXCLUDED.key_data, updated_at = now()`,
		userID, deviceID, displayName, keyData); err != nil {
		return fmt.Errorf("store: remote device %s/%s: %w", userID, deviceID, err)
	}

	return nil
}

// ApplyRemoteDeviceUpdate применяет одно изменение устройства из m.device_list_update.
func (db *DB) ApplyRemoteDeviceUpdate(ctx context.Context, update core.RemoteDeviceUpdate) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: remote device update begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertRemoteListTx(ctx, tx, update.UserID, update.StreamID); err != nil {
		return err
	}

	if update.Deleted {
		if _, err := tx.Exec(ctx, `DELETE FROM remote_devices WHERE user_id = $1 AND device_id = $2`,
			update.UserID, update.DeviceID); err != nil {
			return fmt.Errorf("store: delete remote device: %w", err)
		}
	} else if err := upsertRemoteDeviceTx(ctx, tx, update.UserID, update.DeviceID, update.DisplayName, update.Keys); err != nil {
		return err
	}

	if err := recordDeviceListChangeTx(ctx, tx, update.UserID, update.DeviceID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: remote device update commit: %w", err)
	}

	return nil
}

// MarkRemoteDeviceListStale помечает кэш для ресинка (строка создается, если ее нет).
func (db *DB) MarkRemoteDeviceListStale(ctx context.Context, userID string) error {
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO remote_device_lists (user_id, stale) VALUES ($1, true)
		ON CONFLICT (user_id) DO UPDATE SET stale = true, updated_at = now()`, userID); err != nil {
		return fmt.Errorf("store: mark remote device list stale: %w", err)
	}

	return nil
}

// ApplyRemoteSigningKeys обновляет ключи кросс-подписи отслеживаемого пользователя.
func (db *DB) ApplyRemoteSigningKeys(ctx context.Context, userID string, master, selfSigning json.RawMessage) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: remote signing keys begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := saveRemoteSigningKeysTx(ctx, tx, userID, master, selfSigning); err != nil {
		return err
	}

	if err := recordDeviceListChangeTx(ctx, tx, userID, ""); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: remote signing keys commit: %w", err)
	}

	return nil
}

// RemoteDeviceKeys - ключи устройств из кэша; имя устройства уходит в
// unsigned.device_display_name, как у Synapse.
func (db *DB) RemoteDeviceKeys(ctx context.Context, requests map[string][]string) (map[string]map[string]json.RawMessage, error) {
	out := make(map[string]map[string]json.RawMessage, len(requests))
	if len(requests) == 0 {
		return out, nil
	}

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

	rows, err := db.pool.Query(ctx, `
		SELECT DISTINCT d.user_id, d.device_id, coalesce(d.display_name, ''), d.key_data
		FROM remote_devices d
		JOIN unnest($1::text[], $2::text[]) AS q (user_id, device_id)
		  ON d.user_id = q.user_id AND (q.device_id = '' OR d.device_id = q.device_id)
		WHERE d.key_data IS NOT NULL`, users, devices)
	if err != nil {
		return nil, fmt.Errorf("store: remote device keys: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			userID, deviceID, name string
			keyData                []byte
		)

		if err := rows.Scan(&userID, &deviceID, &name, &keyData); err != nil {
			return nil, fmt.Errorf("store: scan remote device keys: %w", err)
		}

		if out[userID] == nil {
			out[userID] = map[string]json.RawMessage{}
		}

		out[userID][deviceID] = withDisplayName(keyData, name)
	}

	return out, rows.Err()
}

// withDisplayName добавляет unsigned.device_display_name в объект ключей.
func withDisplayName(keyData []byte, name string) json.RawMessage {
	if name == "" {
		return keyData
	}

	var object map[string]any
	if json.Unmarshal(keyData, &object) != nil {
		return keyData
	}

	unsigned, _ := object["unsigned"].(map[string]any)
	if unsigned == nil {
		unsigned = map[string]any{}
	}

	unsigned["device_display_name"] = name
	object["unsigned"] = unsigned

	raw, err := json.Marshal(object)
	if err != nil {
		return keyData
	}

	return raw
}

// RemoteUsersSharingRoom - кто из userIDs (joined по room_state) делит комнату с
// joined-пользователем ownServer. Членство удаленных из /send есть только в
// room_state, свое - в room_memberships.
func (db *DB) RemoteUsersSharingRoom(ctx context.Context, userIDs []string, ownServer string) ([]string, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}

	rows, err := db.pool.Query(ctx, `
		SELECT DISTINCT rs.state_key
		FROM room_state rs
		JOIN events e ON e.event_id = rs.event_id
		WHERE rs.type = 'm.room.member' AND rs.state_key = ANY($1)
		  AND e.content->>'membership' = 'join'
		  AND EXISTS (
		      SELECT 1 FROM room_memberships m
		      WHERE m.room_id = rs.room_id AND m.membership = 'join'
		        AND m.user_id LIKE '%:' || $2)
		ORDER BY 1`, userIDs, ownServer)
	if err != nil {
		return nil, fmt.Errorf("store: remote users sharing room: %w", err)
	}

	users, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: remote users sharing room: %w", err)
	}

	return users, nil
}

// UserJoined - пользователь joined в комнате по текущему состоянию (room_state).
func (db *DB) UserJoined(ctx context.Context, roomID, userID string) (bool, error) {
	var joined bool

	if err := db.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM room_state rs JOIN events e ON e.event_id = rs.event_id
			WHERE rs.room_id = $1 AND rs.type = 'm.room.member' AND rs.state_key = $2
			  AND e.content->>'membership' = 'join')`, roomID, userID).Scan(&joined); err != nil {
		return false, fmt.Errorf("store: user joined: %w", err)
	}

	return joined, nil
}

// HostsSharingRoomWith - серверы (кроме ownServer) с joined-участниками в комнатах,
// где joined userID: получатели presence и изменений device list пользователя.
func (db *DB) HostsSharingRoomWith(ctx context.Context, userID, ownServer string) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT DISTINCT substr(rs.state_key, strpos(rs.state_key, ':') + 1)
		FROM room_state mine
		JOIN events me ON me.event_id = mine.event_id
		JOIN room_state rs ON rs.room_id = mine.room_id AND rs.type = 'm.room.member'
		JOIN events e ON e.event_id = rs.event_id
		WHERE mine.type = 'm.room.member' AND mine.state_key = $1
		  AND me.content->>'membership' = 'join'
		  AND strpos(rs.state_key, ':') > 0
		  AND e.content->>'membership' = 'join'`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: hosts sharing room: %w", err)
	}

	hosts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: hosts sharing room: %w", err)
	}

	out := hosts[:0]

	for _, host := range hosts {
		if host != ownServer {
			out = append(out, host)
		}
	}

	slices.Sort(out)

	return out, nil
}

// NextDeviceListStream выдает следующий stream_id device list пользователя и
// предыдущий (0 - изменений еще не было): stream_id и prev_id m.device_list_update.
func (db *DB) NextDeviceListStream(ctx context.Context, userID string) (stream, prev int64, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: device list stream begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `SELECT stream_id FROM device_lists_outbound WHERE user_id = $1 FOR UPDATE`, userID).Scan(&prev)
	if err != nil && !isNoRows(err) {
		return 0, 0, fmt.Errorf("store: device list stream: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO device_lists_outbound (user_id, stream_id) VALUES ($1, nextval('events_stream_ordering_seq'))
		ON CONFLICT (user_id) DO UPDATE SET stream_id = EXCLUDED.stream_id
		RETURNING stream_id`, userID).Scan(&stream); err != nil {
		return 0, 0, fmt.Errorf("store: device list stream next: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: device list stream commit: %w", err)
	}

	return stream, prev, nil
}

// DeviceListStream - текущий stream_id device list пользователя (0 - не менялся).
func (db *DB) DeviceListStream(ctx context.Context, userID string) (int64, error) {
	var stream int64

	err := db.pool.QueryRow(ctx, `SELECT stream_id FROM device_lists_outbound WHERE user_id = $1`, userID).Scan(&stream)
	if err != nil && !isNoRows(err) {
		return 0, fmt.Errorf("store: device list stream: %w", err)
	}

	return stream, nil
}

// LocalDevices - устройства пользователя и их имена (для /user/devices).
func (db *DB) LocalDevices(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT device_id, coalesce(display_name, '') FROM devices WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: local devices: %w", err)
	}

	defer rows.Close()

	out := map[string]string{}

	for rows.Next() {
		var deviceID, name string
		if err := rows.Scan(&deviceID, &name); err != nil {
			return nil, fmt.Errorf("store: scan local device: %w", err)
		}

		out[deviceID] = name
	}

	return out, rows.Err()
}

// DeliverFederatedToDevice кладет to-device сообщения от удаленного sender в
// очереди наших устройств ("*" - все устройства). Повтор (origin, message_id) -
// duplicate=true без записи.
func (db *DB) DeliverFederatedToDevice(
	ctx context.Context, origin, messageID, sender, eventType string, messages map[DeviceTarget]json.RawMessage,
) (duplicate bool, err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: federated to-device begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if messageID != "" {
		tag, err := tx.Exec(ctx, `
			INSERT INTO device_federation_inbox (origin, message_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, origin, messageID)
		if err != nil {
			return false, fmt.Errorf("store: federated to-device dedup: %w", err)
		}

		if tag.RowsAffected() == 0 {
			return true, nil
		}
	}

	const insert = `
		INSERT INTO device_inbox (user_id, device_id, event_type, sender, content)
		VALUES ($1, $2, $3, $4, $5)`

	for target, content := range messages {
		devices := []string{target.DeviceID}

		if target.DeviceID == "*" {
			devices, err = userDevices(ctx, tx, target.UserID)
			if err != nil {
				return false, err
			}
		}

		for _, deviceID := range devices {
			if _, err := tx.Exec(ctx, insert, target.UserID, deviceID, eventType, sender, []byte(content)); err != nil {
				return false, fmt.Errorf("store: federated to-device %s/%s: %w", target.UserID, deviceID, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: federated to-device commit: %w", err)
	}

	return false, nil
}
