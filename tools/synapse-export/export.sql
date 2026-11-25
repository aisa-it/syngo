-- Экспорт данных Synapse в JSONL-формат SYNGO (ADR-0006 §3).
--
-- Запуск на реплике Synapse (read-only роль):
--   psql "$SYNAPSE_DSN" -v ON_ERROR_STOP=1 -At -f tools/synapse-export/export.sql > synapse-dump.jsonl
--
-- Каждый SELECT отдаёт по строке JSON; строки склеиваются без заголовков (-At).
-- Медиа-файлы этим скриптом не копируются: см. README.md рядом.

-- Пользователи: localpart вычисляется из полного user_id (@local:server). Профиль
-- (displayname, avatar_url) - из profiles; флаги admin, locked, suspended, shadow_banned.
-- Пустой password_hash - учетки LDAP, SSO и appservice.
SELECT row_to_json(t)::text
FROM (
    SELECT 'user' AS kind,
           u.name AS user_id,
           split_part(substr(u.name, 2), ':', 1) AS localpart,
           coalesce(u.password_hash, '') AS password_hash,
           coalesce(p.displayname, '') AS display_name,
           coalesce(p.avatar_url, '') AS avatar_url,
           (u.deactivated <> 0) AS deactivated,
           (u.admin <> 0) AS admin,
           coalesce(u.locked, false) AS locked,
           coalesce(u.suspended, false) AS suspended,
           coalesce(u.shadow_banned, false) AS shadow_banned,
           u.appservice_id
    FROM users u
    LEFT JOIN profiles p ON p.full_user_id = u.name
    WHERE u.name LIKE '@%'
    ORDER BY u.name
) t;

-- Привязки учеток SSO: auth_provider - idp_id Synapse (oidc-<idp_id>),
-- external_id - subject провайдера. После переезда вход через SSO попадает в ту же
-- учетную запись. Идут сразу после пользователей: в SYNGO ссылаются на users.
SELECT row_to_json(t)::text
FROM (
    SELECT 'user_external_id' AS kind, e.user_id, e.auth_provider, e.external_id
    FROM user_external_ids e
    JOIN users u ON u.name = e.user_id
    ORDER BY e.user_id, e.auth_provider, e.external_id
) t;

-- Комнаты.
SELECT row_to_json(t)::text
FROM (
    SELECT 'room' AS kind,
           room_id,
           coalesce(room_version, '10') AS room_version,
           coalesce(creator, '') AS creator,
           coalesce(is_public, false) AS is_public
    FROM rooms
    ORDER BY room_id
) t;

-- События здесь не выгружаются: их выгружает syngo-synapse-export (все
-- stream_ordering, исходный PDU, манифест). Раньше здесь была вторая
-- копия событий с инвертированным outlier, которую runbook выбрасывал grep'ом.

-- Алиасы комнат.
SELECT row_to_json(t)::text
FROM (
    SELECT 'room_alias' AS kind,
           room_alias AS alias,
           room_id,
           coalesce(creator, '') AS creator
    FROM room_aliases
    ORDER BY room_alias
) t;

-- Текущее членство (материализация; история членства уже есть в событиях).
SELECT row_to_json(t)::text
FROM (
    SELECT DISTINCT ON (rm.room_id, rm.user_id)
           'membership' AS kind,
           rm.room_id,
           rm.user_id,
           rm.membership,
           (rm.forgotten <> 0) AS forgotten,
           rm.event_id
    FROM room_memberships rm
    JOIN current_state_events cse
      ON cse.room_id = rm.room_id AND cse.event_id = rm.event_id
    ORDER BY rm.room_id, rm.user_id, rm.event_id
) t;

-- Метаданные медиа (файлы копируются отдельно): синтаксис нашего importer'а — kind=media.
-- quarantined_by (карантин переносится и соблюдается), url_cache (превью ссылок:
-- syngo-import-media их пропускает с отдельным статусом), sha256 (сверка целостности файла;
-- колонки нет в старых Synapse - берется через to_jsonb, без ошибки). Миниатюры
-- (local_media_repository_thumbnails) не выгружаются: syngod строит их по запросу.
-- remote_media_cache не выгружается: федерация вне скоупа, чужие медиа - кеш.
SELECT row_to_json(t)::text
FROM (
    SELECT 'media' AS kind,
           m.media_id,
           m.media_type,
           m.media_length,
           m.upload_name,
           m.user_id,
           m.created_ts AS created_at_ms,
           m.quarantined_by,
           m.url_cache,
           to_jsonb(m) ->> 'sha256' AS sha256
    FROM local_media_repository m
    ORDER BY m.media_id
) t;

-- ===== Данные пользователей: сессии, E2EE, настройки клиента. =====
-- Строки ниже идут после пользователей: в SYNGO они ссылаются на users и devices.

-- Устройства. Скрытые (hidden) - это ключи кросс-подписи, которые Synapse хранит
-- как устройства; они переносятся отдельно (cross_signing_key).
SELECT row_to_json(t)::text
FROM (
    SELECT 'device' AS kind, user_id, device_id,
           coalesce(display_name, '') AS display_name,
           last_seen AS last_seen_ms,
           ip AS last_seen_ip
    FROM devices
    WHERE NOT hidden
    ORDER BY user_id, device_id
) t;

-- Access-токены с парным refresh-токеном. Synapse хранит токены открыто, SYNGO -
-- только SHA-256: хеш считает импорт, клиенты остаются в сессии. Токены без
-- устройства (admin API) не переносятся.
SELECT row_to_json(t)::text
FROM (
    SELECT 'access_token' AS kind, a.user_id, a.device_id, a.token,
           r.token AS refresh_token,
           a.valid_until_ms,
           a.last_validated AS last_validated_ms
    FROM access_tokens a
    LEFT JOIN refresh_tokens r ON r.id = a.refresh_token_id
    WHERE a.device_id IS NOT NULL
    ORDER BY a.id
) t;

-- Ключи устройств (объект /keys/upload целиком).
SELECT row_to_json(t)::text
FROM (
    SELECT 'device_keys' AS kind, k.user_id, k.device_id, k.key_json::jsonb AS key_json
    FROM e2e_device_keys_json k
    JOIN devices d ON d.user_id = k.user_id AND d.device_id = k.device_id AND NOT d.hidden
    ORDER BY k.user_id, k.device_id
) t;

-- One-time keys (выданные Synapse уже удалил) и fallback keys.
SELECT row_to_json(t)::text
FROM (
    SELECT 'one_time_key' AS kind, user_id, device_id, algorithm, key_id, key_json::jsonb AS key_json
    FROM e2e_one_time_keys_json
    ORDER BY user_id, device_id, algorithm, key_id
) t;

SELECT row_to_json(t)::text
FROM (
    SELECT 'fallback_key' AS kind, user_id, device_id, algorithm, key_id,
           key_json::jsonb AS key_json, used
    FROM e2e_fallback_keys_json
    ORDER BY user_id, device_id, algorithm
) t;

-- Ключи кросс-подписи: действующий ключ каждого типа - с наибольшим stream_id.
SELECT row_to_json(t)::text
FROM (
    SELECT DISTINCT ON (user_id, keytype)
           'cross_signing_key' AS kind, user_id, keytype AS key_type, keydata::jsonb AS key_json
    FROM e2e_cross_signing_keys
    ORDER BY user_id, keytype, stream_id DESC
) t;

-- Подписи устройств и ключей (user_id/key_id - подписант).
SELECT row_to_json(t)::text
FROM (
    SELECT 'cross_signing_signature' AS kind, user_id, key_id, target_user_id, target_device_id, signature
    FROM e2e_cross_signing_signatures
    ORDER BY user_id, key_id, target_user_id, target_device_id
) t;

-- Бэкап ключей комнат: версии (в том числе удаленные - номер не выдается повторно)
-- и ключи сессий.
SELECT row_to_json(t)::text
FROM (
    SELECT 'room_keys_version' AS kind, user_id, version::text AS version, algorithm,
           auth_data::jsonb AS auth_data, (deleted <> 0) AS deleted,
           coalesce(etag, 0)::text AS etag
    FROM e2e_room_keys_versions
    ORDER BY user_id, version
) t;

SELECT row_to_json(t)::text
FROM (
    SELECT 'room_key' AS kind, user_id, version::text AS version, room_id, session_id,
           first_message_index, forwarded_count, is_verified,
           session_data::jsonb AS session_data
    FROM e2e_room_keys
    ORDER BY user_id, version, room_id, session_id
) t;

-- Account data: глобальная, комнатная и теги (в SYNGO теги - комнатная m.tag).
SELECT row_to_json(t)::text
FROM (
    SELECT 'account_data' AS kind, user_id, '' AS room_id, account_data_type AS type,
           content::jsonb AS content
    FROM account_data
    UNION ALL
    SELECT 'account_data', user_id, room_id, account_data_type, content::jsonb
    FROM room_account_data
    WHERE account_data_type <> 'm.tag'
    UNION ALL
    SELECT 'account_data', user_id, room_id, 'm.tag',
           jsonb_build_object('tags', jsonb_object_agg(tag, content::jsonb))
    FROM room_tags
    GROUP BY user_id, room_id
    ORDER BY 2, 3, 4
) t;

-- Отметки прочтения: m.read и m.read.private локальных пользователей
-- из receipts_linearized (последняя отметка на комнату, тип и тред; receipts_graph
-- не нужен). Без них досчет непрочитанного после переезда считал историю от
-- последнего своего сообщения - старые чужие сообщения становились непрочитанными.
-- thread_id - через to_jsonb (колонки нет в старых Synapse), NULL - без треда.
SELECT row_to_json(t)::text
FROM (
    SELECT 'receipt' AS kind, r.room_id, r.user_id, r.receipt_type, r.event_id,
           coalesce(to_jsonb(r) ->> 'thread_id', '') AS thread_id,
           coalesce((r.data::jsonb ->> 'ts')::numeric, 0)::bigint AS ts
    FROM receipts_linearized r
    JOIN users u ON u.name = r.user_id
    WHERE r.receipt_type IN ('m.read', 'm.read.private')
    ORDER BY 2, 3, 4, 6
) t;

-- Push rules: пользовательские правила и переопределения серверных (.m.rule.*).
-- rule_id Synapse - "<scope>/<kind>/<id>"; priority - место в своем виде (0 - первое),
-- в Synapse больший priority идет раньше. priority_class -1 - новые actions серверного
-- правила; enabled - из push_rules_enable (null - не менялся).
SELECT row_to_json(t)::text
FROM (
    SELECT 'push_rule' AS kind,
           ids.user_name AS user_id,
           split_part(ids.rule_id, '/', 1) AS scope,
           split_part(ids.rule_id, '/', 2) AS rule_kind,
           regexp_replace(ids.rule_id, '^[^/]*/[^/]*/', '') AS rule_name,
           r.rank AS priority,
           coalesce(r.priority_class = -1, false) AS default_actions,
           CASE WHEN e.enabled IS NULL THEN NULL ELSE e.enabled <> 0 END AS enabled,
           r.conditions::jsonb AS conditions,
           r.actions::jsonb AS actions
    FROM (SELECT user_name, rule_id FROM push_rules
          UNION SELECT user_name, rule_id FROM push_rules_enable) ids
    LEFT JOIN (SELECT user_name, rule_id, priority_class, conditions, actions,
                      row_number() OVER (PARTITION BY user_name, priority_class
                                         ORDER BY priority DESC, rule_id) - 1 AS rank
               FROM push_rules) r USING (user_name, rule_id)
    LEFT JOIN push_rules_enable e USING (user_name, rule_id)
    ORDER BY 2, 4, 6, 5
) t;

-- Pushers (выключенные не переносятся).
SELECT row_to_json(t)::text
FROM (
    SELECT 'pusher' AS kind, user_name AS user_id, app_id, pushkey,
           kind AS pusher_kind, coalesce(app_display_name, '') AS app_display_name,
           coalesce(device_display_name, '') AS device_display_name,
           coalesce(profile_tag, '') AS profile_tag, coalesce(lang, '') AS lang,
           coalesce(data, '{}')::jsonb AS data, device_id
    FROM pushers
    WHERE enabled IS NOT false
    ORDER BY user_name, app_id, pushkey
) t;

-- Фильтры /user/{userId}/filter (filter_id сохраняется: клиент хранит его у себя).
SELECT row_to_json(t)::text
FROM (
    SELECT 'filter' AS kind, full_user_id AS user_id, filter_id::text AS filter_id,
           convert_from(filter_json, 'UTF8')::jsonb AS filter
    FROM user_filters
    WHERE full_user_id IS NOT NULL
    ORDER BY full_user_id, filter_id
) t;

-- Недоставленные to-device (запросы ключей, olm-сессии устройств, не бывших в сети).
SELECT row_to_json(t)::text
FROM (
    SELECT 'to_device' AS kind, user_id, device_id, stream_id, message_json::jsonb AS message
    FROM device_inbox
    ORDER BY stream_id
) t;

-- Сверка (syngo-import -verify): число строк каждого вида по пользователям источника.
SELECT row_to_json(t)::text
FROM (
    SELECT 'user_stats' AS kind, u.name AS user_id,
           json_build_object(
               'devices', (SELECT count(*) FROM devices d WHERE d.user_id = u.name AND NOT d.hidden),
               'access_tokens', (SELECT count(*) FROM access_tokens a WHERE a.user_id = u.name AND a.device_id IS NOT NULL),
               'device_keys', (SELECT count(*) FROM e2e_device_keys_json k JOIN devices d
                                   ON d.user_id = k.user_id AND d.device_id = k.device_id AND NOT d.hidden
                               WHERE k.user_id = u.name),
               'one_time_keys', (SELECT count(*) FROM e2e_one_time_keys_json k WHERE k.user_id = u.name),
               'fallback_keys', (SELECT count(*) FROM e2e_fallback_keys_json k WHERE k.user_id = u.name),
               'cross_signing_keys', (SELECT count(DISTINCT keytype) FROM e2e_cross_signing_keys k WHERE k.user_id = u.name),
               'signatures', (SELECT count(*) FROM e2e_cross_signing_signatures s WHERE s.user_id = u.name),
               'room_keys_versions', (SELECT count(*) FROM e2e_room_keys_versions v WHERE v.user_id = u.name),
               'room_keys', (SELECT count(*) FROM e2e_room_keys k WHERE k.user_id = u.name),
               'account_data', (SELECT count(*) FROM account_data a WHERE a.user_id = u.name)
                             + (SELECT count(*) FROM room_account_data a WHERE a.user_id = u.name AND a.account_data_type <> 'm.tag')
                             + (SELECT count(DISTINCT room_id) FROM room_tags g WHERE g.user_id = u.name),
               'push_rules', (SELECT count(*) FROM (SELECT rule_id FROM push_rules p WHERE p.user_name = u.name
                                                    UNION SELECT rule_id FROM push_rules_enable e WHERE e.user_name = u.name) r),
               'pushers', (SELECT count(*) FROM pushers p WHERE p.user_name = u.name AND p.enabled IS NOT false),
               'filters', (SELECT count(*) FROM user_filters f WHERE f.full_user_id = u.name),
               'to_device', (SELECT count(*) FROM device_inbox i WHERE i.user_id = u.name),
               'external_ids', (SELECT count(*) FROM user_external_ids e WHERE e.user_id = u.name),
               'receipts', (SELECT count(*) FROM receipts_linearized r WHERE r.user_id = u.name
                                AND r.receipt_type IN ('m.read', 'm.read.private'))
           ) AS stats
    FROM users u
    WHERE u.name LIKE '@%'
    ORDER BY u.name
) t;
