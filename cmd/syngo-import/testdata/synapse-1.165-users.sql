-- Фикстура: таблицы пользователей живого Synapse v1.165.0.
-- Сгенерирована tools/synapse-export/fixture/gen.sh, руками не править.
CREATE SCHEMA IF NOT EXISTS synapse;
--
-- PostgreSQL database dump
--


-- Dumped from database version 16.15
-- Dumped by pg_dump version 16.15

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: access_tokens; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.access_tokens (
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


--
-- Name: account_data; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.account_data (
    user_id text NOT NULL,
    account_data_type text NOT NULL,
    stream_id bigint NOT NULL,
    content text NOT NULL,
    instance_name text
);


--
-- Name: current_state_events; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.current_state_events (
    event_id text NOT NULL,
    room_id text NOT NULL,
    type text NOT NULL,
    state_key text NOT NULL,
    membership text,
    event_stream_ordering bigint
);


--
-- Name: device_inbox; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.device_inbox (
    user_id text NOT NULL,
    device_id text NOT NULL,
    stream_id bigint NOT NULL,
    message_json text NOT NULL,
    instance_name text
);


--
-- Name: devices; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.devices (
    user_id text NOT NULL,
    device_id text NOT NULL,
    display_name text,
    last_seen bigint,
    ip text,
    user_agent text,
    hidden boolean DEFAULT false
);


--
-- Name: e2e_cross_signing_keys; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_cross_signing_keys (
    user_id text NOT NULL,
    keytype text NOT NULL,
    keydata text NOT NULL,
    stream_id bigint NOT NULL,
    updatable_without_uia_before_ms bigint,
    instance_name text
);


--
-- Name: e2e_cross_signing_signatures; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_cross_signing_signatures (
    user_id text NOT NULL,
    key_id text NOT NULL,
    target_user_id text NOT NULL,
    target_device_id text NOT NULL,
    signature text NOT NULL
);


--
-- Name: e2e_device_keys_json; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_device_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    ts_added_ms bigint NOT NULL,
    key_json text NOT NULL
);


--
-- Name: e2e_fallback_keys_json; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_fallback_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    algorithm text NOT NULL,
    key_id text NOT NULL,
    key_json text NOT NULL,
    used boolean DEFAULT false NOT NULL
);


--
-- Name: e2e_one_time_keys_json; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_one_time_keys_json (
    user_id text NOT NULL,
    device_id text NOT NULL,
    algorithm text NOT NULL,
    key_id text NOT NULL,
    ts_added_ms bigint NOT NULL,
    key_json text NOT NULL
);


--
-- Name: e2e_room_keys; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_room_keys (
    user_id text NOT NULL,
    room_id text NOT NULL,
    session_id text NOT NULL,
    version bigint NOT NULL,
    first_message_index integer,
    forwarded_count integer,
    is_verified boolean,
    session_data text NOT NULL
);


--
-- Name: e2e_room_keys_versions; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.e2e_room_keys_versions (
    user_id text NOT NULL,
    version bigint NOT NULL,
    algorithm text NOT NULL,
    auth_data text NOT NULL,
    deleted smallint DEFAULT 0 NOT NULL,
    etag bigint
);


--
-- Name: event_json; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.event_json (
    event_id text NOT NULL,
    room_id text NOT NULL,
    internal_metadata text NOT NULL,
    json text NOT NULL,
    format_version integer
);


--
-- Name: events; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.events (
    topological_ordering bigint NOT NULL,
    event_id text NOT NULL,
    type text NOT NULL,
    room_id text NOT NULL,
    content text,
    unrecognized_keys text,
    processed boolean NOT NULL,
    outlier boolean NOT NULL,
    depth bigint DEFAULT 0 NOT NULL,
    origin_server_ts bigint,
    received_ts bigint,
    sender text,
    contains_url boolean,
    instance_name text,
    stream_ordering bigint,
    state_key text,
    rejection_reason text
);


--
-- Name: local_media_repository; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.local_media_repository (
    media_id text,
    media_type text,
    media_length integer,
    created_ts bigint,
    upload_name text,
    user_id text,
    quarantined_by text,
    url_cache text,
    last_access_ts bigint,
    safe_from_quarantine boolean DEFAULT false NOT NULL,
    authenticated boolean DEFAULT false NOT NULL,
    sha256 text
);


--
-- Name: profiles; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.profiles (
    user_id text NOT NULL,
    displayname text,
    avatar_url text,
    full_user_id text,
    fields jsonb
);


--
-- Name: push_rules; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.push_rules (
    id bigint NOT NULL,
    user_name text NOT NULL,
    rule_id text NOT NULL,
    priority_class smallint NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    conditions text NOT NULL,
    actions text NOT NULL
);


--
-- Name: push_rules_enable; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.push_rules_enable (
    id bigint NOT NULL,
    user_name text NOT NULL,
    rule_id text NOT NULL,
    enabled smallint
);


--
-- Name: pushers; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.pushers (
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


--
-- Name: refresh_tokens; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.refresh_tokens (
    id bigint NOT NULL,
    user_id text NOT NULL,
    device_id text NOT NULL,
    token text NOT NULL,
    next_token_id bigint,
    expiry_ts bigint,
    ultimate_session_expiry_ts bigint
);


--
-- Name: rejections; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.rejections (
    event_id text NOT NULL,
    reason text NOT NULL,
    last_check text NOT NULL
);


--
-- Name: room_account_data; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.room_account_data (
    user_id text NOT NULL,
    room_id text NOT NULL,
    account_data_type text NOT NULL,
    stream_id bigint NOT NULL,
    content text NOT NULL,
    instance_name text
);


--
-- Name: room_aliases; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.room_aliases (
    room_alias text NOT NULL,
    room_id text NOT NULL,
    creator text
);


--
-- Name: room_memberships; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.room_memberships (
    event_id text NOT NULL,
    user_id text NOT NULL,
    sender text NOT NULL,
    room_id text NOT NULL,
    membership text NOT NULL,
    forgotten integer DEFAULT 0,
    display_name text,
    avatar_url text,
    event_stream_ordering bigint,
    participant boolean DEFAULT false
);


--
-- Name: room_tags; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.room_tags (
    user_id text NOT NULL,
    room_id text NOT NULL,
    tag text NOT NULL,
    content text NOT NULL
);


--
-- Name: rooms; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.rooms (
    room_id text NOT NULL,
    is_public boolean,
    creator text,
    room_version text,
    has_auth_chain_index boolean
);


--
-- Name: user_filters; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.user_filters (
    user_id text NOT NULL,
    filter_id bigint NOT NULL,
    filter_json bytea NOT NULL,
    full_user_id text
);


--
-- Name: users; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.users (
    name text,
    password_hash text,
    creation_ts bigint,
    admin smallint DEFAULT 0 NOT NULL,
    upgrade_ts bigint,
    is_guest smallint DEFAULT 0 NOT NULL,
    appservice_id text,
    consent_version text,
    consent_server_notice_sent text,
    user_type text,
    deactivated smallint DEFAULT 0 NOT NULL,
    shadow_banned boolean,
    consent_ts bigint,
    approved boolean,
    locked boolean DEFAULT false NOT NULL,
    suspended boolean DEFAULT false NOT NULL
);


--
-- Data for Name: access_tokens; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.access_tokens (id, user_id, device_id, token, valid_until_ms, puppets_user_id, last_validated, refresh_token_id, used) FROM stdin;
2	@alice:fixture.local	FWPMLJSWKV	syt_YWxpY2U_VUwZchMaUcrugpEkxatX_4WuXPk	\N	\N	1795198095875	\N	f
3	@bob:fixture.local	DDAPVIWQXY	syt_Ym9i_kSwRvFLStoDkeHCtAHbG_3YBdWx	\N	\N	1795198096772	\N	f
6	@eve:fixture.local	INAJITMGRC	syt_ZXZl_hsRKTJmMbhFmjVJeZDay_1tm7DS	\N	\N	1795198098943	\N	f
7	@frank:fixture.local	DHZXULYQLT	syt_ZnJhbms_fiVpOZqtELgMmpBqEMha_1m5g0V	\N	\N	1795198099585	\N	f
4	@carol:fixture.local	JTABHXPRPP	syt_Y2Fyb2w_ZfFCtoDGnYTJIiUcbPdx_1GzV0a	\N	\N	1795198097526	\N	t
9	@alice:fixture.local	ALICELAPTOP	syt_YWxpY2U_bOvgKXRKGQBNtMbnvdUd_0113B8	\N	\N	1795198103681	\N	t
8	@alice:fixture.local	ALICEPHONE	syt_YWxpY2U_WFpJEBZUKcHMxBYwJYhW_4Z8dBw	1795198401767	\N	1795198103065	2	t
10	@bob:fixture.local	BOBDEV	syt_Ym9i_vRkeMzPgIaIlpgakRoDL_05grLn	\N	\N	1795198104361	\N	t
\.


--
-- Data for Name: account_data; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.account_data (user_id, account_data_type, stream_id, content, instance_name) FROM stdin;
@alice:fixture.local	m.direct	2	{"@bob:fixture.local":["!UqZyOvgBsnlvTQIuwk:fixture.local"]}	\N
@alice:fixture.local	m.secret_storage.default_key	3	{"key":"SSSSKEY"}	\N
@alice:fixture.local	m.secret_storage.key.SSSSKEY	4	{"algorithm":"m.secret_storage.v1.aes-hmac-sha2","iv":"aXY","mac":"bWFj"}	\N
@alice:fixture.local	m.cross_signing.master	5	{"encrypted":{"SSSSKEY":{"iv":"aXY","ciphertext":"Y3Q","mac":"bWFj"}}}	\N
@alice:fixture.local	m.megolm_backup.v1	6	{"encrypted":{"SSSSKEY":{"iv":"aXY","ciphertext":"Ymt1cA","mac":"bWFj"}}}	\N
@alice:fixture.local	im.vector.setting.breadcrumbs	7	{"recent_rooms":["!UqZyOvgBsnlvTQIuwk:fixture.local"]}	\N
\.


--
-- Data for Name: current_state_events; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.current_state_events (event_id, room_id, type, state_key, membership, event_stream_ordering) FROM stdin;
$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.create		\N	2
$FjvyrVoMuOz75mavO0X1uUWlGVP6Yb13-8LD7Ld1KQE	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.guest_access		\N	7
$5xNHyr6rp5vVrccY1pwEs8Ubdi9BSScWsToH499gmcg	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.history_visibility		\N	6
$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.join_rules		\N	5
$OAMX21jwEnQIPtFWMsUPHjxOz3OQnoYUY-okNzolKQI	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.name		\N	8
$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.power_levels		\N	4
$SDGEe_zhK6gDq0yPo3YbEyMC_yMNhROnPRIJyhMUWKU	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.member	@bob:fixture.local	join	10
$OwKWDldjKxUecka68PgBcRZNYwTwCG6gz_fSqy1u1g8	!UqZyOvgBsnlvTQIuwk:fixture.local	m.room.member	@alice:fixture.local	join	13
\.


--
-- Data for Name: device_inbox; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.device_inbox (user_id, device_id, stream_id, message_json, instance_name) FROM stdin;
@alice:fixture.local	ALICEPHONE	2	{"content":{"action":"request","request_id":"req1","requesting_device_id":"BOBDEV"},"type":"m.room_key_request","sender":"@bob:fixture.local"}	master
\.


--
-- Data for Name: devices; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.devices (user_id, device_id, display_name, last_seen, ip, user_agent, hidden) FROM stdin;
@alice:fixture.local	FWPMLJSWKV	\N	\N	\N	\N	f
@bob:fixture.local	DDAPVIWQXY	\N	\N	\N	\N	f
@eve:fixture.local	INAJITMGRC	\N	\N	\N	\N	f
@frank:fixture.local	DHZXULYQLT	\N	\N	\N	\N	f
@carol:fixture.local	JTABHXPRPP	\N	1795198100176	127.0.0.1	Python-urllib/3.13	f
@alice:fixture.local	jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U	master signing key	\N	\N	\N	t
@alice:fixture.local	1lP/Od383ce1S/ZMlN5Ajw031KgvdIuMvSfOAFxinb4	self_signing signing key	\N	\N	\N	t
@alice:fixture.local	NwHCQHTi36/68FVhAEf2tpZeM7Ws4sVrquv51XT6HPg	user_signing signing key	\N	\N	\N	t
@bob:fixture.local	uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc	master signing key	\N	\N	\N	t
@bob:fixture.local	A6OlSR8TOUVUDJDoaCEp61Ha1OMLBcYNZcUqquw0xf0	self_signing signing key	\N	\N	\N	t
@bob:fixture.local	UvwVFMDd78bzG8nwBoh+f4Ssf0H9tDukPT+R4tbedXU	user_signing signing key	\N	\N	\N	t
@alice:fixture.local	ALICELAPTOP	Alice laptop	1795198104398	127.0.0.1	Python-urllib/3.13	f
@alice:fixture.local	ALICEPHONE	Alice phone	1795198104419	127.0.0.1	Python-urllib/3.13	f
@bob:fixture.local	BOBDEV	Bob desktop	1795198104777	127.0.0.1	Python-urllib/3.13	f
\.


--
-- Data for Name: e2e_cross_signing_keys; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_cross_signing_keys (user_id, keytype, keydata, stream_id, updatable_without_uia_before_ms, instance_name) FROM stdin;
@alice:fixture.local	master	{"user_id":"@alice:fixture.local","usage":["master"],"keys":{"ed25519:jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U":"jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U"}}	2	\N	master
@alice:fixture.local	self_signing	{"user_id":"@alice:fixture.local","usage":["self_signing"],"keys":{"ed25519:1lP/Od383ce1S/ZMlN5Ajw031KgvdIuMvSfOAFxinb4":"1lP/Od383ce1S/ZMlN5Ajw031KgvdIuMvSfOAFxinb4"},"signatures":{"@alice:fixture.local":{"ed25519:jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U":"YRoqba71rTJOr6L6F3pundnC5j+oRlf7/0pGlp5xJcZW/EOs52gl3CkxF+uIf5j7HTCUPhE9j2ABH0254YmhCg"}}}	3	\N	master
@alice:fixture.local	user_signing	{"user_id":"@alice:fixture.local","usage":["user_signing"],"keys":{"ed25519:NwHCQHTi36/68FVhAEf2tpZeM7Ws4sVrquv51XT6HPg":"NwHCQHTi36/68FVhAEf2tpZeM7Ws4sVrquv51XT6HPg"},"signatures":{"@alice:fixture.local":{"ed25519:jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U":"u3R7djFklLjocsx5kzjQvk5l4nol/EsCiH1nf35rRzXc8KZBognTAoJb/JmO33ep/tgJE1nJvb3ep9ZpEuxeBg"}}}	4	\N	master
@bob:fixture.local	master	{"user_id":"@bob:fixture.local","usage":["master"],"keys":{"ed25519:uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc":"uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc"}}	5	\N	master
@bob:fixture.local	self_signing	{"user_id":"@bob:fixture.local","usage":["self_signing"],"keys":{"ed25519:A6OlSR8TOUVUDJDoaCEp61Ha1OMLBcYNZcUqquw0xf0":"A6OlSR8TOUVUDJDoaCEp61Ha1OMLBcYNZcUqquw0xf0"},"signatures":{"@bob:fixture.local":{"ed25519:uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc":"vWxM0Q9Oa8M7WBEkRWfcTKDeu1KNFm073bqFeNZLsZh2qHNMKpIXI1FDdmUM6+3BkBO16chFPl0T3EavL747Aw"}}}	6	\N	master
@bob:fixture.local	user_signing	{"user_id":"@bob:fixture.local","usage":["user_signing"],"keys":{"ed25519:UvwVFMDd78bzG8nwBoh+f4Ssf0H9tDukPT+R4tbedXU":"UvwVFMDd78bzG8nwBoh+f4Ssf0H9tDukPT+R4tbedXU"},"signatures":{"@bob:fixture.local":{"ed25519:uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc":"dfGz2rFuQoiLUuESV44Oj0qHkHqbmZTOIkOrPnlK804eexMs8oDjnwhREVllkm58vd2kmO0FKeOi+n2SBQP7Cw"}}}	7	\N	master
\.


--
-- Data for Name: e2e_cross_signing_signatures; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_cross_signing_signatures (user_id, key_id, target_user_id, target_device_id, signature) FROM stdin;
@alice:fixture.local	ed25519:1lP/Od383ce1S/ZMlN5Ajw031KgvdIuMvSfOAFxinb4	@alice:fixture.local	ALICELAPTOP	fLHbjdfYtZAutCvPCHmYLGwGDH9Obsu1TAm3/SYWWnviZn/SNRowAB5mxoO3oVPjrdPRXI/GBJPnLf3asoYjBw
@alice:fixture.local	ed25519:ALICELAPTOP	@alice:fixture.local	jOySSrxsrFLBjShcNgBs4G8fIkAs+UiZOds1TiC1f7U	jHdJ26WR8hR5q1NGqbmChJkdQ3fAghqTftYLLDppSwdccprnFDE6fuZdutiSn1Nv8D/CXnFmFTpGZRWVVZZyDw
@alice:fixture.local	ed25519:NwHCQHTi36/68FVhAEf2tpZeM7Ws4sVrquv51XT6HPg	@bob:fixture.local	uYCuR2gVnecoFwKDZ/QGVkm9IyafIxzG9Md8kTlnNkc	SJue1d1w8No87eC9LkFZEdcp8Olus+cOMiLWlFpU+3Gft7BowdwQ//fm+I5WsN1/mM5OfCZKtYWr5bXBKiddCg
@bob:fixture.local	ed25519:A6OlSR8TOUVUDJDoaCEp61Ha1OMLBcYNZcUqquw0xf0	@bob:fixture.local	BOBDEV	38es2areLIbq1vhcf/Dlyaa0pF5eNooZ127X75zmgJ+RKaY1Xq9Dk+uJ9yB6a3WbkFMKpXDqOI9tO6INyz4PCw
\.


--
-- Data for Name: e2e_device_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_device_keys_json (user_id, device_id, ts_added_ms, key_json) FROM stdin;
@alice:fixture.local	ALICEPHONE	1795198104419	{"algorithms":["m.olm.v1.curve25519-aes-sha2","m.megolm.v1.aes-sha2"],"device_id":"ALICEPHONE","keys":{"curve25519:ALICEPHONE":"UL0bve6bymWdZfG3C5kVUxkcxAoaI+PzmEQZKLNoIyw","ed25519:ALICEPHONE":"ezUY7A9TsrA6ASY9+JStpJ9p5WZdYmSwTjBFS3EDOPI"},"signatures":{"@alice:fixture.local":{"ed25519:ALICEPHONE":"Gt1loONdUasKqJWENsEBe/990Pscy/U8LmTC3hLU7k+6IQhF4dDQv28wiI6hD1boaB5SShVMnw+E2jzKGAh7Bg"}},"user_id":"@alice:fixture.local"}
@alice:fixture.local	ALICELAPTOP	1795198104580	{"algorithms":["m.olm.v1.curve25519-aes-sha2","m.megolm.v1.aes-sha2"],"device_id":"ALICELAPTOP","keys":{"curve25519:ALICELAPTOP":"u65pqYWkS00DBkB8xNsSejW78+c/hMKiy2+H/0QAFkI","ed25519:ALICELAPTOP":"q5hucY8OSPGQmgC7/hqctsgAdvcc4Eb2TDilmN2FHW4"},"signatures":{"@alice:fixture.local":{"ed25519:ALICELAPTOP":"ajBndChKfCfTckeKAPtNDWnxzKPlR+DOD38H99W2Qqkk4oCC+6mEuiQ9O7Kuv76bVVLQ2s6CfyP0KQySK814CA"}},"user_id":"@alice:fixture.local"}
@bob:fixture.local	BOBDEV	1795198104778	{"algorithms":["m.olm.v1.curve25519-aes-sha2","m.megolm.v1.aes-sha2"],"device_id":"BOBDEV","keys":{"curve25519:BOBDEV":"pfLBGwyNuN3AnsdYV7Gs6scfmM0wBYAOJNSYxWNnsCk","ed25519:BOBDEV":"+YdFLiRM7skgdL8wSOQ+J54u2Kh6Y+Q8bACNUsixwF4"},"signatures":{"@bob:fixture.local":{"ed25519:BOBDEV":"6uZDQPlJhGATGAhWHPFVTxE1SBmYkBp0EUQCyUxi1I0OoEsI6USDtai2NAO12v9zGOIhPYLo87Iz0KUvQWrgCg"}},"user_id":"@bob:fixture.local"}
\.


--
-- Data for Name: e2e_fallback_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_fallback_keys_json (user_id, device_id, algorithm, key_id, key_json, used) FROM stdin;
@alice:fixture.local	ALICEPHONE	signed_curve25519	ALICEPHONEFB	{"key":"COcfefJrNka61Hvt9QqJXIino/7oAQv/eCyDOK5O2VE","signatures":{"@alice:fixture.local":{"ed25519:ALICEPHONE":"BOd0z3Kk/ANrUzlkzfJAZ7F6+QEtilDqpP8N39kRNQRHu0SeTIPeLKwqA51zACQsfozK9Q11gqofPWBRgWxZDA"}},"fallback":true}	f
@alice:fixture.local	ALICELAPTOP	signed_curve25519	ALICELAPTOPFB	{"key":"Opzdg5elz/5TGQ9S3LUPu5IOqT+se0XwvxrBDXlicAE","signatures":{"@alice:fixture.local":{"ed25519:ALICELAPTOP":"K9hN5c5hPy5HnrJ4DbWSCclHrW2p6pqX0NOaZ9SLS4LJ3eKsa0Sqbew6yOOlhC3PvNPOgqj9ArPJvbU4ZPMiBw"}},"fallback":true}	f
@bob:fixture.local	BOBDEV	signed_curve25519	BOBDEVFB	{"key":"k8my8h2KDF2wjcHjLbc1Zf2a/zqLHV8plkl41bMD6HI","signatures":{"@bob:fixture.local":{"ed25519:BOBDEV":"HKPA01aqV8bIejHb5bxK7HmU6+4esWnuDApkszFMxNY4SX+BXF0KSNyw28W7sjbTkTaCkVU01ROcR88/Huf1DA"}},"fallback":true}	f
\.


--
-- Data for Name: e2e_one_time_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_one_time_keys_json (user_id, device_id, algorithm, key_id, ts_added_ms, key_json) FROM stdin;
@alice:fixture.local	ALICEPHONE	signed_curve25519	ALICEPHONEOTK0	1795198104419	{"key":"vv9NQGQOg/f/om8tWeW55xW4q6AdWTHbpr5RxQEWnHg","signatures":{"@alice:fixture.local":{"ed25519:ALICEPHONE":"kqX4IyN2DYRAaDFJ7Era2sp7q2/RSjbsPPqu4l7w8CI2y3a2QXbwOj8a7zuaadPnwvW/P3THMjnM0+rGURDYCA"}}}
@alice:fixture.local	ALICEPHONE	signed_curve25519	ALICEPHONEOTK1	1795198104419	{"key":"Kr4M31Iio0bHVNlCiTYWydGeAgMKv2pr9gQsyXWH0w0","signatures":{"@alice:fixture.local":{"ed25519:ALICEPHONE":"6AkRX22sFBVRh728nVEDuymtNblqcV83ZHb0Of0+D3lqGnwklLheygWwzakHU9d1/ER2yblDERFlLDVcSxxoAQ"}}}
@alice:fixture.local	ALICEPHONE	signed_curve25519	ALICEPHONEOTK2	1795198104419	{"key":"M9gHX1qx7OoRp7bycg9cduFX3IC0F3rlKqh+Mb5fgh8","signatures":{"@alice:fixture.local":{"ed25519:ALICEPHONE":"s5k/RTRDFdv2h/99cmVmqPwR3TcQ7gfQ7AkzWhosoUa6uvjWmvOLpKe7XhlZcU4FP++aib5X+343VoDCym31Dg"}}}
@alice:fixture.local	ALICELAPTOP	signed_curve25519	ALICELAPTOPOTK1	1795198104580	{"key":"fVfmOuCj4szHz0AJHXp+93mDYZZBts3oGARGwzhyQFs","signatures":{"@alice:fixture.local":{"ed25519:ALICELAPTOP":"5WQJ8GffUYzDfHpUCqlhtzIQ1UN5zBKOR1/IV8NFXwTVcRBowJtXVeY58oYWjwJl3ozGtKoNNpAvB5oscB9LBQ"}}}
@alice:fixture.local	ALICELAPTOP	signed_curve25519	ALICELAPTOPOTK2	1795198104580	{"key":"T5oe5x0AYqTmU3Hh4p9DDw1RV8oMkFHeLN1VBF3SRhs","signatures":{"@alice:fixture.local":{"ed25519:ALICELAPTOP":"xbh78Bo/7FtHPhQ8MJQKyR0Tf5VSj9j+o//6JWvpmoiPcEcKhCK5YRpOuhowJiYooFtUz3SftLlkNqK3a2niBA"}}}
@bob:fixture.local	BOBDEV	signed_curve25519	BOBDEVOTK0	1795198104778	{"key":"MiE4q5jiHOYzxatRYM1Vx38WtsInWvHYxPY3h9IBElo","signatures":{"@bob:fixture.local":{"ed25519:BOBDEV":"1eGG5id1R08szLJOnicr5NdVjGLgg2ikI/pNPzlFGZapA9GDtQAQIPwX6HoCdj3MiymD9kwU2mNZdARQwNqSDw"}}}
@bob:fixture.local	BOBDEV	signed_curve25519	BOBDEVOTK1	1795198104778	{"key":"/YcokNSh4o8eUORCaTUtiHX2bRCmhypmF0vInSlp4Ho","signatures":{"@bob:fixture.local":{"ed25519:BOBDEV":"FYXSGqAtgrJ2c+s5adyjSb7Bjd4Qs+PrCoC3F6wK00fBlMG7RzPPrRFuZ7dIhyN09wvf7e/yAkg/Il5pYHYDDQ"}}}
@bob:fixture.local	BOBDEV	signed_curve25519	BOBDEVOTK2	1795198104778	{"key":"XeBYveJifrlTvw7nEVXroGMnEJ392KTmcTQewRfFbyU","signatures":{"@bob:fixture.local":{"ed25519:BOBDEV":"0BC4CU5izXJ/F1Kc8Q+isg5v8xC0+j6244lobHrO+mgttlXBEXHA3GxzQre0A7M/R2tHK8j4nbcsyV/ygoFFAw"}}}
\.


--
-- Data for Name: e2e_room_keys; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_room_keys (user_id, room_id, session_id, version, first_message_index, forwarded_count, is_verified, session_data) FROM stdin;
@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	session0	2	0	0	t	{"ephemeral":"eph0","ciphertext":"ct0","mac":"mac0"}
@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	session1	2	1	0	f	{"ephemeral":"eph1","ciphertext":"ct1","mac":"mac1"}
\.


--
-- Data for Name: e2e_room_keys_versions; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_room_keys_versions (user_id, version, algorithm, auth_data, deleted, etag) FROM stdin;
@alice:fixture.local	1	m.megolm_backup.v1.curve25519-aes-sha2	{"public_key":"q9OYwQi1BSDwkqjcLFrE+Yn8UhjmUvSrMBZqKkAn2m0"}	1	\N
@alice:fixture.local	2	m.megolm_backup.v1.curve25519-aes-sha2	{"public_key":"q9OYwQi1BSDwkqjcLFrE+Yn8UhjmUvSrMBZqKkAn2m0"}	0	1
\.


--
-- Data for Name: event_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.event_json (event_id, room_id, internal_metadata, json, format_version) FROM stdin;
$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"zo2DiWMjDPzorsSQxrWHLefeK/R75SDK+/VPcT0ODARYYNQjqhkz5t+OQSq06hlMV3jzfc6Vmtyo3oqN8Kj1Cw"}},"unsigned":{"age_ts":1795198106093},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":[],"prev_events":[],"content":{"creator":"@alice:fixture.local","room_version":"10"},"depth":1,"hashes":{"sha256":"dLjuuS4nYHCcMi1MHZJz7myvz6iF7cV13gJGKp4VB4E"},"origin_server_ts":1795198106093,"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"}	3
$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"aV4ZmV19UUub9p8hXLPPfdmnBbHuTk+xArZAOKyQxB8v3DiRGB06MiYZqPZ27VvBo7RwW1z+vn+DFIX0iVosBQ"}},"unsigned":{"age_ts":1795198106486},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns"],"prev_events":["$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns"],"content":{"displayname":"alice","membership":"join"},"depth":2,"hashes":{"sha256":"HelknsYHOLzrSuYPlTBem7iviv5tR+62vAobkN2LBmE"},"origin_server_ts":1795198106486,"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}	3
$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"fe0FAoNCcgJp0E8KwDnbfOzjs2CDFZx8GAl9Kkltu/7+AS8E7F9oY7Acm41f8Fd7+buR5a6OMfH/WnB4xkpGAg"}},"unsigned":{"age_ts":1795198106842},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns"],"prev_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":100},"events_default":0,"historical":100,"invite":0,"kick":50,"redact":50,"state_default":50,"users":{"@alice:fixture.local":100},"users_default":0},"depth":3,"hashes":{"sha256":"o3OCfHQxnc3ShokdUiW1RC42qqZvDIRybEnPJAM2Rpw"},"origin_server_ts":1795198106842,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	3
$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"ScNCRNDQRSfkc3B6bisnPg/PXnwBA22qPZJ6t/pZijXIa0AEhCx7otpuG8bu8dqTCvemD6Y+3qaw5ycp/SmvAQ"}},"unsigned":{"age_ts":1795198106877},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"content":{"join_rule":"invite"},"depth":4,"hashes":{"sha256":"eu5nP9vPlH27pMekrbmOA0T7TL1kwdSlmNnr6HA7+J0"},"origin_server_ts":1795198106877,"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"}	3
$5xNHyr6rp5vVrccY1pwEs8Ubdi9BSScWsToH499gmcg	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"8aXl7Yap7P9M1XQ7DP9teAKRRhiQrt0MOUf+O1em9otZUTgekNnBW4aQGU03SDJRdzl9JNRhvW4SPIv5m7zcDg"}},"unsigned":{"age_ts":1795198106878},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY"],"content":{"history_visibility":"shared"},"depth":5,"hashes":{"sha256":"c5Tr05uR49BN3Z6UEt2+jqkeBswBilaFl0gZSZbbck8"},"origin_server_ts":1795198106878,"sender":"@alice:fixture.local","state_key":"","type":"m.room.history_visibility"}	3
$FjvyrVoMuOz75mavO0X1uUWlGVP6Yb13-8LD7Ld1KQE	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"8tZ1/kO4jXinxdWwIMLH4CHa1MFTwiBmUgrKSbRLn97Bgowko2W1gVWKvIa58firiLxs3+Q/RXwTn9XwbNf3AQ"}},"unsigned":{"age_ts":1795198106880},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$5xNHyr6rp5vVrccY1pwEs8Ubdi9BSScWsToH499gmcg"],"content":{"guest_access":"can_join"},"depth":6,"hashes":{"sha256":"7plxTqQo2mM1hDNokF/pe+Q8/HZOsXx4nglJ0ti5Ufg"},"origin_server_ts":1795198106880,"sender":"@alice:fixture.local","state_key":"","type":"m.room.guest_access"}	3
$OAMX21jwEnQIPtFWMsUPHjxOz3OQnoYUY-okNzolKQI	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"6WFG00qtKIF+zhOmpcOye10CpuK6NEqf9HcU8S8LtisaUL77OyxTRW3Msa2icEZOexyfvb/4NTs+Oz8APGdmDw"}},"unsigned":{"age_ts":1795198106881},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$FjvyrVoMuOz75mavO0X1uUWlGVP6Yb13-8LD7Ld1KQE"],"content":{"name":"Fixture"},"depth":7,"hashes":{"sha256":"63dTNNclcyA0TXtySmE/Be2fXBKL3b1BardNFlxqAEY"},"origin_server_ts":1795198106881,"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"}	3
$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"mtK4O6ZVR3u7YZIuEBegefau5zI9Rjb3tWAJ7DqZkU54lBLSMJJQcQ9KoONlkstxVlPCiS+/O9rsTpn5PxcrBg"}},"unsigned":{"age_ts":1795198107287,"invite_room_state":[{"content":{"creator":"@alice:fixture.local","room_version":"10"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"name":"Fixture"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}]},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ","$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY","$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns"],"prev_events":["$OAMX21jwEnQIPtFWMsUPHjxOz3OQnoYUY-okNzolKQI"],"content":{"displayname":"bob","membership":"invite"},"depth":8,"hashes":{"sha256":"PDXXZSqwK2W3WSi/Yr8NP2XS1UTT2eolwVqlfUV4LwI"},"origin_server_ts":1795198107287,"sender":"@alice:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	3
$SDGEe_zhK6gDq0yPo3YbEyMC_yMNhROnPRIJyhMUWKU	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"BOBDEV"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"xg+kAvvs2SN0R2RIdwO1pIdklslgSUNEZ+pjJYQKaYpsagcs9XKhsQptpicBSD3KUNxh0/R3coOcEo04cWfiBQ"}},"unsigned":{"age_ts":1795198107883,"replaces_state":"$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc"},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc"],"content":{"displayname":"bob","membership":"join"},"depth":9,"hashes":{"sha256":"MvG86Yphd27Qe7VZoM8wDYd6OrALH5iPYSxTbgN642s"},"origin_server_ts":1795198107883,"sender":"@bob:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	3
$r0ppiGob5TcQ7VY8tC1JPGsRa3bSv9p-yZjhX0e4KKo	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP","txn_id":"m1"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"OE4tpQdcsPDgRreFkXH4rnuVAgeUT7vvqiQgjLjDwJwGxzHMYWTZMH7gCBojlGxjiRfkSV7Pt+fBUNNfDoVvBw"}},"unsigned":{"age_ts":1795198108410},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$SDGEe_zhK6gDq0yPo3YbEyMC_yMNhROnPRIJyhMUWKU"],"content":{"body":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430","msgtype":"m.text"},"depth":10,"hashes":{"sha256":"A+9x2jzL3JWlRmh0GMFOFoLJ53AOI7mMnALY/pHm0R8"},"origin_server_ts":1795198108410,"sender":"@alice:fixture.local","type":"m.room.message"}	3
$IhuNcFi1ElnfAndTL6eW10u8D4QWtQITxBBRDEol7WY	!UqZyOvgBsnlvTQIuwk:fixture.local	{"device_id":"ALICELAPTOP","txn_id":"m2"}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"EvUx+wS1tKGXrd04JGanALwDdTrGxIpC7V0lWBnPXO+grgks9IwVuJiRhXfWJ3ApkvZ4ha26fYG1Mjc0IdPfCA"}},"unsigned":{"age_ts":1795198108772},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$r0ppiGob5TcQ7VY8tC1JPGsRa3bSv9p-yZjhX0e4KKo"],"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"Y2lwaGVy","device_id":"ALICELAPTOP","sender_key":"c2s","session_id":"session0"},"depth":11,"hashes":{"sha256":"aIRQA/vn7hNH2WYnNINDrajPwRSsabgCIOK+QI8pyao"},"origin_server_ts":1795198108772,"sender":"@alice:fixture.local","type":"m.room.encrypted"}	3
$OwKWDldjKxUecka68PgBcRZNYwTwCG6gz_fSqy1u1g8	!UqZyOvgBsnlvTQIuwk:fixture.local	{}	{"signatures":{"fixture.local":{"ed25519:a_NIKP":"p5AwsK0LPvtStvPhR2ridxTjaTDEr742Zn+E0+x47qacB1ezaphRiBxZxIX6RYiOPlvj4H3gB7R6siQWaCMXAg"}},"unsigned":{"age_ts":1795198110779,"replaces_state":"$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw"},"room_id":"!UqZyOvgBsnlvTQIuwk:fixture.local","auth_events":["$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY","$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw","$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns","$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ"],"prev_events":["$IhuNcFi1ElnfAndTL6eW10u8D4QWtQITxBBRDEol7WY"],"content":{"avatar_url":"mxc://fixture.local/avatar1","displayname":"\\u0410\\u043b\\u0438\\u0441\\u0430","membership":"join"},"depth":12,"hashes":{"sha256":"379xdAdEl5WKn9qY1tSf9sVPUwH2QjbQ4ks5yYO6Y24"},"origin_server_ts":1795198110779,"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}	3
\.


--
-- Data for Name: events; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.events (topological_ordering, event_id, type, room_id, content, unrecognized_keys, processed, outlier, depth, origin_server_ts, received_ts, sender, contains_url, instance_name, stream_ordering, state_key, rejection_reason) FROM stdin;
1	$53ImxifOwugUSMclnSz7kPrKSIMcVbf0pAzCF5Ji1Ns	m.room.create	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	1	1795198106093	1795198106264	@alice:fixture.local	f	master	2		\N
2	$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw	m.room.member	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	2	1795198106486	1795198106588	@alice:fixture.local	f	master	3	@alice:fixture.local	\N
3	$I2lqJOqu6HO8sCdMP9hizDnvAK30-sawu9XPFAazMaQ	m.room.power_levels	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	3	1795198106842	1795198107016	@alice:fixture.local	f	master	4		\N
4	$fUSFHthnFKc5y6vLgxbZUIOkKeKGs1ykJ9-fey9eAPY	m.room.join_rules	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	4	1795198106877	1795198107016	@alice:fixture.local	f	master	5		\N
5	$5xNHyr6rp5vVrccY1pwEs8Ubdi9BSScWsToH499gmcg	m.room.history_visibility	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	5	1795198106878	1795198107016	@alice:fixture.local	f	master	6		\N
6	$FjvyrVoMuOz75mavO0X1uUWlGVP6Yb13-8LD7Ld1KQE	m.room.guest_access	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	6	1795198106880	1795198107016	@alice:fixture.local	f	master	7		\N
7	$OAMX21jwEnQIPtFWMsUPHjxOz3OQnoYUY-okNzolKQI	m.room.name	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	7	1795198106881	1795198107016	@alice:fixture.local	f	master	8		\N
8	$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc	m.room.member	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	8	1795198107287	1795198107567	@alice:fixture.local	f	master	9	@bob:fixture.local	\N
9	$SDGEe_zhK6gDq0yPo3YbEyMC_yMNhROnPRIJyhMUWKU	m.room.member	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	9	1795198107883	1795198107972	@bob:fixture.local	f	master	10	@bob:fixture.local	\N
10	$r0ppiGob5TcQ7VY8tC1JPGsRa3bSv9p-yZjhX0e4KKo	m.room.message	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	10	1795198108410	1795198108566	@alice:fixture.local	f	master	11	\N	\N
11	$IhuNcFi1ElnfAndTL6eW10u8D4QWtQITxBBRDEol7WY	m.room.encrypted	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	11	1795198108772	1795198108841	@alice:fixture.local	f	master	12	\N	\N
12	$OwKWDldjKxUecka68PgBcRZNYwTwCG6gz_fSqy1u1g8	m.room.member	!UqZyOvgBsnlvTQIuwk:fixture.local	\N	\N	t	f	12	1795198110779	1795198110855	@alice:fixture.local	f	master	13	@alice:fixture.local	\N
\.


--
-- Data for Name: local_media_repository; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) FROM stdin;
\.


--
-- Data for Name: profiles; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.profiles (user_id, displayname, avatar_url, full_user_id, fields) FROM stdin;
bob	bob	\N	@bob:fixture.local	\N
carol	carol	\N	@carol:fixture.local	\N
dave	dave	\N	@dave:fixture.local	\N
eve	eve	\N	@eve:fixture.local	\N
frank	frank	\N	@frank:fixture.local	\N
ldapuser	LDAP User	\N	@ldapuser:fixture.local	\N
alice	Алиса	mxc://fixture.local/avatar1	@alice:fixture.local	\N
\.


--
-- Data for Name: push_rules; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.push_rules (id, user_name, rule_id, priority_class, priority, conditions, actions) FROM stdin;
2	@alice:fixture.local	global/override/second	5	0	[{"kind":"event_match","key":"type","pattern":"m.second"}]	[]
3	@alice:fixture.local	global/override/first	5	1	[{"kind":"event_match","key":"type","pattern":"m.first"}]	["notify"]
4	@alice:fixture.local	global/content/coffee	4	0	[{"kind":"event_match","key":"content.body","pattern":"coffee"}]	["notify",{"set_tweak":"highlight"}]
5	@alice:fixture.local	global/room/!UqZyOvgBsnlvTQIuwk:fixture.local	3	0	[{"kind":"event_match","key":"room_id","pattern":"!UqZyOvgBsnlvTQIuwk:fixture.local"}]	[]
6	@alice:fixture.local	global/underride/.m.rule.message	-1	1	[]	[]
\.


--
-- Data for Name: push_rules_enable; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.push_rules_enable (id, user_name, rule_id, enabled) FROM stdin;
2	@alice:fixture.local	global/override/second	1
4	@alice:fixture.local	global/content/coffee	1
5	@alice:fixture.local	global/room/!UqZyOvgBsnlvTQIuwk:fixture.local	1
6	@alice:fixture.local	global/override/.m.rule.invite_for_me	0
7	@alice:fixture.local	global/underride/.m.rule.message	1
3	@alice:fixture.local	global/override/first	0
\.


--
-- Data for Name: pushers; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.pushers (id, user_name, access_token, profile_tag, kind, app_id, app_display_name, device_display_name, pushkey, ts, lang, data, last_stream_ordering, last_success, failing_since, enabled, device_id, instance_name) FROM stdin;
3	@alice:fixture.local	\N		http	im.vector.app.android	Element	Alice laptop	pushkey-alice-1	1795198110346	ru	{"url":"https://push.example.org/_matrix/push/v1/notify","format":"event_id_only"}	12	\N	\N	t	ALICELAPTOP	master
\.


--
-- Data for Name: refresh_tokens; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.refresh_tokens (id, user_id, device_id, token, next_token_id, expiry_ts, ultimate_session_expiry_ts) FROM stdin;
2	@alice:fixture.local	ALICEPHONE	syr_YWxpY2U_OpnvTcAmzcCYukFCroHr_0ShXtR	\N	\N	\N
\.


--
-- Data for Name: rejections; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.rejections (event_id, reason, last_check) FROM stdin;
\.


--
-- Data for Name: room_account_data; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_account_data (user_id, room_id, account_data_type, stream_id, content, instance_name) FROM stdin;
@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	org.example.room.note	8	{"note":"\\u0437\\u0430\\u043c\\u0435\\u0442\\u043a\\u0430"}	\N
\.


--
-- Data for Name: room_aliases; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_aliases (room_alias, room_id, creator) FROM stdin;
#fixture:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	@alice:fixture.local
\.


--
-- Data for Name: room_memberships; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_memberships (event_id, user_id, sender, room_id, membership, forgotten, display_name, avatar_url, event_stream_ordering, participant) FROM stdin;
$c_XEj7vQ-mzhgRXplmnBcrBzrRb2dwEEJmzL9xyYPUc	@bob:fixture.local	@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	invite	0	bob	\N	9	f
$SDGEe_zhK6gDq0yPo3YbEyMC_yMNhROnPRIJyhMUWKU	@bob:fixture.local	@bob:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	join	0	bob	\N	10	f
$cfJbS1jeg7na7n6NrX5QU4vvIRji46hBHDvwBic7sfw	@alice:fixture.local	@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	join	0	alice	\N	3	t
$OwKWDldjKxUecka68PgBcRZNYwTwCG6gz_fSqy1u1g8	@alice:fixture.local	@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	join	0	Алиса	mxc://fixture.local/avatar1	13	f
\.


--
-- Data for Name: room_tags; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_tags (user_id, room_id, tag, content) FROM stdin;
@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	m.favourite	{"order":0.5}
@alice:fixture.local	!UqZyOvgBsnlvTQIuwk:fixture.local	u.work	{}
\.


--
-- Data for Name: rooms; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.rooms (room_id, is_public, creator, room_version, has_auth_chain_index) FROM stdin;
!UqZyOvgBsnlvTQIuwk:fixture.local	f	@alice:fixture.local	10	t
\.


--
-- Data for Name: user_filters; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.user_filters (user_id, filter_id, filter_json, full_user_id) FROM stdin;
alice	0	\\x7b22726f6f6d223a7b2274696d656c696e65223a7b226c696d6974223a377d7d7d	@alice:fixture.local
\.


--
-- Data for Name: users; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.users (name, password_hash, creation_ts, admin, upgrade_ts, is_guest, appservice_id, consent_version, consent_server_notice_sent, user_type, deactivated, shadow_banned, consent_ts, approved, locked, suspended) FROM stdin;
@alice:fixture.local	$2b$12$LNMq7mW4MFWIFoy0kLKX6eci/PX2RzJtGBy8nCRyxrjG4F7DClciO	1795198095	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@bob:fixture.local	$2b$12$IG5WZjF/lC1OCEX4TEzBqeQLOJDUYYAJBYisrjOz2H..HxGVUKB7y	1795198096	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@carol:fixture.local	$2b$12$TR1avybePp2Or3SvRqyaJunhGIBKI6DKFbgpEQQbOVqHjaT5QZo7C	1795198097	1	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@dave:fixture.local	\N	1795198097	0	\N	0	\N	\N	\N	\N	1	f	\N	t	f	f
@eve:fixture.local	$2b$12$kbJphQihPaLCM2.wyVI32OnaiEYNCrI3RlAkVTSXTHapuI.iGdzlW	1795198098	0	\N	0	\N	\N	\N	\N	0	f	\N	t	t	f
@frank:fixture.local	$2b$12$F1kNnRzgy6fG7u98YfnHQuvAtCOIS9ex58hLwvQlkpkyb5.2Ookhu	1795198099	0	\N	0	\N	\N	\N	\N	0	t	\N	t	f	f
@ldapuser:fixture.local	\N	1795198101	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
\.


--
-- Name: access_tokens access_tokens_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.access_tokens
    ADD CONSTRAINT access_tokens_pkey PRIMARY KEY (id);


--
-- Name: access_tokens access_tokens_token_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.access_tokens
    ADD CONSTRAINT access_tokens_token_key UNIQUE (token);


--
-- Name: account_data account_data_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.account_data
    ADD CONSTRAINT account_data_uniqueness UNIQUE (user_id, account_data_type);


--
-- Name: current_state_events current_state_events_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.current_state_events
    ADD CONSTRAINT current_state_events_event_id_key UNIQUE (event_id);


--
-- Name: current_state_events current_state_events_room_id_type_state_key_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.current_state_events
    ADD CONSTRAINT current_state_events_room_id_type_state_key_key UNIQUE (room_id, type, state_key);


--
-- Name: devices device_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.devices
    ADD CONSTRAINT device_uniqueness UNIQUE (user_id, device_id);


--
-- Name: e2e_device_keys_json e2e_device_keys_json_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.e2e_device_keys_json
    ADD CONSTRAINT e2e_device_keys_json_uniqueness UNIQUE (user_id, device_id);


--
-- Name: e2e_fallback_keys_json e2e_fallback_keys_json_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.e2e_fallback_keys_json
    ADD CONSTRAINT e2e_fallback_keys_json_uniqueness UNIQUE (user_id, device_id, algorithm);


--
-- Name: e2e_one_time_keys_json e2e_one_time_keys_json_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.e2e_one_time_keys_json
    ADD CONSTRAINT e2e_one_time_keys_json_uniqueness UNIQUE (user_id, device_id, algorithm, key_id);


--
-- Name: event_json event_json_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.event_json
    ADD CONSTRAINT event_json_event_id_key UNIQUE (event_id);


--
-- Name: events events_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.events
    ADD CONSTRAINT events_event_id_key UNIQUE (event_id);


--
-- Name: profiles full_user_id_not_null; Type: CHECK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE synapse.profiles
    ADD CONSTRAINT full_user_id_not_null CHECK ((full_user_id IS NOT NULL)) NOT VALID;


--
-- Name: user_filters full_user_id_not_null; Type: CHECK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE synapse.user_filters
    ADD CONSTRAINT full_user_id_not_null CHECK ((full_user_id IS NOT NULL)) NOT VALID;


--
-- Name: local_media_repository local_media_repository_media_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.local_media_repository
    ADD CONSTRAINT local_media_repository_media_id_key UNIQUE (media_id);


--
-- Name: profiles profiles_user_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.profiles
    ADD CONSTRAINT profiles_user_id_key UNIQUE (user_id);


--
-- Name: push_rules_enable push_rules_enable_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.push_rules_enable
    ADD CONSTRAINT push_rules_enable_pkey PRIMARY KEY (id);


--
-- Name: push_rules_enable push_rules_enable_user_name_rule_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.push_rules_enable
    ADD CONSTRAINT push_rules_enable_user_name_rule_id_key UNIQUE (user_name, rule_id);


--
-- Name: push_rules push_rules_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.push_rules
    ADD CONSTRAINT push_rules_pkey PRIMARY KEY (id);


--
-- Name: push_rules push_rules_user_name_rule_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.push_rules
    ADD CONSTRAINT push_rules_user_name_rule_id_key UNIQUE (user_name, rule_id);


--
-- Name: pushers pushers2_app_id_pushkey_user_name_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.pushers
    ADD CONSTRAINT pushers2_app_id_pushkey_user_name_key UNIQUE (app_id, pushkey, user_name);


--
-- Name: pushers pushers2_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.pushers
    ADD CONSTRAINT pushers2_pkey PRIMARY KEY (id);


--
-- Name: refresh_tokens refresh_tokens_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.refresh_tokens
    ADD CONSTRAINT refresh_tokens_pkey PRIMARY KEY (id);


--
-- Name: refresh_tokens refresh_tokens_token_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.refresh_tokens
    ADD CONSTRAINT refresh_tokens_token_key UNIQUE (token);


--
-- Name: rejections rejections_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.rejections
    ADD CONSTRAINT rejections_event_id_key UNIQUE (event_id);


--
-- Name: room_account_data room_account_data_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.room_account_data
    ADD CONSTRAINT room_account_data_uniqueness UNIQUE (user_id, room_id, account_data_type);


--
-- Name: room_aliases room_aliases_room_alias_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.room_aliases
    ADD CONSTRAINT room_aliases_room_alias_key UNIQUE (room_alias);


--
-- Name: room_memberships room_memberships_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.room_memberships
    ADD CONSTRAINT room_memberships_event_id_key UNIQUE (event_id);


--
-- Name: room_tags room_tag_uniqueness; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.room_tags
    ADD CONSTRAINT room_tag_uniqueness UNIQUE (user_id, room_id, tag);


--
-- Name: rooms rooms_pkey; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.rooms
    ADD CONSTRAINT rooms_pkey PRIMARY KEY (room_id);


--
-- Name: users users_name_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.users
    ADD CONSTRAINT users_name_key UNIQUE (name);


--
-- Name: access_tokens_device_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX access_tokens_device_id ON synapse.access_tokens USING btree (user_id, device_id);


--
-- Name: account_data_stream_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX account_data_stream_id ON synapse.account_data USING btree (user_id, stream_id);


--
-- Name: current_state_events_member_index; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX current_state_events_member_index ON synapse.current_state_events USING btree (state_key) WHERE (type = 'm.room.member'::text);


--
-- Name: device_inbox_stream_id_user_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX device_inbox_stream_id_user_id ON synapse.device_inbox USING btree (stream_id, user_id);


--
-- Name: device_inbox_user_stream_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX device_inbox_user_stream_id ON synapse.device_inbox USING btree (user_id, device_id, stream_id);


--
-- Name: e2e_cross_signing_keys_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX e2e_cross_signing_keys_idx ON synapse.e2e_cross_signing_keys USING btree (user_id, keytype, stream_id);


--
-- Name: e2e_cross_signing_keys_stream_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX e2e_cross_signing_keys_stream_idx ON synapse.e2e_cross_signing_keys USING btree (stream_id);


--
-- Name: e2e_cross_signing_signatures2_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX e2e_cross_signing_signatures2_idx ON synapse.e2e_cross_signing_signatures USING btree (user_id, target_user_id, target_device_id);


--
-- Name: e2e_room_keys_versions_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX e2e_room_keys_versions_idx ON synapse.e2e_room_keys_versions USING btree (user_id, version);


--
-- Name: e2e_room_keys_with_version_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX e2e_room_keys_with_version_idx ON synapse.e2e_room_keys USING btree (user_id, version, room_id, session_id);


--
-- Name: event_contains_url_index; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX event_contains_url_index ON synapse.events USING btree (room_id, topological_ordering, stream_ordering) WHERE ((contains_url = true) AND (outlier = false));


--
-- Name: events_order_room; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX events_order_room ON synapse.events USING btree (room_id, topological_ordering, stream_ordering);


--
-- Name: events_room_stream; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX events_room_stream ON synapse.events USING btree (room_id, stream_ordering);


--
-- Name: events_stream_ordering; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX events_stream_ordering ON synapse.events USING btree (stream_ordering);


--
-- Name: events_ts; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX events_ts ON synapse.events USING btree (origin_server_ts, stream_ordering);


--
-- Name: local_media_repository_url_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX local_media_repository_url_idx ON synapse.local_media_repository USING btree (created_ts) WHERE (url_cache IS NOT NULL);


--
-- Name: public_room_index; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX public_room_index ON synapse.rooms USING btree (is_public);


--
-- Name: push_rules_enable_user_name; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX push_rules_enable_user_name ON synapse.push_rules_enable USING btree (user_name);


--
-- Name: push_rules_user_name; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX push_rules_user_name ON synapse.push_rules USING btree (user_name);


--
-- Name: refresh_tokens_next_token_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX refresh_tokens_next_token_id ON synapse.refresh_tokens USING btree (next_token_id) WHERE (next_token_id IS NOT NULL);


--
-- Name: room_account_data_stream_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_account_data_stream_id ON synapse.room_account_data USING btree (user_id, stream_id);


--
-- Name: room_aliases_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_aliases_id ON synapse.room_aliases USING btree (room_id);


--
-- Name: room_memberships_room_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_memberships_room_id ON synapse.room_memberships USING btree (room_id);


--
-- Name: room_memberships_user_id; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_memberships_user_id ON synapse.room_memberships USING btree (user_id);


--
-- Name: room_memberships_user_room_forgotten; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_memberships_user_room_forgotten ON synapse.room_memberships USING btree (user_id, room_id) WHERE (forgotten = 1);


--
-- Name: user_filters_unique; Type: INDEX; Schema: synapse; Owner: -
--

CREATE UNIQUE INDEX user_filters_unique ON synapse.user_filters USING btree (user_id, filter_id);


--
-- Name: users_creation_ts; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX users_creation_ts ON synapse.users USING btree (creation_ts);


--
-- Name: users_have_local_media; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX users_have_local_media ON synapse.local_media_repository USING btree (user_id, created_ts);


--
-- Name: current_state_events check_event_stream_ordering; Type: TRIGGER; Schema: synapse; Owner: -
--



--
-- Name: room_memberships check_event_stream_ordering; Type: TRIGGER; Schema: synapse; Owner: -
--



--
-- Name: access_tokens access_tokens_refresh_token_id_fkey; Type: FK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.access_tokens
    ADD CONSTRAINT access_tokens_refresh_token_id_fkey FOREIGN KEY (refresh_token_id) REFERENCES synapse.refresh_tokens(id) ON DELETE CASCADE;


--
-- Name: current_state_events event_stream_ordering_fkey; Type: FK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.current_state_events
    ADD CONSTRAINT event_stream_ordering_fkey FOREIGN KEY (event_stream_ordering) REFERENCES synapse.events(stream_ordering) NOT VALID;


--
-- Name: room_memberships event_stream_ordering_fkey; Type: FK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.room_memberships
    ADD CONSTRAINT event_stream_ordering_fkey FOREIGN KEY (event_stream_ordering) REFERENCES synapse.events(stream_ordering) NOT VALID;


--
-- Name: refresh_tokens refresh_tokens_next_token_id_fkey; Type: FK CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.refresh_tokens
    ADD CONSTRAINT refresh_tokens_next_token_id_fkey FOREIGN KEY (next_token_id) REFERENCES synapse.refresh_tokens(id) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--


