DROP INDEX IF EXISTS account_data_stream_idx;
DELETE FROM account_data WHERE content = '{}'::jsonb;
ALTER TABLE account_data DROP COLUMN IF EXISTS stream_ordering;
