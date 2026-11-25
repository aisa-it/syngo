package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/aisa-it/syngo/internal/core"
)

// AdminUsers — страница пользователей для admin API (курсор from — localpart).
func (db *DB) AdminUsers(ctx context.Context, from string, limit int) ([]core.User, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	rows, err := db.pool.Query(ctx, `
		-- password_hash не нужен admin API: не вытаскиваем его из БД вовсе.
		SELECT user_id, localpart, ''::text, display_name, coalesce(avatar_url,''),
		       deactivated, is_admin
		FROM users
		WHERE localpart > $1
		ORDER BY localpart
		LIMIT $2`, from, limit)
	if err != nil {
		return nil, fmt.Errorf("store: admin users: %w", err)
	}

	defer rows.Close()

	var out []core.User

	for rows.Next() {
		var u core.User
		if err := rows.Scan(&u.UserID, &u.Localpart, &u.PasswordHash, &u.DisplayName,
			&u.AvatarURL, &u.Deactivated, &u.IsAdmin); err != nil {
			return nil, fmt.Errorf("store: scan admin user: %w", err)
		}

		out = append(out, u)
	}

	return out, rows.Err()
}

// Результаты admin-действий в аудите. Константы и типы admin-контура
// объявлены в core: store реализует core.AdminStore.
const (
	AuditResultOK       = core.AuditResultOK
	AuditResultDenied   = core.AuditResultDenied
	AuditResultNotFound = core.AuditResultNotFound
	AuditResultError    = core.AuditResultError
)

var _ core.AdminStore = (*DB)(nil)

// WriteAdminAudit пишет запись в admin_audit отдельным запросом - для действий,
// которые не меняют БД (отказ, вход) или идут вне транзакции хранилища. Секреты
// (пароли, токены) в details не передаются: вызывающий кладет только параметры
// действия и результат.
func (db *DB) WriteAdminAudit(ctx context.Context, actor, action, target string, details map[string]any) error {
	raw, err := auditDetails(ctx, details)
	if err != nil {
		return err
	}

	if _, err := db.pool.Exec(ctx, `
		INSERT INTO admin_audit (actor, action, target, details) VALUES ($1,$2,$3,$4)`,
		actor, action, target, raw); err != nil {
		return fmt.Errorf("store: admin audit: %w", err)
	}

	return nil
}

// auditDetails сериализует details; без явного result действие считается успешным.
func auditDetails(ctx context.Context, details map[string]any) ([]byte, error) {
	out := make(map[string]any, len(details)+2)
	for k, v := range details {
		out[k] = v
	}

	// IP клиента (за доверенным прокси - из X-Forwarded-For): по
	// аудиту видно, откуда пришло действие. Схема не меняется - это поле details.
	if ip, ok := core.ClientIPFrom(ctx); ok {
		if _, set := out["ip"]; !set {
			out["ip"] = ip
		}
	}

	if _, ok := out["result"]; !ok {
		out["result"] = AuditResultOK
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("store: admin audit details: %w", err)
	}

	return raw, nil
}

// PurgeAdminAudit удаляет записи аудита старше retention (0 — не чистим).
func (db *DB) PurgeAdminAudit(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}

	if _, err := db.pool.Exec(ctx,
		`DELETE FROM admin_audit WHERE created_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(retention.Seconds()))); err != nil {
		return fmt.Errorf("store: purge admin audit: %w", err)
	}

	return nil
}

// AdminAuditPage возвращает записи аудита после id.
func (db *DB) AdminAuditPage(ctx context.Context, afterID int64, limit int) ([]AdminAuditRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := db.pool.Query(ctx, `
		SELECT id, actor, action, target, details, created_at
		FROM admin_audit WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: admin audit page: %w", err)
	}

	defer rows.Close()

	var out []AdminAuditRow

	for rows.Next() {
		var row AdminAuditRow
		if err := rows.Scan(&row.ID, &row.Actor, &row.Action, &row.Target, &row.Details, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan admin audit: %w", err)
		}

		out = append(out, row)
	}

	return out, rows.Err()
}

// AdminAuditRow — запись admin_audit.
type AdminAuditRow = core.AdminAuditRow

// AdminRooms — страница комнат для admin API (курсор from — room_id).
func (db *DB) AdminRooms(ctx context.Context, from string, limit int) ([]AdminRoomRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	rows, err := db.pool.Query(ctx, `
		SELECT r.room_id, coalesce(r.creator,''), r.room_version, r.is_public,
		       (SELECT count(*) FROM room_memberships m WHERE m.room_id = r.room_id AND m.membership = 'join')
		FROM rooms r
		WHERE r.room_id > $1
		ORDER BY r.room_id
		LIMIT $2`, from, limit)
	if err != nil {
		return nil, fmt.Errorf("store: admin rooms: %w", err)
	}

	defer rows.Close()

	var out []AdminRoomRow

	for rows.Next() {
		var row AdminRoomRow
		if err := rows.Scan(&row.RoomID, &row.Creator, &row.RoomVersion, &row.IsPublic, &row.Joined); err != nil {
			return nil, fmt.Errorf("store: scan admin room: %w", err)
		}

		out = append(out, row)
	}

	return out, rows.Err()
}

// AdminRoomRow — комната в admin API.
type AdminRoomRow = core.AdminRoomRow

// RoomBlock - блокировка комнаты (admin API).
type RoomBlock = core.RoomBlock

// BlockRoom блокирует комнату: join и события пользователей запрещены, история и
// состояние остаются. Повторная блокировка - не ошибка; неизвестную комнату тоже
// можно заблокировать заранее, как в Synapse.
func (db *DB) BlockRoom(ctx context.Context, roomID, actor string) error {
	_, err := setRoomBlock(ctx, db.pool, roomID, actor, true)

	return err
}

// AdminSetRoomBlock ставит или снимает блокировку и пишет аудит в одной транзакции:
// без записи аудита действие не проходит. changed=false - состояние уже было таким.
func (db *DB) AdminSetRoomBlock(ctx context.Context, actor, roomID string, block bool) (bool, error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: room block begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	changed, err := setRoomBlock(ctx, tx, roomID, actor, block)
	if err != nil {
		return false, err
	}

	action := "room_block"
	if !block {
		action = "room_unblock"
	}

	if err := insertAuditTx(ctx, tx, actor, action, roomID, map[string]any{"changed": changed}); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: room block commit: %w", err)
	}

	return changed, nil
}

// execer - общий интерфейс пула и транзакции для мелких запросов.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func setRoomBlock(ctx context.Context, q execer, roomID, actor string, block bool) (bool, error) {
	if !block {
		tag, err := q.Exec(ctx, `DELETE FROM room_blocks WHERE room_id = $1`, roomID)
		if err != nil {
			return false, fmt.Errorf("store: unblock room: %w", err)
		}

		return tag.RowsAffected() > 0, nil
	}

	tag, err := q.Exec(ctx, `
		INSERT INTO room_blocks (room_id, blocked_by) VALUES ($1,$2)
		ON CONFLICT (room_id) DO NOTHING`, roomID, actor)
	if err != nil {
		return false, fmt.Errorf("store: block room: %w", err)
	}

	return tag.RowsAffected() > 0, nil
}

// RoomBlocked сообщает, заблокирована ли комната.
func (db *DB) RoomBlocked(ctx context.Context, roomID string) (bool, error) {
	var exists bool

	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM room_blocks WHERE room_id = $1)`, roomID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: room blocked: %w", err)
	}

	return exists, nil
}

// RoomBlockInfo - кто и когда заблокировал комнату (Blocked=false, если не заблокирована).
func (db *DB) RoomBlockInfo(ctx context.Context, roomID string) (RoomBlock, error) {
	var info RoomBlock

	err := db.pool.QueryRow(ctx,
		`SELECT blocked_by, blocked_at FROM room_blocks WHERE room_id = $1`, roomID).
		Scan(&info.BlockedBy, &info.BlockedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoomBlock{}, nil
	}

	if err != nil {
		return RoomBlock{}, fmt.Errorf("store: room block info: %w", err)
	}

	info.Blocked = true

	return info, nil
}

// AdminDeleteRoom удаляет комнату со всеми связанными данными (без аудита).
func (db *DB) AdminDeleteRoom(ctx context.Context, roomID string) error {
	return db.AdminPurgeRoom(ctx, "", roomID, false, nil)
}

// AdminPurgeRoom удаляет комнату, ее историю и все, что на нее ссылается, и пишет
// аудит в той же транзакции. Каскад от rooms снимает события, состояние,
// членства и алиасы; таблицы без внешнего ключа на комнату чистятся явно: раньше
// оставались event_auth, account_data, destination_rooms, federation_outbox и
// transactions (повтор txn после purge давал 500). Блокировка room_blocks не
// удаляется: заблокированная комната остается заблокированной и после purge,
// block=true дополнительно блокирует ее. actor="" - без записи аудита.
func (db *DB) AdminPurgeRoom(ctx context.Context, actor, roomID string, block bool, details map[string]any) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: purge room begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	// Блокировка строки комнаты: параллельная запись события (FK на rooms) ждет
	// purge и затем получает «нет комнаты», а не вставляет событие-сироту.
	var locked string
	if err := tx.QueryRow(ctx, `SELECT room_id FROM rooms WHERE room_id = $1 FOR UPDATE`, roomID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}

		return fmt.Errorf("store: purge room lock: %w", err)
	}

	// Порядок важен: event_auth находим по событиям комнаты до их удаления.
	steps := []struct{ name, sql string }{
		{"event_auth", `DELETE FROM event_auth WHERE event_id IN (SELECT event_id FROM events WHERE room_id = $1)`},
		{"transactions", `DELETE FROM transactions WHERE room_id = $1`},
		{"account_data", `DELETE FROM account_data WHERE room_id = $1`},
		{"receipts", `DELETE FROM receipts WHERE room_id = $1`},
		{"destination_rooms", `DELETE FROM destination_rooms WHERE room_id = $1`},
		// room_id заполнен у всех строк: у старых готовых PDU его проставила миграция
		// 0041, pdu_json - текст.
		{"federation_outbox", `DELETE FROM federation_outbox WHERE room_id = $1`},
		{"federation_outbox_dead", `DELETE FROM federation_outbox_dead WHERE room_id = $1`},
		{"rooms", `DELETE FROM rooms WHERE room_id = $1`},
	}

	for _, step := range steps {
		if _, err := tx.Exec(ctx, step.sql, roomID); err != nil {
			return fmt.Errorf("store: purge room %s: %w", step.name, err)
		}
	}

	if block {
		if _, err := setRoomBlock(ctx, tx, roomID, actor, true); err != nil {
			return err
		}
	}

	if actor != "" {
		audit := map[string]any{"block": block}
		for k, v := range details {
			audit[k] = v
		}

		if err := insertAuditTx(ctx, tx, actor, "room_purge", roomID, audit); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: purge room commit: %w", err)
	}

	return nil
}

// CountRooms - общее число комнат (поле total в admin-списке комнат).
func (db *DB) CountRooms(ctx context.Context) (int, error) {
	var total int

	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM rooms`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count rooms: %w", err)
	}

	return total, nil
}

// CountAdmins возвращает число пользователей с ролью администратора.
func (db *DB) CountAdmins(ctx context.Context) (int, error) {
	var count int

	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_admin`).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count admins: %w", err)
	}

	return count, nil
}

// ErrLastAdmin - попытка оставить сервер без активного администратора.
// Та же ошибка, что в core: API различает ее одним errors.Is.
var ErrLastAdmin = core.ErrLastAdmin

// adminRolesLockKey - ключ advisory-блокировки изменений ролей и деактивации админов.
const adminRolesLockKey = "syngo:admin-roles"

// lockAdminRoles сериализует операции, которые могут уменьшить число активных
// админов. Раньше guard был подзапросом count(*) в UPDATE: на READ COMMITTED две
// параллельные транзакции снимали роли с двух последних админов, каждая видела
// второго, и оставалось 0 (write skew). Под блокировкой следующий
// запрос транзакции видит уже закоммиченный результат предыдущей.
func lockAdminRoles(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, adminRolesLockKey); err != nil {
		return fmt.Errorf("store: lock admin roles: %w", err)
	}

	return nil
}

// guardLastAdmin возвращает ErrLastAdmin, если userID - активный админ и других
// активных админов нет. Вызывается под lockAdminRoles.
func guardLastAdmin(ctx context.Context, tx pgx.Tx, userID string) error {
	var targetActiveAdmin bool

	err := tx.QueryRow(ctx,
		`SELECT is_admin AND NOT deactivated FROM users WHERE user_id = $1`, userID).Scan(&targetActiveAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("store: admin target: %w", err)
	}

	if !targetActiveAdmin {
		return nil
	}

	var others int

	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM users WHERE is_admin AND NOT deactivated AND user_id <> $1`, userID).
		Scan(&others); err != nil {
		return fmt.Errorf("store: count other admins: %w", err)
	}

	if others == 0 {
		return ErrLastAdmin
	}

	return nil
}

// AdminSetUserDeactivated меняет деактивацию, при деактивации отзывает токены и
// пишет аудит - одной транзакцией. Деактивировать последнего активного админа нельзя:
// это тоже снятие прав.
func (db *DB) AdminSetUserDeactivated(ctx context.Context, actor, userID string, deactivated bool) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: admin deactivate begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAdminRoles(ctx, tx); err != nil {
		return err
	}

	if deactivated {
		if err := guardLastAdmin(ctx, tx, userID); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, `UPDATE users SET deactivated = $2 WHERE user_id = $1`, userID, deactivated)
	if err != nil {
		return fmt.Errorf("store: admin deactivate: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}

	action := "activate"
	details := map[string]any{}

	if deactivated {
		action = "deactivate"

		// Деактивация отзывает сессии в той же транзакции: иначе токен продолжит работать.
		revoked, err := tx.Exec(ctx,
			`UPDATE access_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("store: admin deactivate revoke: %w", err)
		}

		details["revoked_tokens"] = revoked.RowsAffected()

		// Деактивированный не получает push.
		if _, err := tx.Exec(ctx, `DELETE FROM pushers WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("store: admin deactivate pushers: %w", err)
		}
	}

	if err := insertAuditTx(ctx, tx, actor, action, userID, details); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// AdminSetUserAdmin меняет роль с защитой «последнего админа» и аудитом в одной
// транзакции. Снятие роли отзывает admin-токены цели: иначе при повторной выдаче
// роли старые токены снова заработали бы.
func (db *DB) AdminSetUserAdmin(ctx context.Context, actor, userID string, admin bool) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: admin set admin begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAdminRoles(ctx, tx); err != nil {
		return err
	}

	if !admin {
		if err := guardLastAdmin(ctx, tx, userID); err != nil {
			return err
		}
	}

	tag, err := tx.Exec(ctx, `UPDATE users SET is_admin = $2 WHERE user_id = $1`, userID, admin)
	if err != nil {
		return fmt.Errorf("store: admin set admin: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}

	action := "admin"
	details := map[string]any{}

	if !admin {
		action = "unadmin"

		revoked, err := tx.Exec(ctx, `
			UPDATE access_tokens SET revoked_at = now()
			WHERE user_id = $1 AND scope <> '' AND revoked_at IS NULL`, userID)
		if err != nil {
			return fmt.Errorf("store: admin revoke admin tokens: %w", err)
		}

		details["revoked_admin_tokens"] = revoked.RowsAffected()
	}

	if err := insertAuditTx(ctx, tx, actor, action, userID, details); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// AdminSetPasswordHash атомарно меняет hash пароля, отзывает токены и пишет аудит.
// Ни пароль, ни hash в аудит не попадают.
func (db *DB) AdminSetPasswordHash(ctx context.Context, actor, userID, hash string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: admin password begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE user_id = $1`, userID, hash)
	if err != nil {
		return fmt.Errorf("store: admin password: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}

	revoked, err := tx.Exec(ctx,
		`UPDATE access_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return fmt.Errorf("store: admin password revoke: %w", err)
	}

	if err := insertAuditTx(ctx, tx, actor, "password_reset", userID,
		map[string]any{"revoked_tokens": revoked.RowsAffected()}); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// AdminRevokeToken отзывает admin-токен (выход из admin-контура) и пишет аудит;
// служебное устройство admin-токена удаляется, когда на нем не осталось активных
// токенов. Раньше admin-токен жил до истечения TTL.
func (db *DB) AdminRevokeToken(ctx context.Context, actor string, tokenHash []byte, adminDeviceID string) error {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: admin logout begin: %w", err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	var userID, deviceID string

	err = tx.QueryRow(ctx, `
		UPDATE access_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND scope <> '' AND revoked_at IS NULL
		RETURNING user_id, device_id`, tokenHash).Scan(&userID, &deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrInvalidToken
	}

	if err != nil {
		return fmt.Errorf("store: admin logout: %w", err)
	}

	if deviceID == adminDeviceID {
		if _, err := tx.Exec(ctx, `
			DELETE FROM devices d
			WHERE d.user_id = $1 AND d.device_id = $2
			  AND NOT EXISTS (SELECT 1 FROM access_tokens t
			                  WHERE t.user_id = d.user_id AND t.device_id = d.device_id
			                    AND t.revoked_at IS NULL)`, userID, deviceID); err != nil {
			return fmt.Errorf("store: admin logout device: %w", err)
		}
	}

	if err := insertAuditTx(ctx, tx, actor, "logout", userID, nil); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// insertAuditTx пишет запись аудита в переданной транзакции.
func insertAuditTx(ctx context.Context, tx pgx.Tx, actor, action, target string, details map[string]any) error {
	raw, err := auditDetails(ctx, details)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO admin_audit (actor, action, target, details) VALUES ($1,$2,$3,$4)`,
		actor, action, target, raw); err != nil {
		return fmt.Errorf("store: admin audit: %w", err)
	}

	return nil
}

// CountUsers — общее число пользователей (для поля total в admin-списках).
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var total int

	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}

	return total, nil
}

// CountAdminAudit — общее число записей аудита (для поля total).
func (db *DB) CountAdminAudit(ctx context.Context) (int, error) {
	var total int

	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count admin audit: %w", err)
	}

	return total, nil
}
