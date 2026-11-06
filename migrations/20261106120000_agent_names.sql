-- An agent's name is free again once its key is revoked, so a new key of that
-- name, in any case, can become the agent; released marks the old card.
ALTER TABLE agent ADD COLUMN IF NOT EXISTS released BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE agent SET released = TRUE FROM api_key k WHERE k.id = agent.key_id AND k.disabled_at IS NOT NULL;
ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS agent_name_live_idx ON agent (name) WHERE NOT released;
CREATE INDEX IF NOT EXISTS agent_name_idx ON agent (name);

-- Tasks one agent sent another lately, for AGENTS_MAX_TASKS_PER_PAIR_HOUR.
CREATE INDEX IF NOT EXISTS agent_task_pair_idx ON agent_task (from_agent, to_agent, created_at);
