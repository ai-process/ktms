-- Flexible throttling: burst capacity, queue wait timeouts, grace margins, and user rate limits.
ALTER TABLE model ADD COLUMN IF NOT EXISTS burst INT NOT NULL DEFAULT 0;
ALTER TABLE model ADD COLUMN IF NOT EXISTS queue_timeout_ms INT NOT NULL DEFAULT 0;
ALTER TABLE model ADD COLUMN IF NOT EXISTS budget_grace_margin DOUBLE PRECISION NOT NULL DEFAULT 0.0;

CREATE TABLE IF NOT EXISTS user_rate_limits (
    user_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    rpm INT NOT NULL DEFAULT 0,
    burst INT NOT NULL DEFAULT 0,
    queue_timeout_ms INT NOT NULL DEFAULT 0,
    daily_tokens BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, model_id)
);

CREATE INDEX IF NOT EXISTS user_rate_limits_user_id_idx ON user_rate_limits (user_id);
