-- Фикстура (v12): таблицы живого Synapse v1.165.0.
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
-- Name: redactions; Type: TABLE; Schema: synapse; Owner: -
--

CREATE TABLE synapse.redactions (
    event_id text NOT NULL,
    redacts text NOT NULL,
    have_censored boolean DEFAULT false NOT NULL,
    received_ts bigint,
    recheck boolean DEFAULT true NOT NULL
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
2	@alice:fixture.local	WOWPJLSBKX	syt_YWxpY2U_oPQViFTjCeROdtCxFCUU_0YaQKo	\N	\N	1795207459445	\N	f
3	@bob:fixture.local	CULTEZMBKW	syt_Ym9i_riHHMBMQkLksvVBcXaNB_37V6oi	\N	\N	1795207460175	\N	f
4	@carol:fixture.local	MDKROGNYHR	syt_Y2Fyb2w_jvmgdyljSUVagsqomXSg_1nTGmj	\N	\N	1795207460878	\N	f
5	@dave:fixture.local	CWXHEIDRFU	syt_ZGF2ZQ_ngvlHAhkVSsbkCjaHEsj_1mN9QE	\N	\N	1795207461687	\N	f
9	@dave:fixture.local	DAVEDEV	syt_ZGF2ZQ_XeyHPkiEDHluEkNwbsKA_0mRx3c	\N	\N	1795207464785	\N	f
6	@alice:fixture.local	ALICEDEV	syt_YWxpY2U_zQpeiXXZbMEFaQEKSxiQ_1akazL	\N	\N	1795207462481	\N	t
7	@bob:fixture.local	BOBDEV	syt_Ym9i_YMtWGDmDLKpTprgcYUlw_4e1HI6	\N	\N	1795207463110	\N	t
8	@carol:fixture.local	CAROLDEV	syt_Y2Fyb2w_XuCKSSxzVJlgubjuIZuL_244Ni9	\N	\N	1795207463711	\N	t
\.


--
-- Data for Name: account_data; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.account_data (user_id, account_data_type, stream_id, content, instance_name) FROM stdin;
\.


--
-- Data for Name: current_state_events; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.current_state_events (event_id, room_id, type, state_key, membership, event_stream_ordering) FROM stdin;
$k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.create		\N	2
$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.member	@alice:fixture.local	join	3
$-gch8UEWgJJBGUBv9oQh7oGzzwjIAEX2gQmS221AYrM	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.guest_access		\N	7
$F1y8uIj5FgVBf3PjTbk3HE6a6HWIbgcphEyODucCuCY	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.history_visibility		\N	6
$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.join_rules		\N	5
$NM4fHJAwtOtVzoRQjpWUB22LqWnLsPXJ0iU5CB3eRr8	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.name		\N	8
$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.member	@bob:fixture.local	join	12
$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.member	@carol:fixture.local	join	13
$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.power_levels		\N	16
$8733C3VE4Yuihm9GWz7HX2r4HnxpJTlY58UQ-r_9P98	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.topic		\N	17
$v0ISLQuPaNYS4pxp2Cr_FmFHwRUpKWXq42cbH6KOs5A	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.member	@dave:fixture.local	invite	21
$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.create		\N	22
$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.member	@alice:fixture.local	join	23
$hGc-bVHtxOWQq2kyjkHViwVwaeomeXD6gNY1YG7Ru94	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.guest_access		\N	27
$qdaSMhHXnY8_oHfU5nup32bJkeXRRKOAtJ7fOtFkuf4	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.history_visibility		\N	26
$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.join_rules		\N	25
$TdvUnpfyIvJd25aJQuZ_9dxFEM3OuRnyQaPzkpR-0U4	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.name		\N	28
$DeHs04kBmQ6v5qqrgG6-PZYCsEJGcTwh0GkZ6t6UlNg	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.member	@bob:fixture.local	join	30
$gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.create		\N	32
$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.member	@alice:fixture.local	join	33
$V3SIVCFsx3-yRQYj9e8bhwP1krSKy3WO-SyoMnLaFi4	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.guest_access		\N	35
$Baj13SXAdqwSBrUIs4mo97J9Pq-zXZeNUxA8lmVYT5I	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.history_visibility		\N	36
$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.join_rules		\N	37
$EKwILfSMo2TKPtnbO7darzKPbf16GbPgfWrmLoKkHg8	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.name		\N	38
$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.power_levels		\N	34
$xiDfJR_-0rsK7Mz3X3nRcEQNZryjLFcyLZDG0HBByaQ	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.tombstone		\N	39
$XrnebK1kmZiWWQUdsjTqCXXWpNpF6qT71nt0PKp1ZXM	!TmPykHDOnIszcdrgRJ:fixture.local	m.room.power_levels		\N	40
$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.member	@bob:fixture.local	join	42
\.


--
-- Data for Name: device_inbox; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.device_inbox (user_id, device_id, stream_id, message_json, instance_name) FROM stdin;
\.


--
-- Data for Name: devices; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.devices (user_id, device_id, display_name, last_seen, ip, user_agent, hidden) FROM stdin;
@alice:fixture.local	WOWPJLSBKX	\N	\N	\N	\N	f
@bob:fixture.local	CULTEZMBKW	\N	\N	\N	\N	f
@carol:fixture.local	MDKROGNYHR	\N	\N	\N	\N	f
@dave:fixture.local	CWXHEIDRFU	\N	\N	\N	\N	f
@dave:fixture.local	DAVEDEV	Dave desktop	\N	\N	\N	f
@alice:fixture.local	ALICEDEV	Alice desktop	1795207465030	127.0.0.1	Python-urllib/3.13	f
@bob:fixture.local	BOBDEV	Bob desktop	1795207467346	127.0.0.1	Python-urllib/3.13	f
@carol:fixture.local	CAROLDEV	Carol desktop	1795207467780	127.0.0.1	Python-urllib/3.13	f
\.


--
-- Data for Name: e2e_cross_signing_keys; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_cross_signing_keys (user_id, keytype, keydata, stream_id, updatable_without_uia_before_ms, instance_name) FROM stdin;
\.


--
-- Data for Name: e2e_cross_signing_signatures; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_cross_signing_signatures (user_id, key_id, target_user_id, target_device_id, signature) FROM stdin;
\.


--
-- Data for Name: e2e_device_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_device_keys_json (user_id, device_id, ts_added_ms, key_json) FROM stdin;
\.


--
-- Data for Name: e2e_fallback_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_fallback_keys_json (user_id, device_id, algorithm, key_id, key_json, used) FROM stdin;
\.


--
-- Data for Name: e2e_one_time_keys_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_one_time_keys_json (user_id, device_id, algorithm, key_id, ts_added_ms, key_json) FROM stdin;
\.


--
-- Data for Name: e2e_room_keys; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_room_keys (user_id, room_id, session_id, version, first_message_index, forwarded_count, is_verified, session_data) FROM stdin;
\.


--
-- Data for Name: e2e_room_keys_versions; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.e2e_room_keys_versions (user_id, version, algorithm, auth_data, deleted, etag) FROM stdin;
\.


--
-- Data for Name: event_json; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.event_json (event_id, room_id, internal_metadata, json, format_version) FROM stdin;
$k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"fAv1y29/5Ev1SWrW9+damQAkBVPYUnlE2NZVQaS1MkcWtl2MU5PsUhmX186VmMQe9ujbp1HJlXGCPEsiWlsYBA"}},"unsigned":{"age_ts":1795207465042},"auth_events":[],"prev_events":[],"content":{"additional_creators":["@bob:fixture.local"],"room_version":"12"},"depth":1,"hashes":{"sha256":"HQjJHCMunEg5NxboBE+uo9xteW+nUG9qsz/vlG5xyVA"},"origin_server_ts":1795207465042,"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"}	4
$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"mCv9Z7DYQhbiEOB3JYj7TLJXi7vbVuqFTAYKbk0bjc7pl8IpFcRwscDgWzvVZer0u2uhYSZRMGy5+lPAsvTrBQ"}},"unsigned":{"age_ts":1795207465678},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":[],"prev_events":["$k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8"],"content":{"displayname":"alice","membership":"join"},"depth":2,"hashes":{"sha256":"cRxFsW+zL9qLATFat2WkiWxmePW2X2I4miaxhpVTJYE"},"origin_server_ts":1795207465678,"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}	4
$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"RAYrZxzGduqclj08R17C3b1DUgKu1+oh6PZAWflaJqxKfmp4aY/QkX2IR533NPSgWpGB7CGGOwI1pyuVxHl2DA"}},"unsigned":{"age_ts":1795207465971},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":150},"events_default":0,"historical":100,"invite":0,"kick":50,"redact":50,"state_default":50,"users":{},"users_default":0},"depth":3,"hashes":{"sha256":"jmRBuUswwmt5QTId+yLgpOH+kC/nT7Uw0k3oGWBPptc"},"origin_server_ts":1795207465971,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	4
$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"s4vNlESA8lIAe7eABNO/ItOmcfrfOQ/P3zqlSO9fNMgTPTZfDsrnMcNsBLjPLIcYBVBIdPOSsjhVdbvJCJB6AQ"}},"unsigned":{"age_ts":1795207466008},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc"],"content":{"join_rule":"invite"},"depth":4,"hashes":{"sha256":"RfS5w2kMrhsGYgEUPoDsXm39I3sTFzcLTn6GSYLmUus"},"origin_server_ts":1795207466008,"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"}	4
$F1y8uIj5FgVBf3PjTbk3HE6a6HWIbgcphEyODucCuCY	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"GFxQUL+xfVRM/+nGBhkn3SMYPjzDvsz77oN63jcHZXZuZECaHTbjsNeMa2yF1w1UJJt+mRdsTfa+pwgdee2nBg"}},"unsigned":{"age_ts":1795207466010},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M"],"content":{"history_visibility":"shared"},"depth":5,"hashes":{"sha256":"jeIkRWy8a2Kqfk3M9Q+1TFi45h+eHsrpIFuqZK4MIg4"},"origin_server_ts":1795207466010,"sender":"@alice:fixture.local","state_key":"","type":"m.room.history_visibility"}	4
$-gch8UEWgJJBGUBv9oQh7oGzzwjIAEX2gQmS221AYrM	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"6UJVz9KfnfqHxV6Xh/XHkU85lvuGsNTUZ6FBalINKd3EvyaorjKYsGc9bpdYNqzCTakUrJ2Q5SFb56Q/LUt1AA"}},"unsigned":{"age_ts":1795207466012},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$F1y8uIj5FgVBf3PjTbk3HE6a6HWIbgcphEyODucCuCY"],"content":{"guest_access":"can_join"},"depth":6,"hashes":{"sha256":"fAFD7GCAbCa036AOVXWnynWzMU+MJ6IfgEAfbcRGbHI"},"origin_server_ts":1795207466012,"sender":"@alice:fixture.local","state_key":"","type":"m.room.guest_access"}	4
$NM4fHJAwtOtVzoRQjpWUB22LqWnLsPXJ0iU5CB3eRr8	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"TAqzag8dEtWmZzpLMGK7G8uSHPaFkEBS8oB2r1tD9MYzGLRwLJKFCx6Cx3qNg/f7zZWhgZoThC3RoVnFHcm/Dw"}},"unsigned":{"age_ts":1795207466013},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$-gch8UEWgJJBGUBv9oQh7oGzzwjIAEX2gQmS221AYrM"],"content":{"name":"\\u041a\\u043e\\u043c\\u043d\\u0430\\u0442\\u0430 v12"},"depth":7,"hashes":{"sha256":"33rkr7YyW6mXgiGvb8fAbPoHblt/InPLkGz81HwWdVo"},"origin_server_ts":1795207466013,"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"}	4
$F7BYkKJ01hXYFsnIQPLDQqISu1FCLLUVbeTssIwZbfs	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"8ixgclkM/NONDHwrl4CSi6Q9kfv9lzzOQCHMUfAV46ubgAR8myqsGrTyOJG+ieSfI1TEq3V6k4XroLHgch7PDw"}},"unsigned":{"age_ts":1795207466015},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$NM4fHJAwtOtVzoRQjpWUB22LqWnLsPXJ0iU5CB3eRr8"],"content":{"m.topic":{"m.text":[{"body":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"}]},"topic":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"},"depth":8,"hashes":{"sha256":"GhHZBBtc9n12CPvYqfejNTxxbb+VBn7pDHQSoZSsrIE"},"origin_server_ts":1795207466015,"sender":"@alice:fixture.local","state_key":"","type":"m.room.topic"}	4
$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"vxLXCDONDuch9wkVFBr0wkF9R0peG7XazH3vCd/Zm+9V+L7zTGif/n0cxbCPny67Sedfh9uquMeb3bPmYWUVBA"}},"unsigned":{"age_ts":1795207466340,"invite_room_state":[{"content":{"additional_creators":["@bob:fixture.local"],"room_version":"12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"name":"\\u041a\\u043e\\u043c\\u043d\\u0430\\u0442\\u0430 v12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"m.topic":{"m.text":[{"body":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"}]},"topic":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.topic"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}]},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY","$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M"],"prev_events":["$F7BYkKJ01hXYFsnIQPLDQqISu1FCLLUVbeTssIwZbfs"],"content":{"displayname":"bob","membership":"invite"},"depth":9,"hashes":{"sha256":"9RuUskMuqepIJ41QtTiCkjRTcjbArzQGexpV2Hg+UJI"},"origin_server_ts":1795207466340,"sender":"@alice:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	4
$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"8LOfJjcXXrBPa+xI77fQufpkan9AUfqBPv+NpYq66bjChg7uqBwoxqlPNTRfn5Mw+6ZgAxjCpOO/7K5GnJ/CCQ"}},"unsigned":{"age_ts":1795207466879,"invite_room_state":[{"content":{"m.topic":{"m.text":[{"body":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"}]},"topic":"\\u0434\\u043e \\u043f\\u0435\\u0440\\u0435\\u0435\\u0437\\u0434\\u0430"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.topic"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"},{"content":{"name":"\\u041a\\u043e\\u043c\\u043d\\u0430\\u0442\\u0430 v12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"additional_creators":["@bob:fixture.local"],"room_version":"12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"}]},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY","$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M"],"prev_events":["$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk"],"content":{"displayname":"carol","membership":"invite"},"depth":10,"hashes":{"sha256":"tUnxgQnq+/Iwe0ZBVkCHCRKvNNShj+fDLUHlJgD0vcU"},"origin_server_ts":1795207466879,"sender":"@alice:fixture.local","state_key":"@carol:fixture.local","type":"m.room.member"}	4
$mNxnb85BU-7gj_Yj5-ZFwKCENrR8fwPvTmb67Y4jQVE	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV","txn_id":"a1"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"+v8fYmMzbQ3HIn6nrbT+rG7RG/NPswcFkI8zsIe0OfLHxvlRY1AOvQJsUslY9Ow4e6boinZaNOKWXoBCxhhzDw"}},"unsigned":{"age_ts":1795207468430},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo"],"content":{"body":"\\u043f\\u0440\\u0438\\u0432\\u0435\\u0442 \\u0438\\u0437 v12","msgtype":"m.text"},"depth":13,"hashes":{"sha256":"r31fUADKy30+Q6E0IcK8iarfjhPzosy2v0uDfcb9B8Q"},"origin_server_ts":1795207468430,"sender":"@alice:fixture.local","type":"m.room.message"}	4
$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"D0MY1iinWkUs0BFQUeplSO68lvl/In0RHLKSgwOwUvtF/Z1xO8NxIi3mOQqQB39t8nAFHWO5fdXX5Qo2dTtbDw"}},"unsigned":{"age_ts":1795207468913,"replaces_state":"$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc"},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY"],"prev_events":["$r1YKR60rTXUkMJNmK7YEsGI9DZCpZ-KxEJhSHXtl5dA"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":150},"events_default":0,"historical":100,"invite":0,"kick":50,"redact":50,"state_default":50,"users":{"@carol:fixture.local":50},"users_default":0},"depth":15,"hashes":{"sha256":"UciIfR57X7OOWwtCGl7Lx+Y4KDzli96MMdY5baayS8Q"},"origin_server_ts":1795207468913,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	4
$IB6Y_MqFHnUInKr7a6MPYGsNVclxmbyb2IQOaJ9zE8o	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"BOBDEV","txn_id":"r1"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"zsufXJ4FBC9H5rwAz3FRne4dxYQaIk4ycreJnT/7H2ocFDDzSyU1FIztLYykjKPrHG78AobcKx/rG570xEmRDw"}},"unsigned":{"age_ts":1795207469897},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc","$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g"],"prev_events":["$Wyp2x2yyuwDp96HwopQg6nD0nNRpdKHh-h8tzzYpudI"],"content":{"reason":"\\u043c\\u043e\\u0434\\u0435\\u0440\\u0430\\u0446\\u0438\\u044f","redacts":"$Wyp2x2yyuwDp96HwopQg6nD0nNRpdKHh-h8tzzYpudI"},"depth":18,"hashes":{"sha256":"0dTYOtUrzHOO11G3Q3t2c/VKrSk7Ei+00T/PY0a3GHw"},"origin_server_ts":1795207469897,"sender":"@bob:fixture.local","type":"m.room.redaction"}	4
$gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"xEjz3AGFOUh5nvEqMRZ+Lpib0m4XQDFVNdnEOvkLJ1x+MJwoIMGZ4a1bLndwSylVy1lsaIH4HjiAhqMacVH+CA"}},"unsigned":{"age_ts":1795207472866},"auth_events":[],"prev_events":[],"content":{"predecessor":{"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local"},"room_version":"12"},"depth":1,"hashes":{"sha256":"yB6L/PsLSAIA2CNcn+v1soNOoh+5uemfwVs+w151UHY"},"origin_server_ts":1795207472866,"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"}	4
$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"6UYPxpfxxSPqjsQJhgSaMvR9PhObHFZX4ueNsd8yOjvxuA6+tqzIgO8tU+F4k/VBpoSWPxqpkAj/L8vdoTxJAA"}},"unsigned":{"age_ts":1795207474632,"invite_room_state":[{"content":{"predecessor":{"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local"},"room_version":"12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"name":"Upgraded"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}]},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM","$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I"],"prev_events":["$EKwILfSMo2TKPtnbO7darzKPbf16GbPgfWrmLoKkHg8"],"content":{"displayname":"bob","membership":"invite"},"depth":8,"hashes":{"sha256":"5POdURdpG+oYlOD9CD9y+1Zwieivmr1aXPEXjM7TWrI"},"origin_server_ts":1795207474632,"sender":"@alice:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	4
$B1jH6c-cRFrOWR0PnkgFCDZK4z7TzA8F_HS7VCEDC4o	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"BOBDEV","txn_id":"u2"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"+9W5wkO8xomgN4xumZBmeWzpIpcpQYx4Arw9zjVfbo3rqa9/cdTQPtUXcWIKQjxPxYUHQbqO8hZzXJDY3pt3CQ"}},"unsigned":{"age_ts":1795207475260},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE","$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw"],"prev_events":["$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE"],"content":{"body":"\\u043f\\u043e\\u0441\\u043b\\u0435 \\u0437\\u0430\\u043c\\u0435\\u043d\\u044b","msgtype":"m.text"},"depth":10,"hashes":{"sha256":"YpwOobZ4yqJjpjKM+AgsoSXgp49OKKb0PcoPZdu4XHc"},"origin_server_ts":1795207475260,"sender":"@bob:fixture.local","type":"m.room.message"}	4
$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"BOBDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"TnIyJNcA/6TQ8E1GK7gHvQkXhIHOVjVI4vjMVKRkKsFhHKlKdAseCmIjz6bUQTd2X4hveWCPJ5GEG2OS2Y1TCA"}},"unsigned":{"age_ts":1795207467395,"replaces_state":"$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk"},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk","$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M"],"prev_events":["$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c"],"content":{"displayname":"bob","membership":"join"},"depth":11,"hashes":{"sha256":"450Io2TW7Qquf5ePGKZvdnOr4QaL+dkVZic/A9ic9KE"},"origin_server_ts":1795207467395,"sender":"@bob:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	4
$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"CAROLDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"YNnrb+/5Gl0zg47ZjOk7NW5zgFMhr5HjXy6H7HXeD8tqWMsENejOc2Age1tQ3/VQqb5yoXbNdnOugJpYNMyEBg"}},"unsigned":{"age_ts":1795207467828,"replaces_state":"$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c"},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc","$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M","$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c"],"prev_events":["$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc"],"content":{"displayname":"carol","membership":"join"},"depth":12,"hashes":{"sha256":"TbsJitfs0QEKsPt12pc54Ku/0LMGMD6r0KO2H5FNtSY"},"origin_server_ts":1795207467828,"sender":"@carol:fixture.local","state_key":"@carol:fixture.local","type":"m.room.member"}	4
$Wyp2x2yyuwDp96HwopQg6nD0nNRpdKHh-h8tzzYpudI	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"CAROLDEV","txn_id":"c1"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"EZX53DymVx2JaPau7alB6ggowdYZFgGpRK+dOZrP8hfWca1BzMrPhu7yOZuj5USOPBmkltK4usYSZE1psRV8AQ"}},"unsigned":{"age_ts":1795207469530},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g","$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo"],"prev_events":["$8733C3VE4Yuihm9GWz7HX2r4HnxpJTlY58UQ-r_9P98"],"content":{"body":"\\u043b\\u0438\\u0448\\u043d\\u0435\\u0435 \\u0441\\u043e\\u043e\\u0431\\u0449\\u0435\\u043d\\u0438\\u0435 carol","msgtype":"m.text"},"depth":17,"hashes":{"sha256":"RA1l13FhTp3zhRXLiHOYNrR+GUEU6Y3W1mnLzdH6vT4"},"origin_server_ts":1795207469530,"sender":"@carol:fixture.local","type":"m.room.message"}	4
$v0ISLQuPaNYS4pxp2Cr_FmFHwRUpKWXq42cbH6KOs5A	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"DA9Uurq6/uH9GhiLT2IED4+3mBe/KiuirJ08+J5SreHpW2RQEWUjaq34bhukVUtTq3JEs+ksV2zpVZWt5WJrBA"}},"unsigned":{"age_ts":1795207470640,"invite_room_state":[{"content":{"additional_creators":["@bob:fixture.local"],"room_version":"12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"name":"\\u041a\\u043e\\u043c\\u043d\\u0430\\u0442\\u0430 v12"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"topic":"\\u0442\\u0435\\u043c\\u0430 \\u043e\\u0442 carol"},"sender":"@carol:fixture.local","state_key":"","type":"m.room.topic"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}]},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY","$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g","$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M"],"prev_events":["$T8cveHJE3iiz_9NyHYsLxD6493_xGZo-kNCgO5sPL98"],"content":{"displayname":"dave","membership":"invite"},"depth":20,"hashes":{"sha256":"V8R1BnROJF6x9Ul9W40w90ioPVILcvaxsLmXAUPzSAk"},"origin_server_ts":1795207470640,"sender":"@alice:fixture.local","state_key":"@dave:fixture.local","type":"m.room.member"}	4
$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"hUOwOSik8EMO4E7Q+8RKEBx/g4qL9Uhw/CfdY/t34TUs+FOYf3vKF87zVYNgx0+yvidbVi60YG6Trr1YwUmjCQ"}},"unsigned":{"age_ts":1795207470947},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":[],"prev_events":[],"content":{"room_version":"11"},"depth":1,"hashes":{"sha256":"P6hSrTEdIWE/+3y7dy80EIefUiIkB6Vxq6jjyhVCVWk"},"origin_server_ts":1795207470947,"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"}	3
$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"59G+NAhCPNleHlbWOTdzB40/qpbtR9nyfQbfd2gLX+EfirhcWG4xZfBdnFytzBe52pYrZayq9ER+JaY2IdfUCA"}},"unsigned":{"age_ts":1795207471332},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":100},"events_default":0,"historical":100,"invite":0,"kick":50,"redact":50,"state_default":50,"users":{"@alice:fixture.local":100},"users_default":0},"depth":3,"hashes":{"sha256":"ubZt4p6OVDbW6VeyGHfYtG8unGZZeGI2W3ftjMJ5CcI"},"origin_server_ts":1795207471332,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	3
$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"zFmbwSpFBCo4VeJoiUpnb7lR1EgRp/yZNaAdS4ZROQWxe8XZvzTAMOJdhNs9GWCx/U0HdRfC8/chaY6nIjQhDw"}},"unsigned":{"age_ts":1795207471364},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k"],"content":{"join_rule":"invite"},"depth":4,"hashes":{"sha256":"Ax4RMV/nsEcZAZfB/87KU3xGaqUBDbsRs6FYrOotdu0"},"origin_server_ts":1795207471364,"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"}	3
$qdaSMhHXnY8_oHfU5nup32bJkeXRRKOAtJ7fOtFkuf4	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"v94uio51GiTwf1FIKpvgLh/hw81QC0pJ6f47EjbIAAhdd9L5hUYJflXU732Hcd0RVTLXSfV8oQo+JnAJh3diBg"}},"unsigned":{"age_ts":1795207471366},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM"],"content":{"history_visibility":"shared"},"depth":5,"hashes":{"sha256":"5ljRfT+Miot1UW1kuWM5i//4Tmyh7ZIMmpnhlRKy51k"},"origin_server_ts":1795207471366,"sender":"@alice:fixture.local","state_key":"","type":"m.room.history_visibility"}	3
$r1YKR60rTXUkMJNmK7YEsGI9DZCpZ-KxEJhSHXtl5dA	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"BOBDEV","txn_id":"b1"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"WC1Ub1evDG5pIf6YRp1R3veV1EUX0gA0Tu0nMsa64Eix1p+LaMPhvcaxS+UmK57/oPR/Zn6GEDEsD68+/SrWBw"}},"unsigned":{"age_ts":1795207468661},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc","$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc"],"prev_events":["$mNxnb85BU-7gj_Yj5-ZFwKCENrR8fwPvTmb67Y4jQVE"],"content":{"body":"\\u043e\\u0442\\u0432\\u0435\\u0442 \\u0434\\u043e\\u043f\\u043e\\u043b\\u043d\\u0438\\u0442\\u0435\\u043b\\u044c\\u043d\\u043e\\u0433\\u043e \\u0441\\u043e\\u0437\\u0434\\u0430\\u0442\\u0435\\u043b\\u044f","msgtype":"m.text"},"depth":14,"hashes":{"sha256":"x8U47b62aRIFv1nP6jDZXdNYI+PmnqW6lhIBCthimww"},"origin_server_ts":1795207468661,"sender":"@bob:fixture.local","type":"m.room.message"}	4
$8733C3VE4Yuihm9GWz7HX2r4HnxpJTlY58UQ-r_9P98	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"CAROLDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"g+u1EkSb7v0bJjW8nGASRurS/JeQlJ8cLwfeZBEHV4dQNfMSgHbZUOVuDHwN/PrjsMsCacCP5z33PaTEMuurBw"}},"unsigned":{"age_ts":1795207469228,"replaces_state":"$F7BYkKJ01hXYFsnIQPLDQqISu1FCLLUVbeTssIwZbfs"},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g","$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo"],"prev_events":["$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g"],"content":{"topic":"\\u0442\\u0435\\u043c\\u0430 \\u043e\\u0442 carol"},"depth":16,"hashes":{"sha256":"GscKBOHlo5d8bdSSK8fGxFqZmiQWgmJb435vrimR01Q"},"origin_server_ts":1795207469228,"sender":"@carol:fixture.local","state_key":"","type":"m.room.topic"}	4
$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"Xc9yelpK4J02m+Qhnyj0gmn8timnwEdJ6EeT2s63o83gYKOqtntQo5BzksbJeLNuSHOTR1ys4/dOQdrx9UoaBA"}},"unsigned":{"age_ts":1795207471769,"invite_room_state":[{"content":{"room_version":"11"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.create"},{"content":{"join_rule":"invite"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"},{"content":{"name":"Upgraded"},"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"},{"content":{"displayname":"alice","membership":"join"},"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}]},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$TdvUnpfyIvJd25aJQuZ_9dxFEM3OuRnyQaPzkpR-0U4"],"content":{"displayname":"bob","membership":"invite"},"depth":8,"hashes":{"sha256":"W3BhE6mhEOc/FoDHsUdkD2FjSoVffMX6/j/MvWQRDvw"},"origin_server_ts":1795207471769,"sender":"@alice:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	3
$kfjBp62ZFSZuLyZb-NxtL8enhuG7dNELmHavdFIuDLw	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV","txn_id":"u1"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"psqWog9aX3he8/tq5qlPE0mKzWXUS+lDPG0VWe/3j6q85Upm2LLG9Z+eEtXhJuCCHoAE1YSzueQ5+OZzRTRdAg"}},"unsigned":{"age_ts":1795207472475},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$DeHs04kBmQ6v5qqrgG6-PZYCsEJGcTwh0GkZ6t6UlNg"],"content":{"body":"\\u0434\\u043e \\u0437\\u0430\\u043c\\u0435\\u043d\\u044b","msgtype":"m.text"},"depth":10,"hashes":{"sha256":"tx2nmA4xJ6DW5xvA8jTBUnCyUPpGzckMnd1HZMK91SQ"},"origin_server_ts":1795207472475,"sender":"@alice:fixture.local","type":"m.room.message"}	3
$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"UJwkIb4MbzKcuuqAnCxocdymfLHmSvXWr5eaXFXWZon3xJg7cpCeNc/aF9yUGC3AN2Mfb71vRVy64KGqjMvsBw"}},"unsigned":{"age_ts":1795207473638},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"prev_events":["$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":100},"events_default":0,"historical":100,"invite":0,"kick":50,"redact":50,"state_default":50,"users":{},"users_default":0},"depth":3,"hashes":{"sha256":"sRLsztVmNxRT25OwqYA6QEcF/zwpO5R6AyPWPwfbvyE"},"origin_server_ts":1795207473638,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	4
$V3SIVCFsx3-yRQYj9e8bhwP1krSKy3WO-SyoMnLaFi4	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"WdG6HWkoAjNAk7GuShbjEZqYRmHsRTnGs0jeVGN6C4yMhoWgUfCuBvbSuLCSweMxULk88dI2jvIW5P/STcJdBg"}},"unsigned":{"age_ts":1795207473671},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"prev_events":["$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw"],"content":{"guest_access":"can_join"},"depth":4,"hashes":{"sha256":"sPNwe2OlTBlLkpU+sYVvveBIAnnLKabKivvyPhMRuyk"},"origin_server_ts":1795207473671,"sender":"@alice:fixture.local","state_key":"","type":"m.room.guest_access"}	4
$Baj13SXAdqwSBrUIs4mo97J9Pq-zXZeNUxA8lmVYT5I	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"gwfurbcltu016NlC9Q1+1D2z2PcdSf4r6gjwSgQfuDfuG14zydh3mcFEA/uckYuLhqpxqbsBBCcxahxiuUO/DA"}},"unsigned":{"age_ts":1795207473673},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"prev_events":["$V3SIVCFsx3-yRQYj9e8bhwP1krSKy3WO-SyoMnLaFi4"],"content":{"history_visibility":"shared"},"depth":5,"hashes":{"sha256":"ibPywawB8b5WhSjJoAbxsvOwfIBwlMulM016uRrKekI"},"origin_server_ts":1795207473673,"sender":"@alice:fixture.local","state_key":"","type":"m.room.history_visibility"}	4
$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"vQ+NXxkyAFLteXyihKF8qhZUzN/AnLC42qY0MThrsbaQFpMu/CrUdn0VxDkJg3Q+jiDrUwNTzqomGOZShQXBBg"}},"unsigned":{"age_ts":1795207473675},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"prev_events":["$Baj13SXAdqwSBrUIs4mo97J9Pq-zXZeNUxA8lmVYT5I"],"content":{"join_rule":"invite"},"depth":6,"hashes":{"sha256":"kwBpCQbPg5+tA6/L+LD/JtnqsR57r45nDgUcRI6Acjs"},"origin_server_ts":1795207473675,"sender":"@alice:fixture.local","state_key":"","type":"m.room.join_rules"}	4
$T8cveHJE3iiz_9NyHYsLxD6493_xGZo-kNCgO5sPL98	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	{"device_id":"CAROLDEV","txn_id":"c2"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"XruGqyJyludVsd+AZrHkG/Y9Qw0F9YZqWTKhMrmlj4YESlLoXow5NbG+w2xXM5tD7srNmQCcTxndYsQH1/1iCw"}},"unsigned":{"age_ts":1795207470167},"room_id":"!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8","auth_events":["$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g","$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo"],"prev_events":["$IB6Y_MqFHnUInKr7a6MPYGsNVclxmbyb2IQOaJ9zE8o"],"content":{"body":"\\u043f\\u043e\\u0441\\u043b\\u0435 \\u043c\\u043e\\u0434\\u0435\\u0440\\u0430\\u0446\\u0438\\u0438","msgtype":"m.text"},"depth":19,"hashes":{"sha256":"IMso7P4xpdCg9QFrSt26/uSeER4aAhRFaGWIjW7h6vA"},"origin_server_ts":1795207470167,"sender":"@carol:fixture.local","type":"m.room.message"}	4
$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"/qPHk+febzq44FzXARK1qt5LuiSJB2U66v3h75WhvOAhtQn/xaHqtZKfWTKANSNaMaworx8V9SHlvu8mjfuzAg"}},"unsigned":{"age_ts":1795207471127},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"content":{"displayname":"alice","membership":"join"},"depth":2,"hashes":{"sha256":"8guuMaShrFPu9Kw0lshiZXEFwYpFLGiKSmDgAnFfRrs"},"origin_server_ts":1795207471127,"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}	3
$hGc-bVHtxOWQq2kyjkHViwVwaeomeXD6gNY1YG7Ru94	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"1ApNzBeDLWwcmZKSESc5BA1+VDjiVok7PWpMtuSmuSoy+HjOCjfN86lo1DhN0kwTYzo/1lafSazXdAQCNYZlAQ"}},"unsigned":{"age_ts":1795207471367},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$qdaSMhHXnY8_oHfU5nup32bJkeXRRKOAtJ7fOtFkuf4"],"content":{"guest_access":"can_join"},"depth":6,"hashes":{"sha256":"uBeDZKLQaqcTZ52S915Q3i+GU7gsm85VKOUTHNPYRjk"},"origin_server_ts":1795207471367,"sender":"@alice:fixture.local","state_key":"","type":"m.room.guest_access"}	3
$TdvUnpfyIvJd25aJQuZ_9dxFEM3OuRnyQaPzkpR-0U4	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"hMeM/jW/P8yTqUPKDHe5B9/OtA17LFGnKb1TIyQ5519j0OO2LXFwK3GChvVrk0bbR4hh/3yD1XDMdoC89eU0Aw"}},"unsigned":{"age_ts":1795207471369},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$hGc-bVHtxOWQq2kyjkHViwVwaeomeXD6gNY1YG7Ru94"],"content":{"name":"Upgraded"},"depth":7,"hashes":{"sha256":"Aa0LOlUkTr/zRsAS7pttpA3DPjbnC9URABSXOlJI+HY"},"origin_server_ts":1795207471369,"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"}	3
$DeHs04kBmQ6v5qqrgG6-PZYCsEJGcTwh0GkZ6t6UlNg	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"BOBDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"9RDiorF3lprdY8lpmpyxoCZJcoqcPlsbsbcVT1A60sB5t5jRgnSc6nBBR8dt908UKXrE2A3FehqctfwkrQBfAg"}},"unsigned":{"age_ts":1795207472178,"replaces_state":"$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0"},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0","$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0"],"content":{"displayname":"bob","membership":"join"},"depth":9,"hashes":{"sha256":"/n+6QuJhloSHLcyyBQKXvi96mgR7IYpJp8OUG65vtfk"},"origin_server_ts":1795207472178,"sender":"@bob:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	3
$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"CtcDJNLas8cm6QQA1y8GwSNwfuLt9fOxFTsK8OPE7YnwXI/NptEjOtDHQtCdraGAc8O15As9hyWCYO3wlxLCDA"}},"unsigned":{"age_ts":1795207473405},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":[],"prev_events":["$gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs"],"content":{"displayname":"alice","membership":"join"},"depth":2,"hashes":{"sha256":"cchKFvY9PMqaHwCfwqnsW6TuXBOywvN0/ZiDm85IXvw"},"origin_server_ts":1795207473405,"sender":"@alice:fixture.local","state_key":"@alice:fixture.local","type":"m.room.member"}	4
$EKwILfSMo2TKPtnbO7darzKPbf16GbPgfWrmLoKkHg8	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"rHOm/8Wb0oYxc8Yc8cJL8gC1LCPiObAukMP7fowXLjlbjRhLbQHRiKiClJ0Y4xCw+SiWWqSCw3l9G4qUfuF5Bw"}},"unsigned":{"age_ts":1795207473676},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM"],"prev_events":["$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I"],"content":{"name":"Upgraded"},"depth":7,"hashes":{"sha256":"25Q/ZSZaR8xuTKnYUTz1aarJNPDdErcbzXZggJeFQjQ"},"origin_server_ts":1795207473676,"sender":"@alice:fixture.local","state_key":"","type":"m.room.name"}	4
$xiDfJR_-0rsK7Mz3X3nRcEQNZryjLFcyLZDG0HBByaQ	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"7+e0PgLnaS62NbQLB9DjVcpb7rtdohEyCp5p7W7de5TsgyBY0WIpgq90F3tqurWmtIaEYkhJ/a7NbmlcKxtqAg"}},"unsigned":{"age_ts":1795207472996},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$kfjBp62ZFSZuLyZb-NxtL8enhuG7dNELmHavdFIuDLw"],"content":{"body":"This room has been replaced","replacement_room":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs"},"depth":11,"hashes":{"sha256":"sAQdtunuwELdzb5Panc4jLINl/XuUyYY0veXnX3m328"},"origin_server_ts":1795207472996,"sender":"@alice:fixture.local","state_key":"","type":"m.room.tombstone"}	3
$XrnebK1kmZiWWQUdsjTqCXXWpNpF6qT71nt0PKp1ZXM	!TmPykHDOnIszcdrgRJ:fixture.local	{"device_id":"ALICEDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"cIP/JIzAy8WLFLnE7l1b3ZYmSx8S6M6l4PFhXaSHlifbhELXuDP1x1PjSaDw7nTQ5rT34BZCACW6piFOB1EpBg"}},"unsigned":{"age_ts":1795207474284,"replaces_state":"$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k"},"room_id":"!TmPykHDOnIszcdrgRJ:fixture.local","auth_events":["$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k","$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM","$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg"],"prev_events":["$xiDfJR_-0rsK7Mz3X3nRcEQNZryjLFcyLZDG0HBByaQ"],"content":{"ban":50,"events":{"m.room.avatar":50,"m.room.canonical_alias":50,"m.room.encryption":100,"m.room.history_visibility":100,"m.room.name":50,"m.room.power_levels":100,"m.room.server_acl":100,"m.room.tombstone":100},"events_default":50,"historical":100,"invite":50,"kick":50,"redact":50,"state_default":50,"users":{"@alice:fixture.local":100},"users_default":0},"depth":12,"hashes":{"sha256":"oV5PUNIzyNu6RTx9Vkehv/RAecomX3S6H/amhi2qEKU"},"origin_server_ts":1795207474284,"sender":"@alice:fixture.local","state_key":"","type":"m.room.power_levels"}	3
$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	{"device_id":"BOBDEV"}	{"signatures":{"fixture.local":{"ed25519:a_VOHe":"hDA5gj13/GnUPqwvQbRygri/fl2mOWNA422QwKk/AzSK4CBEiFSAh3hB9m6mtStGs6m1Sg76CyTvQrCLnbBuBg"}},"unsigned":{"age_ts":1795207474947,"replaces_state":"$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg"},"room_id":"!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs","auth_events":["$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg","$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw","$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I"],"prev_events":["$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg"],"content":{"displayname":"bob","membership":"join"},"depth":9,"hashes":{"sha256":"syTg0+CJy4EnPkFo5JsOj0nNQTuQdLhpqRkRdCjdK38"},"origin_server_ts":1795207474947,"sender":"@bob:fixture.local","state_key":"@bob:fixture.local","type":"m.room.member"}	4
\.


--
-- Data for Name: events; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.events (topological_ordering, event_id, type, room_id, content, unrecognized_keys, processed, outlier, depth, origin_server_ts, received_ts, sender, contains_url, instance_name, stream_ordering, state_key, rejection_reason) FROM stdin;
1	$k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	m.room.create	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	1	1795207465042	1795207465456	@alice:fixture.local	f	master	2		\N
2	$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	2	1795207465678	1795207465785	@alice:fixture.local	f	master	3	@alice:fixture.local	\N
3	$Qm08roCbHNOKn_U7aDmXxv0ugt_Ywfp3QJxoD0TR3dc	m.room.power_levels	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	3	1795207465971	1795207466156	@alice:fixture.local	f	master	4		\N
4	$-uuwc1aF5XTxPB6Mkt8cBW5p9oMhb5RaFMGdGw__59M	m.room.join_rules	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	4	1795207466008	1795207466156	@alice:fixture.local	f	master	5		\N
5	$F1y8uIj5FgVBf3PjTbk3HE6a6HWIbgcphEyODucCuCY	m.room.history_visibility	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	5	1795207466010	1795207466156	@alice:fixture.local	f	master	6		\N
6	$-gch8UEWgJJBGUBv9oQh7oGzzwjIAEX2gQmS221AYrM	m.room.guest_access	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	6	1795207466012	1795207466156	@alice:fixture.local	f	master	7		\N
7	$NM4fHJAwtOtVzoRQjpWUB22LqWnLsPXJ0iU5CB3eRr8	m.room.name	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	7	1795207466013	1795207466156	@alice:fixture.local	f	master	8		\N
8	$F7BYkKJ01hXYFsnIQPLDQqISu1FCLLUVbeTssIwZbfs	m.room.topic	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	8	1795207466015	1795207466156	@alice:fixture.local	f	master	9		\N
9	$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	9	1795207466340	1795207466498	@alice:fixture.local	f	master	10	@bob:fixture.local	\N
10	$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	10	1795207466879	1795207467131	@alice:fixture.local	f	master	11	@carol:fixture.local	\N
11	$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	11	1795207467395	1795207467528	@bob:fixture.local	f	master	12	@bob:fixture.local	\N
12	$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	12	1795207467828	1795207468015	@carol:fixture.local	f	master	13	@carol:fixture.local	\N
13	$mNxnb85BU-7gj_Yj5-ZFwKCENrR8fwPvTmb67Y4jQVE	m.room.message	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	13	1795207468430	1795207468531	@alice:fixture.local	f	master	14	\N	\N
14	$r1YKR60rTXUkMJNmK7YEsGI9DZCpZ-KxEJhSHXtl5dA	m.room.message	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	14	1795207468661	1795207468786	@bob:fixture.local	f	master	15	\N	\N
15	$DrauGC1CJLHzQQrcADyz_yygbpzOSJfiIJhhDIvNp6g	m.room.power_levels	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	15	1795207468913	1795207469056	@alice:fixture.local	f	master	16		\N
16	$8733C3VE4Yuihm9GWz7HX2r4HnxpJTlY58UQ-r_9P98	m.room.topic	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	16	1795207469228	1795207469331	@carol:fixture.local	f	master	17		\N
17	$Wyp2x2yyuwDp96HwopQg6nD0nNRpdKHh-h8tzzYpudI	m.room.message	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	17	1795207469530	1795207469681	@carol:fixture.local	f	master	18	\N	\N
18	$IB6Y_MqFHnUInKr7a6MPYGsNVclxmbyb2IQOaJ9zE8o	m.room.redaction	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	18	1795207469897	1795207469978	@bob:fixture.local	f	master	19	\N	\N
19	$T8cveHJE3iiz_9NyHYsLxD6493_xGZo-kNCgO5sPL98	m.room.message	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	19	1795207470167	1795207470231	@carol:fixture.local	f	master	20	\N	\N
20	$v0ISLQuPaNYS4pxp2Cr_FmFHwRUpKWXq42cbH6KOs5A	m.room.member	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	\N	\N	t	f	20	1795207470640	1795207470739	@alice:fixture.local	f	master	21	@dave:fixture.local	\N
1	$hV2MoxVZsebubspeFJzkcfYGwfkEg9ptfVXVGe2EdXg	m.room.create	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	1	1795207470947	1795207471023	@alice:fixture.local	f	master	22		\N
2	$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM	m.room.member	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	2	1795207471127	1795207471204	@alice:fixture.local	f	master	23	@alice:fixture.local	\N
3	$XbRqAl2k7pJ0URelOQ8HzIwmsBPqXUFoHZzQeTj0Q3k	m.room.power_levels	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	3	1795207471332	1795207471478	@alice:fixture.local	f	master	24		\N
4	$CGNKGFd1iMJ4VauzYuIRlzMUGhURTmoZR2RB_gVc9XM	m.room.join_rules	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	4	1795207471364	1795207471478	@alice:fixture.local	f	master	25		\N
5	$qdaSMhHXnY8_oHfU5nup32bJkeXRRKOAtJ7fOtFkuf4	m.room.history_visibility	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	5	1795207471366	1795207471478	@alice:fixture.local	f	master	26		\N
6	$hGc-bVHtxOWQq2kyjkHViwVwaeomeXD6gNY1YG7Ru94	m.room.guest_access	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	6	1795207471367	1795207471478	@alice:fixture.local	f	master	27		\N
7	$TdvUnpfyIvJd25aJQuZ_9dxFEM3OuRnyQaPzkpR-0U4	m.room.name	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	7	1795207471369	1795207471478	@alice:fixture.local	f	master	28		\N
8	$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0	m.room.member	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	8	1795207471769	1795207471982	@alice:fixture.local	f	master	29	@bob:fixture.local	\N
9	$DeHs04kBmQ6v5qqrgG6-PZYCsEJGcTwh0GkZ6t6UlNg	m.room.member	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	9	1795207472178	1795207472265	@bob:fixture.local	f	master	30	@bob:fixture.local	\N
10	$kfjBp62ZFSZuLyZb-NxtL8enhuG7dNELmHavdFIuDLw	m.room.message	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	10	1795207472475	1795207472612	@alice:fixture.local	f	master	31	\N	\N
1	$gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	m.room.create	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	1	1795207472866	1795207473212	@alice:fixture.local	f	master	32		\N
2	$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM	m.room.member	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	2	1795207473405	1795207473520	@alice:fixture.local	f	master	33	@alice:fixture.local	\N
3	$Hlz9ZCRsLEp5gWos4d2CZkXAVyPOnG-MlVwNW_nUkKw	m.room.power_levels	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	3	1795207473638	1795207473811	@alice:fixture.local	f	master	34		\N
4	$V3SIVCFsx3-yRQYj9e8bhwP1krSKy3WO-SyoMnLaFi4	m.room.guest_access	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	4	1795207473671	1795207473811	@alice:fixture.local	f	master	35		\N
5	$Baj13SXAdqwSBrUIs4mo97J9Pq-zXZeNUxA8lmVYT5I	m.room.history_visibility	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	5	1795207473673	1795207473811	@alice:fixture.local	f	master	36		\N
6	$vbDODGwq68Xy2w9HU8So___6bl8w_5a01-tlRD01H_I	m.room.join_rules	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	6	1795207473675	1795207473811	@alice:fixture.local	f	master	37		\N
7	$EKwILfSMo2TKPtnbO7darzKPbf16GbPgfWrmLoKkHg8	m.room.name	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	7	1795207473676	1795207473811	@alice:fixture.local	f	master	38		\N
11	$xiDfJR_-0rsK7Mz3X3nRcEQNZryjLFcyLZDG0HBByaQ	m.room.tombstone	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	11	1795207472996	1795207473990	@alice:fixture.local	f	master	39		\N
12	$XrnebK1kmZiWWQUdsjTqCXXWpNpF6qT71nt0PKp1ZXM	m.room.power_levels	!TmPykHDOnIszcdrgRJ:fixture.local	\N	\N	t	f	12	1795207474284	1795207474412	@alice:fixture.local	f	master	40		\N
9	$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE	m.room.member	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	9	1795207474947	1795207475070	@bob:fixture.local	f	master	42	@bob:fixture.local	\N
8	$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg	m.room.member	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	8	1795207474632	1795207474745	@alice:fixture.local	f	master	41	@bob:fixture.local	\N
10	$B1jH6c-cRFrOWR0PnkgFCDZK4z7TzA8F_HS7VCEDC4o	m.room.message	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	\N	\N	t	f	10	1795207475260	1795207475420	@bob:fixture.local	f	master	43	\N	\N
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
alice	alice	\N	@alice:fixture.local	\N
bob	bob	\N	@bob:fixture.local	\N
carol	carol	\N	@carol:fixture.local	\N
dave	dave	\N	@dave:fixture.local	\N
\.


--
-- Data for Name: push_rules; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.push_rules (id, user_name, rule_id, priority_class, priority, conditions, actions) FROM stdin;
\.


--
-- Data for Name: push_rules_enable; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.push_rules_enable (id, user_name, rule_id, enabled) FROM stdin;
\.


--
-- Data for Name: pushers; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.pushers (id, user_name, access_token, profile_tag, kind, app_id, app_display_name, device_display_name, pushkey, ts, lang, data, last_stream_ordering, last_success, failing_since, enabled, device_id, instance_name) FROM stdin;
\.


--
-- Data for Name: redactions; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.redactions (event_id, redacts, have_censored, received_ts, recheck) FROM stdin;
$IB6Y_MqFHnUInKr7a6MPYGsNVclxmbyb2IQOaJ9zE8o	$Wyp2x2yyuwDp96HwopQg6nD0nNRpdKHh-h8tzzYpudI	f	1795207470009	f
\.


--
-- Data for Name: refresh_tokens; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.refresh_tokens (id, user_id, device_id, token, next_token_id, expiry_ts, ultimate_session_expiry_ts) FROM stdin;
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
\.


--
-- Data for Name: room_aliases; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_aliases (room_alias, room_id, creator) FROM stdin;
#v12:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	@alice:fixture.local
\.


--
-- Data for Name: room_memberships; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_memberships (event_id, user_id, sender, room_id, membership, forgotten, display_name, avatar_url, event_stream_ordering, participant) FROM stdin;
$hFZ1HQrK_L3kozXkhZXa4QT3ZYre1M76ihDOKYuZxOk	@bob:fixture.local	@alice:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	invite	0	bob	\N	10	f
$yavng1ULZJoa2puyE_q89QoPBbnrIlUPAApo8aJhk7c	@carol:fixture.local	@alice:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	invite	0	carol	\N	11	f
$8TIUzSbrKoa-Nbna2iYpHrRIGvb2fK2qIiUhcKvNhwY	@alice:fixture.local	@alice:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	join	0	alice	\N	3	t
$J0UgdHGCGiPbDAZ6KfYDw1w-SoGA4YD9__GsPgiE1wc	@bob:fixture.local	@bob:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	join	0	bob	\N	12	t
$y8_mQljL_yzMOhf7xWWOfv6Bc3yc2U6H4eGzW1aEPFo	@carol:fixture.local	@carol:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	join	0	carol	\N	13	t
$v0ISLQuPaNYS4pxp2Cr_FmFHwRUpKWXq42cbH6KOs5A	@dave:fixture.local	@alice:fixture.local	!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	invite	0	dave	\N	21	f
$BVZ55WKsXsO9Y_5zjc16P94nUc5nSC_It5amjmeNPz0	@bob:fixture.local	@alice:fixture.local	!TmPykHDOnIszcdrgRJ:fixture.local	invite	0	bob	\N	29	f
$DeHs04kBmQ6v5qqrgG6-PZYCsEJGcTwh0GkZ6t6UlNg	@bob:fixture.local	@bob:fixture.local	!TmPykHDOnIszcdrgRJ:fixture.local	join	0	bob	\N	30	f
$cm1qrDSg1I090B7N0RRLe-EGDepBLAJ68N4IDbDdoWM	@alice:fixture.local	@alice:fixture.local	!TmPykHDOnIszcdrgRJ:fixture.local	join	0	alice	\N	23	t
$JgSSaAJM-C6drR0J-lOaqEF2enP6NMUaJkZlHCikiIM	@alice:fixture.local	@alice:fixture.local	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	join	0	alice	\N	33	f
$a4JlVnZHJp6d7lAW2NBQkNU-exv-IAIbIaS7LjDQoZg	@bob:fixture.local	@alice:fixture.local	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	invite	0	bob	\N	41	f
$0JDGzKz56OF2IC1TUd4QCzHYMMXQnpw5Hb1PjnG-EJE	@bob:fixture.local	@bob:fixture.local	!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	join	0	bob	\N	42	t
\.


--
-- Data for Name: room_tags; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.room_tags (user_id, room_id, tag, content) FROM stdin;
\.


--
-- Data for Name: rooms; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.rooms (room_id, is_public, creator, room_version, has_auth_chain_index) FROM stdin;
!k0VFWGD6FN1gwSvyMvPmKhHAsI24vcdFrKp9f9ct-P8	f	@alice:fixture.local	12	t
!TmPykHDOnIszcdrgRJ:fixture.local	f	@alice:fixture.local	11	t
!gjINBAxzgofB30rx2WDy-9wxEJpWI0-NwadMiO6IsZs	f	@alice:fixture.local	12	t
\.


--
-- Data for Name: user_filters; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.user_filters (user_id, filter_id, filter_json, full_user_id) FROM stdin;
\.


--
-- Data for Name: users; Type: TABLE DATA; Schema: synapse; Owner: -
--

COPY synapse.users (name, password_hash, creation_ts, admin, upgrade_ts, is_guest, appservice_id, consent_version, consent_server_notice_sent, user_type, deactivated, shadow_banned, consent_ts, approved, locked, suspended) FROM stdin;
@alice:fixture.local	$2b$12$AZhaUo0tptGNF7SOHe1kUehfXX5ZgocTvVG.a4LHZwGvcH7HLyuqG	1795207459	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@bob:fixture.local	$2b$12$38G7KYSatDaZ1lPCcgml9O8zXKjtIw2Retfe6W58KjE6Pa/aWkTnK	1795207460	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@carol:fixture.local	$2b$12$nCHpjxs/c1xIU//r5mYEGuCZYgmxMQe8WRCUJ.PejdV4ICA0va.Jq	1795207460	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
@dave:fixture.local	$2b$12$NVelHI6eNxPM/gVNX.w11OTCuGc1NlMkpzt9gNyMK6F3HhLFB3Hb6	1795207461	0	\N	0	\N	\N	\N	\N	0	f	\N	t	f	f
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
-- Name: redactions redactions_event_id_key; Type: CONSTRAINT; Schema: synapse; Owner: -
--

ALTER TABLE ONLY synapse.redactions
    ADD CONSTRAINT redactions_event_id_key UNIQUE (event_id);


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
-- Name: events_jump_to_date_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX events_jump_to_date_idx ON synapse.events USING btree (room_id, origin_server_ts) WHERE (NOT outlier);


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
-- Name: redactions_have_censored_ts; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX redactions_have_censored_ts ON synapse.redactions USING btree (received_ts) WHERE (NOT have_censored);


--
-- Name: redactions_redacts; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX redactions_redacts ON synapse.redactions USING btree (redacts);


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
-- Name: room_membership_user_room_idx; Type: INDEX; Schema: synapse; Owner: -
--

CREATE INDEX room_membership_user_room_idx ON synapse.room_memberships USING btree (user_id, room_id);


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


