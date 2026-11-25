-- Таблицы данных пользователей живого Synapse 1.165.0 (DDL из pg_dump, без строк) в схеме
-- synapse_fixture: на фикстуре событий export.sql исполняется целиком, включая выгрузку
-- устройств, токенов, E2EE и настроек. Источник - cmd/syngo-import/testdata/
-- synapse-1.165-users.sql (tools/synapse-export/fixture/gen.sh). Грузится после synapse-1.165.sql.

CREATE TABLE synapse_fixture.access_tokens (
    id bigint NOT NULL,
    user_id text NOT NULL,
    device_id text,
    token text NOT NULL,
    valid_until_ms bigint,
    puppets_user_id text,
    last_validated bigint,
    refresh_token_id bigint,
    used boolean
);

CREATE TABLE synapse_fixture.account_data (
    user_id text NOT NULL,
    account_data_type text NOT NULL,
    stream_id bigint NOT NULL,
    content text NOT NULL,
    instance_name text
);

CREATE TABLE synapse_fixture.device_inbox (
    user_id text NOT NULL,
    device_id text NOT NULL,
    stream_id bigint NOT NULL,
    message_json text NOT NULL,
    instance_name text
);

CREATE TABLE synapse_fixture.devices (
    user_id text NOT NULL,
    device_id text NOT NULL,
    display_name text,
    last_seen bigint,
    ip text,
    user_agent text,
    hidden boolean DEFAULT false
);

CREATE TABLE synapse_fixture.e2e_cross_signing_keys (
    user_id text NOT NULL,
    keytype text NOT NULL,
    keydata text NOT NULL,
    stream_id bigint NOT NULL,
    updatable_without_uia_before_ms bigint,
    instance_name text
);

CREATE TABLE synapse_fixture.e2e_cross_signing_signatures (
    user_id text NOT NULL,
    key_id text NOT NULL,
    target_user_id text NOT NULL,
    target_device_id text NOT NULL,
    signature text NOT NULL
);

CREATE TABLE synapse_fixture.e2e_device_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    ts_added_ms bigint NOT NULL,
    key_json text NOT NULL
);

CREATE TABLE synapse_fixture.e2e_fallback_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    algorithm text NOT NULL,
    key_id text NOT NULL,
    key_json text NOT NULL,
    used boolean DEFAULT false NOT NULL
);

CREATE TABLE synapse_fixture.e2e_one_time_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    algorithm text NOT NULL,
    key_id text NOT NULL,
    ts_added_ms bigint NOT NULL,
    key_json text NOT NULL
);

CREATE TABLE synapse_fixture.e2e_room_keys (
    user_id text NOT NULL,
    room_id text NOT NULL,
    session_id text NOT NULL,
    version bigint NOT NULL,
    first_message_index integer,
    forwarded_count integer,
    is_verified boolean,
    session_data text NOT NULL
);

CREATE TABLE synapse_fixture.e2e_room_keys_versions (
    user_id text NOT NULL,
    version bigint NOT NULL,
    algorithm text NOT NULL,
    auth_data text NOT NULL,
    deleted smallint DEFAULT 0 NOT NULL,
    etag bigint
);

CREATE TABLE synapse_fixture.push_rules (
    id bigint NOT NULL,
    user_name text NOT NULL,
    rule_id text NOT NULL,
    priority_class smallint NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    conditions text NOT NULL,
    actions text NOT NULL
);

CREATE TABLE synapse_fixture.push_rules_enable (
    id bigint NOT NULL,
    user_name text NOT NULL,
    rule_id text NOT NULL,
    enabled smallint
);

CREATE TABLE synapse_fixture.pushers (
    id bigint NOT NULL,
    user_name text NOT NULL,
    access_token bigint,
    profile_tag text NOT NULL,
    kind text NOT NULL,
    app_id text NOT NULL,
    app_display_name text NOT NULL,
    device_display_name text NOT NULL,
    pushkey text NOT NULL,
    ts bigint NOT NULL,
    lang text,
    data text,
    last_stream_ordering bigint,
    last_success bigint,
    failing_since bigint,
    enabled boolean,
    device_id text,
    instance_name text
);

CREATE TABLE synapse_fixture.refresh_tokens (
    id bigint NOT NULL,
    user_id text NOT NULL,
    device_id text NOT NULL,
    token text NOT NULL,
    next_token_id bigint,
    expiry_ts bigint,
    ultimate_session_expiry_ts bigint
);

CREATE TABLE synapse_fixture.room_account_data (
    user_id text NOT NULL,
    room_id text NOT NULL,
    account_data_type text NOT NULL,
    stream_id bigint NOT NULL,
    content text NOT NULL,
    instance_name text
);

CREATE TABLE synapse_fixture.room_tags (
    user_id text NOT NULL,
    room_id text NOT NULL,
    tag text NOT NULL,
    content text NOT NULL
);

CREATE TABLE synapse_fixture.user_filters (
    user_id text NOT NULL,
    filter_id bigint NOT NULL,
    filter_json bytea NOT NULL,
    full_user_id text
);

-- user_external_ids: в pg_dump фикстуры таблицы не было, DDL - по схеме
-- Synapse (synapse/storage/schema/main/full_schemas, уникальность auth_provider+external_id).
CREATE TABLE synapse_fixture.user_external_ids (
    auth_provider text NOT NULL,
    external_id text NOT NULL,
    user_id text NOT NULL,
    UNIQUE (auth_provider, external_id)
);

-- receipts_linearized: в pg_dump фикстуры таблицы не было, DDL - по схеме
-- Synapse (main/delta 72: event_stream_ordering, thread_id; уникальность отметки на
-- комнату, тип, пользователя и тред).
CREATE TABLE synapse_fixture.receipts_linearized (
    stream_id bigint NOT NULL,
    room_id text NOT NULL,
    receipt_type text NOT NULL,
    user_id text NOT NULL,
    event_id text NOT NULL,
    data text NOT NULL,
    instance_name text,
    event_stream_ordering bigint,
    thread_id text,
    UNIQUE (room_id, receipt_type, user_id, thread_id)
);
