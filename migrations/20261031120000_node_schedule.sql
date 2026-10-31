-- Schedules KTMS keeps for nodes whose agents hand their schedule to it
-- (ktms-node --schedule-source=ktms). The agent re-applies on a new version.
CREATE TABLE IF NOT EXISTS node_schedule (
    node_id TEXT PRIMARY KEY,
    schedule TEXT NOT NULL DEFAULT '',
    version BIGINT NOT NULL DEFAULT 1,
    override_level INT CHECK (override_level BETWEEN 0 AND 100),
    override_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((override_level IS NULL) = (override_until IS NULL))
);
