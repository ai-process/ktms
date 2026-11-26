-- Competencies of a model and needs of a rule (1.7): topic -> level
-- (basic | proficient | expert). Efforts stay until 2.0 and are read as
-- general: an empty map here keeps every older row as it routed before.
ALTER TABLE model ADD COLUMN IF NOT EXISTS competencies jsonb NOT NULL DEFAULT '{}';
ALTER TABLE rule ADD COLUMN IF NOT EXISTS needs jsonb NOT NULL DEFAULT '{}';
