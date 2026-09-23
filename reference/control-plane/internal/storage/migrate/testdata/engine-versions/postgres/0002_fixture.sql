ALTER TABLE arop_engine_fixture
ADD COLUMN generation BIGINT NOT NULL DEFAULT 1 CHECK (generation >= 1);
-- arop:statement
CREATE INDEX arop_engine_fixture_generation_idx
ON arop_engine_fixture (generation);
