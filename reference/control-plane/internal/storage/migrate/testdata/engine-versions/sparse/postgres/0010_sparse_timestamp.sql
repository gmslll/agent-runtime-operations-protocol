ALTER TABLE arop_engine_fixture
ADD COLUMN updated_at_ns BIGINT NOT NULL DEFAULT 0 CHECK (updated_at_ns >= 0);
