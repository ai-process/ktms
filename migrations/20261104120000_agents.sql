-- Agents talk through Keystone: an agent is a client key with a card, a task
-- is one request from one agent to another (A2A task), and its messages are
-- the conversation. owner names the person a key belongs to.
ALTER TABLE api_key ADD COLUMN IF NOT EXISTS owner TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS api_key_owner_idx ON api_key (owner) WHERE owner <> '';

CREATE TABLE IF NOT EXISTS agent (
    key_id UUID PRIMARY KEY REFERENCES api_key(id) ON DELETE CASCADE,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    skills TEXT[] NOT NULL DEFAULT '{}',
    repos TEXT[] NOT NULL DEFAULT '{}',
    -- Agents ("name") and teams ("team:ID") that may send it tasks; empty = any it is visible to.
    accept_from TEXT[] NOT NULL DEFAULT '{}',
    -- off | metadata | full; empty takes AGENTS_RECORDING.
    recording TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS agent_task (
    id UUID PRIMARY KEY,
    context_id UUID NOT NULL,
    from_agent UUID NOT NULL REFERENCES agent(key_id) ON DELETE CASCADE,
    to_agent UUID NOT NULL REFERENCES agent(key_id) ON DELETE CASCADE,
    -- submitted | working | input-required | completed | failed | canceled
    status TEXT NOT NULL,
    status_reason TEXT NOT NULL DEFAULT '',
    depth INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS agent_task_to_idx ON agent_task (to_agent, status, updated_at);
CREATE INDEX IF NOT EXISTS agent_task_from_idx ON agent_task (from_agent, updated_at);
CREATE INDEX IF NOT EXISTS agent_task_context_idx ON agent_task (context_id);
CREATE INDEX IF NOT EXISTS agent_task_updated_idx ON agent_task (updated_at);

CREATE TABLE IF NOT EXISTS agent_message (
    id BIGSERIAL PRIMARY KEY,
    task_id UUID NOT NULL REFERENCES agent_task(id) ON DELETE CASCADE,
    context_id UUID NOT NULL,
    from_agent UUID NOT NULL,
    role TEXT NOT NULL, -- user (the sender of the task) | agent (its recipient)
    text TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS agent_message_task_idx ON agent_message (task_id, id);
CREATE INDEX IF NOT EXISTS agent_message_context_idx ON agent_message (context_id);
