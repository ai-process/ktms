-- Who wrote a message on its agent's side: the agent itself or its owner (a
-- person answering through the console). A correction amends an answer after
-- the task ended; the task keeps its status.
ALTER TABLE agent_message ADD COLUMN IF NOT EXISTS author TEXT NOT NULL DEFAULT 'agent';
ALTER TABLE agent_message ADD COLUMN IF NOT EXISTS correction BOOLEAN NOT NULL DEFAULT false;
