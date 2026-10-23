-- Prompt-cache rates. Vendors bill cached input apart from plain input (Anthropic
-- reads at 0.1x and writes at 1.25x, OpenAI and Gemini read at a discount), so
-- one input price misprices every cached call. 0 means the vendor default.
ALTER TABLE model ADD COLUMN IF NOT EXISTS price_cache_read_per_mtok numeric NOT NULL DEFAULT 0;
ALTER TABLE model ADD COLUMN IF NOT EXISTS price_cache_write_per_mtok numeric NOT NULL DEFAULT 0;
