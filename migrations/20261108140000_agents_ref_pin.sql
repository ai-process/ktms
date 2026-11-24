-- external_ref ties a task to an issue elsewhere (PROJ-123 or a URL). Each side
-- pins for itself, so one side cannot drop the other's record; a task pinned by
-- either side outlives the retention sweep.
ALTER TABLE agent_task ADD COLUMN IF NOT EXISTS external_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_task ADD COLUMN IF NOT EXISTS pinned_by_from BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_task ADD COLUMN IF NOT EXISTS pinned_by_to BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS agent_task_ref_idx ON agent_task (external_ref) WHERE external_ref <> '';
