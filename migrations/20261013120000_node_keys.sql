-- Node credentials for self-registering local inference machines.
-- Partial unique indexes allow minting a replacement key for the same node_id
-- or name after the previous key is revoked.
CREATE TABLE IF NOT EXISTS node_key (
    id BIGSERIAL PRIMARY KEY,
    node_id TEXT NOT NULL,
    name TEXT NOT NULL,
    key_lookup TEXT NOT NULL UNIQUE,
    key_hash TEXT NOT NULL,
    allowed_cidrs TEXT[] NOT NULL DEFAULT '{}',
    allowed_models TEXT[] NOT NULL DEFAULT '{}',
    max_base_weight DOUBLE PRECISION NOT NULL DEFAULT 1000.0,
    max_concurrency INT NOT NULL DEFAULT 256,
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS node_key_active_node_id_idx
    ON node_key (node_id) WHERE disabled_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS node_key_active_name_idx
    ON node_key (name) WHERE disabled_at IS NULL;
