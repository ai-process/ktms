-- Shadow evaluation: automated Judge quality comparison for mirrored traffic.
ALTER TABLE rule ADD COLUMN IF NOT EXISTS shadow_eval BOOLEAN NOT NULL DEFAULT false;
