-- Медиа живого Synapse 1.165.0 для тестов экспорта и сверки медиа:
-- local_media_repository как есть (pg_dump -t local_media_repository --column-inserts),
-- в схеме synapse_fixture; грузится после synapse-1.165.sql. Строки - из репетиции
-- миграции (tools/migration-rehearsal): два загруженных PNG, бинарный файл, медиа
-- в карантине (admin API quarantine) и две строки превью ссылки (url_cache), user_id
-- переведены на пользователей фикстуры (c4.test). Файлы - synapse-media/local_content
-- (раскладка media_store Synapse; файлов превью нет - они не переносятся).
CREATE TABLE synapse_fixture.local_media_repository (
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
ALTER TABLE ONLY synapse_fixture.local_media_repository
    ADD CONSTRAINT local_media_repository_media_id_key UNIQUE (media_id);
CREATE INDEX local_media_repository_sha256 ON synapse_fixture.local_media_repository USING btree (sha256) WHERE (sha256 IS NOT NULL);
CREATE INDEX local_media_repository_url_idx ON synapse_fixture.local_media_repository USING btree (created_ts) WHERE (url_cache IS NOT NULL);
CREATE INDEX users_have_local_media ON synapse_fixture.local_media_repository USING btree (user_id, created_ts);
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('2026-11-20_lZfBCynoTCFonPSk', 'image/png', 8595, 1795196442241, NULL, '@alice:c4.test', NULL, 'http://syngo-rh-web:8000/og.png', NULL, false, true, '79c09da45f8f451e0a36e820de9c9c2c158d81235fa3cc7ca2b8ae6529fffa28');
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('2026-11-20_rHSBsqRlVsnGyshy', 'text/html', 199, 1795196441872, NULL, '@alice:c4.test', NULL, 'http://syngo-rh-web:8000/page.html', NULL, false, true, '0a0bbc17ded6738aacd598b63137b8458b571a6a1b1a8558e98c5fe4a469f916');
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('BUxrpOQdskLEoOPyHNIfvrjD', 'image/png', 852, 1795196421340, 'pic0.png', '@alice:c4.test', NULL, NULL, NULL, false, true, 'bc7a278edfd181f4ae4d19615b998be41d3acad75685b1696feb49f1b084bb3f');
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('HUETQSZOhOGDWXAutUjoBFJg', 'image/png', 6188, 1795196437119, 'pic17.png', '@carol:c4.test', NULL, NULL, NULL, false, true, 'f0b78ddedc1aae2e3eb87fc3d3037e28a1492d1dc50aa4a2a714ed1543dbe967');
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('ILiNILCJgUPPBYnWRiaJnqhn', 'image/png', 8548, 1795196422406, 'pic1.png', '@bob:c4.test', '@alice:c4.test', NULL, NULL, false, true, '0c5f41cac8cfaaa736288f699b5923548691e2bbb239618cfd40cb23a70b593e');
INSERT INTO synapse_fixture.local_media_repository (media_id, media_type, media_length, created_ts, upload_name, user_id, quarantined_by, url_cache, last_access_ts, safe_from_quarantine, authenticated, sha256) VALUES ('OFGnbiwgWppuHChYKrhlkztB', 'application/octet-stream', 1024, 1795196440339, 'blob0.bin', '@alice:c4.test', NULL, NULL, NULL, false, true, '0c94211e0e60bb151e0627682f7ae37475917cd434612b1995c06654798c0e4e');
