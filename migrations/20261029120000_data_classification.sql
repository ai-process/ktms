-- data_classification "restricted" on a key or a team keeps every call of the
-- key on self-hosted nodes: no cloud vendor, no cloud fallback. '' is unrestricted.
ALTER TABLE api_key ADD COLUMN IF NOT EXISTS data_classification TEXT NOT NULL DEFAULT '';
ALTER TABLE team ADD COLUMN IF NOT EXISTS data_classification TEXT NOT NULL DEFAULT '';
