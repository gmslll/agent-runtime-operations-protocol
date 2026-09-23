CREATE TABLE arop_engine_fixture (
    fixture_id TEXT PRIMARY KEY NOT NULL,
    fixture_value TEXT NOT NULL
);
-- arop:statement
INSERT INTO arop_engine_fixture (fixture_id, fixture_value)
VALUES ('baseline', 'v1');
