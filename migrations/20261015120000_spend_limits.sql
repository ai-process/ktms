-- Hard monetary caps (nano-USD, 1e-9 USD) per subject and UTC calendar period.
-- Counters live in Redis; this table is the source of truth for the caps.
-- 'team' is reserved for team-scoped caps and is not enforced yet.
CREATE TABLE IF NOT EXISTS spend_limit (
    subject_type TEXT NOT NULL CHECK (subject_type IN ('key', 'user', 'team')),
    subject_id TEXT NOT NULL CHECK (subject_id <> ''),
    period TEXT NOT NULL CHECK (period IN ('day', 'month')),
    limit_nanousd BIGINT NOT NULL CHECK (limit_nanousd > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject_type, subject_id, period)
);
