CREATE TABLE IF NOT EXISTS user_filters (
    user_id   text NOT NULL,
    filter_id text NOT NULL,
    content   jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, filter_id)
);
