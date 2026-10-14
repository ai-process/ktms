-- Weighted routing: per-model traffic weight within a rule's chain (0 = unweighted/deterministic).
ALTER TABLE rule_model ADD COLUMN IF NOT EXISTS weight INT NOT NULL DEFAULT 0;
