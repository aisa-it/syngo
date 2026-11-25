-- 0002_media: метаданные медиа для блока MEDIA (P0, последний блок P0).
-- Сами байты лежат в файловом хранилище/S3, в БД — только метаданные и ключ объекта.

CREATE TABLE media (
    content_uri      text PRIMARY KEY,
    media_id         text        NOT NULL,
    server_name      text        NOT NULL,
    uploader         text,
    content_type     text        NOT NULL,
    size_bytes       bigint      NOT NULL,
    sha256           bytea,
    file_name        text,
    storage_key      text        NOT NULL,
    is_remote        boolean     NOT NULL DEFAULT false,
    blocked          boolean     NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_accessed_at timestamptz,
    CONSTRAINT media_content_uri_format CHECK (content_uri LIKE 'mxc://%'),
    CONSTRAINT media_media_id_not_empty CHECK (media_id <> ''),
    CONSTRAINT media_size_non_negative CHECK (size_bytes >= 0),
    CONSTRAINT media_storage_key_not_empty CHECK (storage_key <> ''),
    CONSTRAINT media_sha256_len CHECK (sha256 IS NULL OR octet_length(sha256) = 32)
);

-- Идентификация медиа по (server, media_id): так приходит /media/download/{serverName}/{mediaId}.
CREATE UNIQUE INDEX media_server_media_id_key ON media (server_name, media_id);

CREATE INDEX media_uploader_idx ON media (uploader);
CREATE INDEX media_sha256_idx ON media (sha256) WHERE sha256 IS NOT NULL;

COMMENT ON TABLE media IS 'Метаданные медиа: байты в объектном хранилище, здесь content_uri и storage_key';
