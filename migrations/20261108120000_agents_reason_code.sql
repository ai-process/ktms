-- Why a task waits: question (the recipient asks its sender), guardrail_hold
-- (a guardrail holds it for the recipient's person), needs_owner (the
-- recipient's agent asks its own person). Empty when nothing waits.
ALTER TABLE agent_task ADD COLUMN IF NOT EXISTS reason_code TEXT NOT NULL DEFAULT '';

UPDATE agent_task SET reason_code = CASE WHEN status_reason LIKE 'guardrail: %' THEN 'guardrail_hold' ELSE 'question' END
 WHERE status = 'input-required' AND reason_code = '';

-- since: tasks of an agent changed after a moment.
CREATE INDEX IF NOT EXISTS agent_task_from_updated_idx ON agent_task (from_agent, updated_at);
