package main

// Перенос данных пользователей Synapse: устройства, access и refresh токены,
// E2EE (ключи устройств, one-time и fallback keys, кросс-подписи и подписи), бэкап
// ключей, account data и теги, push rules, pushers, фильтры, недоставленные to-device,
// отметки прочтения.
// Без этого после переключения все клиенты разлогинивались и стирали крипто-хранилище,
// а зашифрованная история становилась нечитаемой.
//
// Повторный запуск идемпотентен: строки с тем же ключом обновляются из источника
// (Synapse - источник истины до переключения), неизменяемые (токены, one-time keys,
// to-device) не дублируются.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aisa-it/syngo/internal/core"
	"github.com/aisa-it/syngo/internal/pushrules"
)

// importUserData импортирует запись данных пользователя; handled=false - kind не наш.
func (im *importer) importUserData(ctx context.Context, record *dumpRecord) (handled bool, err error) {
	counter := im.counts.userDataCounter(record.Kind)
	if counter == nil {
		return false, nil
	}

	switch record.Kind {
	case "device":
		err = im.importDevice(ctx, record)
	case "access_token":
		err = im.importAccessToken(ctx, record)
	case "device_keys":
		err = im.exec(ctx, `
			INSERT INTO device_keys (user_id, device_id, key_data) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, device_id) DO UPDATE SET key_data = EXCLUDED.key_data, updated_at = now()`,
			record.UserID, record.DeviceID, rawJSON(record.KeyJSON))
	case "one_time_key":
		// key_id SYNGO - "<алгоритм>:<id>", в Synapse это два столбца.
		err = im.exec(ctx, `
			INSERT INTO one_time_keys (user_id, device_id, key_id, algorithm, key_data) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, device_id, key_id) DO NOTHING`,
			record.UserID, record.DeviceID, record.Algorithm+":"+record.KeyID, record.Algorithm, rawJSON(record.KeyJSON))
	case "fallback_key":
		err = im.exec(ctx, `
			INSERT INTO e2e_fallback_keys (user_id, device_id, algorithm, key_id, key_data, used)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, device_id, algorithm) DO UPDATE
			SET key_id = EXCLUDED.key_id, key_data = EXCLUDED.key_data, used = EXCLUDED.used, updated_at = now()`,
			record.UserID, record.DeviceID, record.Algorithm, record.Algorithm+":"+record.KeyID,
			rawJSON(record.KeyJSON), record.Used)
	case "cross_signing_key":
		err = im.importCrossSigningKey(ctx, record)
	case "cross_signing_signature":
		// В Synapse user_id/key_id - подписант, target_device_id - устройство или кросс-ключ.
		err = im.exec(ctx, `
			INSERT INTO e2e_signatures (target_user_id, target_key_id, signer_user_id, signer_key_id, signature)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (target_user_id, target_key_id, signer_user_id, signer_key_id)
			DO UPDATE SET signature = EXCLUDED.signature, updated_at = now()`,
			record.TargetUserID, record.TargetDeviceID, record.UserID, record.KeyID, record.Signature)
	case "room_keys_version":
		err = im.exec(ctx, `
			INSERT INTO room_keys_versions (user_id, version, algorithm, auth_data, etag, deleted)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id, version) DO UPDATE
			SET algorithm = EXCLUDED.algorithm, auth_data = EXCLUDED.auth_data,
			    etag = EXCLUDED.etag, deleted = EXCLUDED.deleted`,
			record.UserID, record.Version, record.Algorithm, rawJSON(record.AuthData), record.Etag, record.Deleted)
	case "room_key":
		err = im.importRoomKey(ctx, record)
	case "account_data":
		err = im.exec(ctx, `
			INSERT INTO account_data (user_id, room_id, type, content) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, room_id, type) DO UPDATE
			SET content = EXCLUDED.content, updated_at = now(),
			    stream_ordering = nextval('events_stream_ordering_seq')
			WHERE account_data.content IS DISTINCT FROM EXCLUDED.content`,
			record.UserID, record.RoomID, record.Type, rawJSON(record.Content))
	case "push_rule":
		err = im.importPushRule(ctx, record)
	case "pusher":
		err = im.exec(ctx, `
			INSERT INTO pushers (user_id, app_id, pushkey, kind, app_display_name, device_display_name,
			                     profile_tag, lang, data, device_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (user_id, app_id, pushkey) DO UPDATE
			SET kind = EXCLUDED.kind, app_display_name = EXCLUDED.app_display_name,
			    device_display_name = EXCLUDED.device_display_name, profile_tag = EXCLUDED.profile_tag,
			    lang = EXCLUDED.lang, data = EXCLUDED.data, device_id = EXCLUDED.device_id, updated_at = now()`,
			record.UserID, record.AppID, record.Pushkey, record.PusherKind, record.AppDisplayName,
			record.DeviceDisplay, record.ProfileTag, record.Lang, rawJSON(record.Data), nullable(record.DeviceID))
	case "filter":
		err = im.exec(ctx, `
			INSERT INTO user_filters (user_id, filter_id, content) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, filter_id) DO UPDATE SET content = EXCLUDED.content`,
			record.UserID, record.FilterID, rawJSON(record.Filter))
	case "to_device":
		err = im.importToDevice(ctx, record)
	case "user_external_id":
		err = im.importExternalID(ctx, record)
	case "receipt":
		err = im.importReceipt(ctx, record)
	}

	if err == nil {
		*counter++
	}

	return true, err
}

// userDataCounter - счетчик вида данных пользователя; nil - вид не из userdata.
func (c *counters) userDataCounter(kind string) *int {
	return map[string]*int{
		"device": &c.Devices, "access_token": &c.AccessTokens, "device_keys": &c.DeviceKeys,
		"one_time_key": &c.OneTimeKeys, "fallback_key": &c.FallbackKeys,
		"cross_signing_key": &c.CrossSigning, "cross_signing_signature": &c.Signatures,
		"room_keys_version": &c.RoomKeyVersion, "room_key": &c.RoomKeys, "account_data": &c.AccountData,
		"push_rule": &c.PushRules, "pusher": &c.Pushers, "filter": &c.Filters, "to_device": &c.ToDevice,
		"user_external_id": &c.ExternalIDs, "receipt": &c.Receipts,
	}[kind]
}

// exec выполняет одну вставку данных пользователя.
func (im *importer) exec(ctx context.Context, query string, args ...any) error {
	_, err := im.db.Pool().Exec(ctx, query, args...)

	return err
}

// importDevice переносит устройство с именем и последней активностью.
func (im *importer) importDevice(ctx context.Context, record *dumpRecord) error {
	if record.DeviceID == "" {
		return errors.New("device без device_id")
	}

	return im.exec(ctx, `
		INSERT INTO devices (user_id, device_id, display_name, last_seen_ip, last_seen_at)
		VALUES ($1, $2, nullif($3, ''), $4, $5)
		ON CONFLICT (user_id, device_id) DO UPDATE
		SET display_name = EXCLUDED.display_name,
		    last_seen_ip = coalesce(EXCLUDED.last_seen_ip, devices.last_seen_ip),
		    last_seen_at = greatest(devices.last_seen_at, EXCLUDED.last_seen_at)`,
		record.UserID, record.DeviceID, record.Display, record.LastSeenIP, msTime(record.LastSeenMS))
}

// importExternalID переносит привязку учетки SSO (user_external_ids):
// вход через того же провайдера с тем же subject попадает в перенесенную учетку.
// Synapse - источник истины до переключения: повтор обновляет user_id.
func (im *importer) importExternalID(ctx context.Context, record *dumpRecord) error {
	if record.AuthProvider == "" || record.ExternalID == "" {
		return errors.New("user_external_id без auth_provider/external_id")
	}

	return im.exec(ctx, `
		INSERT INTO user_external_ids (auth_provider, external_id, user_id) VALUES ($1, $2, $3)
		ON CONFLICT (auth_provider, external_id) DO UPDATE SET user_id = EXCLUDED.user_id`,
		record.AuthProvider, record.ExternalID, record.UserID)
}

// importReceipt переносит отметку прочтения: без нее досчет
// непрочитанного после переезда считал историю от последнего своего сообщения, и
// старые чужие сообщения становились непрочитанными. Позиция события берется
// сразу, если события уже импортированы; иначе ее проставит импорт событий
// (store.ApplyImportedReceipts). Отметка не откатывается назад: более поздняя,
// поставленная уже в SYNGO, остается.
func (im *importer) importReceipt(ctx context.Context, record *dumpRecord) error {
	if record.RoomID == "" || record.EventID == "" || record.ReceiptType == "" {
		return errors.New("receipt без room_id/event_id/receipt_type")
	}

	return im.exec(ctx, `
		INSERT INTO receipts (room_id, user_id, receipt_type, thread_id, event_id, ts, event_stream)
		VALUES ($1, $2, $3, $4, $5, $6,
		        (SELECT stream_ordering FROM events WHERE event_id = $5 AND room_id = $1))
		ON CONFLICT (room_id, user_id, receipt_type, thread_id) DO UPDATE
		SET event_id = EXCLUDED.event_id, ts = EXCLUDED.ts, event_stream = EXCLUDED.event_stream
		WHERE receipts.event_stream IS NULL OR receipts.event_stream < EXCLUDED.event_stream`,
		record.RoomID, record.UserID, record.ReceiptType, record.ThreadID, record.EventID, record.TS)
}

// importAccessToken переносит access-токен (и парный refresh) как SHA-256: тот же
// хеш считает core.HashToken при каждом запросе, поэтому старый токен клиента
// продолжает работать. Срок жизни (valid_until_ms) сохраняется: истекший токен
// клиент обновит по refresh-токену.
func (im *importer) importAccessToken(ctx context.Context, record *dumpRecord) error {
	if record.Token == "" || record.DeviceID == "" {
		return errors.New("access_token без token/device_id")
	}

	var refreshHash []byte
	if record.RefreshToken != nil && *record.RefreshToken != "" {
		refreshHash = core.HashToken(*record.RefreshToken)
	}

	return im.exec(ctx, `
		INSERT INTO access_tokens (token_hash, user_id, device_id, refresh_hash, expires_at, last_validated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		core.HashToken(record.Token), record.UserID, record.DeviceID, refreshHash,
		msTime(record.ValidUntilMS), msTime(record.LastValidatedMS))
}

// importCrossSigningKey переносит ключ кросс-подписи; key_id - открытый ключ ed25519.
func (im *importer) importCrossSigningKey(ctx context.Context, record *dumpRecord) error {
	var key struct {
		Keys map[string]string `json:"keys"`
	}

	if err := json.Unmarshal(record.KeyJSON, &key); err != nil {
		return fmt.Errorf("cross_signing_key: %w", err)
	}

	pub := ""

	for keyID, value := range key.Keys {
		if strings.HasPrefix(keyID, "ed25519:") {
			pub = value
		}
	}

	if pub == "" || len(key.Keys) != 1 {
		return fmt.Errorf("cross_signing_key %s/%s: нужен ровно один ключ ed25519", record.UserID, record.KeyType)
	}

	return im.exec(ctx, `
		INSERT INTO e2e_cross_signing_keys (user_id, key_type, key_id, key_data) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, key_type) DO UPDATE
		SET key_id = EXCLUDED.key_id, key_data = EXCLUDED.key_data, updated_at = now()`,
		record.UserID, record.KeyType, pub, rawJSON(record.KeyJSON))
}

// importRoomKey переносит ключ сессии бэкапа в формате KeyBackupData.
func (im *importer) importRoomKey(ctx context.Context, record *dumpRecord) error {
	key, err := json.Marshal(map[string]any{
		"first_message_index": record.FirstMsgIndex,
		"forwarded_count":     record.ForwardedCount,
		"is_verified":         record.IsVerified,
		"session_data":        json.RawMessage(rawJSON(record.SessionData)),
	})
	if err != nil {
		return err
	}

	return im.exec(ctx, `
		INSERT INTO room_keys (user_id, version, room_id, session_id, key) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, version, room_id, session_id) DO UPDATE SET key = EXCLUDED.key`,
		record.UserID, record.Version, record.RoomID, record.SessionID, key)
}

// importPushRule переносит правило. Серверные правила (.m.rule.*) хранятся как
// переопределение enabled/actions с conditions = null (как SetDefaultPushRuleAttr);
// у content-правил шаблон - отдельное поле, у room/sender условия синтезируются.
func (im *importer) importPushRule(ctx context.Context, record *dumpRecord) error {
	if !pushrules.ValidKind(record.RuleKind) || record.RuleName == "" {
		return fmt.Errorf("push_rule: вид %q или id %q не поддержан", record.RuleKind, record.RuleName)
	}

	const upsert = `
		INSERT INTO push_rules (user_id, scope, kind, rule_id, priority, enabled, conditions, actions, pattern)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (user_id, scope, kind, rule_id) DO UPDATE
		SET priority = EXCLUDED.priority, enabled = EXCLUDED.enabled, conditions = EXCLUDED.conditions,
		    actions = EXCLUDED.actions, pattern = EXCLUDED.pattern, updated_at = now()`

	if pushrules.IsServerDefaultID(record.RuleName) {
		enabled := true
		if def, ok := pushrules.Defaults(record.UserID).Find(record.RuleKind, record.RuleName); ok {
			enabled = def.Enabled
		}

		if record.Enabled != nil {
			enabled = *record.Enabled
		}

		actions := json.RawMessage("null")
		if record.DefaultActions && len(record.Actions) > 0 {
			actions = record.Actions
		}

		return im.exec(ctx, upsert, record.UserID, record.Scope, record.RuleKind, record.RuleName,
			0, enabled, "null", string(actions), "")
	}

	if record.Priority == nil {
		// Только строка enabled без самого правила - правило удалено.
		return fmt.Errorf("push_rule %s: нет правила, только enabled", record.RuleName)
	}

	enabled := record.Enabled == nil || *record.Enabled
	conditions, pattern := json.RawMessage("[]"), ""

	switch record.RuleKind {
	case pushrules.KindContent:
		var conds []struct {
			Pattern string `json:"pattern"`
		}

		if err := json.Unmarshal(record.Conditions, &conds); err != nil || len(conds) == 0 || conds[0].Pattern == "" {
			return fmt.Errorf("push_rule %s: content без pattern", record.RuleName)
		}

		pattern = conds[0].Pattern
	case pushrules.KindOverride, pushrules.KindUnderride:
		conditions = json.RawMessage(rawJSON(record.Conditions))
	}

	return im.exec(ctx, upsert, record.UserID, record.Scope, record.RuleKind, record.RuleName,
		*record.Priority, enabled, string(conditions), rawJSON(record.Actions), pattern)
}

// finishUserData - действия после всех строк: текущая версия бэкапа ключей.
func (im *importer) finishUserData(ctx context.Context) error {
	if im.counts.RoomKeyVersion == 0 {
		return nil
	}

	return im.exec(ctx, `
		INSERT INTO room_keys_current (user_id, version)
		SELECT DISTINCT ON (user_id) user_id, version FROM room_keys_versions
		WHERE NOT deleted AND version ~ '^[0-9]+$'
		ORDER BY user_id, version::bigint DESC
		ON CONFLICT (user_id) DO UPDATE SET version = EXCLUDED.version`)
}

// printUserData печатает итоги по данным пользователей.
func (im *importer) printUserData() {
	c := im.counts
	fmt.Printf("данные пользователей: devices=%d access_tokens=%d device_keys=%d one_time_keys=%d "+
		"fallback_keys=%d cross_signing_keys=%d signatures=%d room_keys_versions=%d room_keys=%d "+
		"account_data=%d push_rules=%d pushers=%d filters=%d to_device=%d external_ids=%d receipts=%d\n",
		c.Devices, c.AccessTokens, c.DeviceKeys, c.OneTimeKeys, c.FallbackKeys, c.CrossSigning,
		c.Signatures, c.RoomKeyVersion, c.RoomKeys, c.AccountData, c.PushRules, c.Pushers, c.Filters, c.ToDevice,
		c.ExternalIDs, c.Receipts)

	if c.LockedAsDeactivated > 0 || c.Suspended > 0 || c.ShadowBanned > 0 {
		fmt.Printf("флаги без аналога в SYNGO: locked -> deactivated %d, suspended %d, shadow_banned %d "+
			"(перенесены как обычные пользователи)\n", c.LockedAsDeactivated, c.Suspended, c.ShadowBanned)
	}
}

// rawJSON - JSON-значение для jsonb-параметра; пустое - пустой объект.
func rawJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}

	return string(raw)
}

// msTime переводит миллисекунды Synapse во время (nil - NULL).
func msTime(ms *int64) *time.Time {
	if ms == nil || *ms <= 0 {
		return nil
	}

	t := time.UnixMilli(*ms).UTC()

	return &t
}

// nullable - пустая строка как NULL.
func nullable(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}
