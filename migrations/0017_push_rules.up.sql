-- Пользовательские push-правила. Дефолтные правила синтезируются в коде.
CREATE TABLE IF NOT EXISTS push_rules (
    user_id   text NOT NULL,
    scope     text NOT NULL,
    kind      text NOT NULL,
    rule_id   text NOT NULL,
    priority  int  NOT NULL DEFAULT 0,
    enabled   boolean NOT NULL DEFAULT true,
    conditions jsonb NOT NULL DEFAULT '[]'::jsonb,
    actions    jsonb NOT NULL DEFAULT '[]'::jsonb,
    pattern    text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, kind, rule_id)
);
