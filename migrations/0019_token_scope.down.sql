-- Откат 0019_token_scope.
DROP INDEX IF EXISTS access_tokens_scope_idx;
ALTER TABLE access_tokens DROP COLUMN IF EXISTS scope;
