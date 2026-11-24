-- Translate models: the language pairs a model serves (empty = any) and a price
-- per million source characters, the unit MT vendors bill by.
ALTER TABLE model ADD COLUMN IF NOT EXISTS languages_source text[] NOT NULL DEFAULT '{}';
ALTER TABLE model ADD COLUMN IF NOT EXISTS languages_target text[] NOT NULL DEFAULT '{}';
ALTER TABLE model ADD COLUMN IF NOT EXISTS price_per_million_chars numeric NOT NULL DEFAULT 0;
