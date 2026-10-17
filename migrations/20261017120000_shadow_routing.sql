-- Shadow routing: async request mirroring to a secondary model.
ALTER TABLE rule ADD COLUMN IF NOT EXISTS shadow_model TEXT NOT NULL DEFAULT '';
ALTER TABLE rule ADD COLUMN IF NOT EXISTS shadow_percentage DOUBLE PRECISION NOT NULL DEFAULT 0.0;
