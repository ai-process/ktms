-- Per-key policy on top of the team's: the models a key may call and its
-- request and token rates over all models. Empty and 0 mean no limit.
ALTER TABLE api_key ADD COLUMN IF NOT EXISTS allowed_models text[] NOT NULL DEFAULT '{}';
ALTER TABLE api_key ADD COLUMN IF NOT EXISTS rpm_limit integer NOT NULL DEFAULT 0;
ALTER TABLE api_key ADD COLUMN IF NOT EXISTS tpm_limit bigint NOT NULL DEFAULT 0;
