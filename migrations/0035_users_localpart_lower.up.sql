-- 0035_users_localpart_lower: регистронезависимый поиск пользователя при входе.
--
-- Synapse ищет учетную запись по lower(user_id): одно совпадение - оно, несколько -
-- только точное. Перенесенные из Synapse localpart бывают с заглавными буквами
-- (старые регистрации), и поиск по точному совпадению их не находил.
CREATE INDEX IF NOT EXISTS users_localpart_lower_idx ON users (lower(localpart));
