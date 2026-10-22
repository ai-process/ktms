-- Hierarchical Multi-Tenancy: Organizations, Teams and scoped API keys.
CREATE TABLE IF NOT EXISTS organization (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE CHECK (name <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS team (
    id UUID PRIMARY KEY,
    org_id UUID NOT NULL REFERENCES organization(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name <> ''),
    monthly_budget_usd NUMERIC NOT NULL DEFAULT 0 CHECK (monthly_budget_usd >= 0),
    allowed_models TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (id, org_id),
    UNIQUE (org_id, name)
);

CREATE INDEX IF NOT EXISTS idx_team_org_id ON team(org_id);

ALTER TABLE api_key
    ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organization(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS team_id UUID REFERENCES team(id) ON DELETE RESTRICT;

-- Guarded: migrations must be safe to re-run (see migrations/embed.go).
DO $$
BEGIN
    ALTER TABLE api_key
        ADD CONSTRAINT fk_api_key_team_org FOREIGN KEY (team_id, org_id) REFERENCES team(id, org_id) ON DELETE RESTRICT;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_api_key_org_id ON api_key(org_id);
CREATE INDEX IF NOT EXISTS idx_api_key_team_id ON api_key(team_id);
