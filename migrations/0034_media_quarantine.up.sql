-- 0034: карантин медиа при переносе из Synapse.
--
-- quarantined_by - кто поместил медиа в карантин (local_media_repository.quarantined_by
-- Synapse). Соблюдается через blocked: заблокированное медиа не отдается ни
-- скачиванием, ни миниатюрой. Само поле нужно, чтобы карантин не терялся при
-- обратной выгрузке (syngo-export) и был виден в сверке.
ALTER TABLE media ADD COLUMN IF NOT EXISTS quarantined_by text;
