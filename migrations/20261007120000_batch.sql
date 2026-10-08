-- Batch generation: batch-specific model pricing, batch jobs, and results.
ALTER TABLE model ADD COLUMN IF NOT EXISTS price_in_batch_per_mtok numeric NOT NULL DEFAULT 0;
ALTER TABLE model ADD COLUMN IF NOT EXISTS price_out_batch_per_mtok numeric NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS batch (
    id UUID PRIMARY KEY,
    client_batch_id TEXT,
    api_key_id UUID NOT NULL REFERENCES api_key(id),
    key_name TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    vendor_job_name TEXT NOT NULL,
    state TEXT NOT NULL, -- PENDING, RUNNING, SUCCEEDED, FAILED, CANCELLED, EXPIRED
    total_count INT NOT NULL DEFAULT 0,
    done_count INT NOT NULL DEFAULT 0,
    failed_count INT NOT NULL DEFAULT 0,
    attributes JSONB NOT NULL DEFAULT '{}',
    action_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    leased_until TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_batch_client_batch_id ON batch (api_key_id, client_batch_id)
WHERE client_batch_id IS NOT NULL AND client_batch_id != '';

CREATE INDEX IF NOT EXISTS idx_batch_unfinished ON batch (state)
WHERE state IN ('PENDING', 'RUNNING');

CREATE INDEX IF NOT EXISTS idx_batch_api_key_id ON batch (api_key_id);

CREATE TABLE IF NOT EXISTS batch_item (
    batch_id UUID NOT NULL REFERENCES batch(id) ON DELETE CASCADE,
    custom_id TEXT NOT NULL,
    position INT NOT NULL,
    status TEXT NOT NULL, -- PENDING, SUCCEEDED, FAILED
    response_schema JSONB,
    result JSONB,
    error JSONB,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (batch_id, custom_id)
);

CREATE INDEX IF NOT EXISTS idx_batch_item_batch_pos ON batch_item (batch_id, position);
