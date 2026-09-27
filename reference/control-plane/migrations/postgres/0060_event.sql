CREATE TABLE arop_event_sessions (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  runtime_session_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  fencing_token BIGINT NOT NULL,
  token_digest TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_event_sessions_pkey PRIMARY KEY(tenant_id,token_digest),
  CONSTRAINT arop_event_sessions_token_unique UNIQUE(token_digest),
  CONSTRAINT arop_event_sessions_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_sessions_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991 AND fencing_token BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_event_sessions_digest_check CHECK(token_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$')
)
-- arop:statement
CREATE INDEX arop_event_sessions_scope_idx ON arop_event_sessions(tenant_id,run_id,attempt_id,expires_at)
-- arop:statement
CREATE TABLE arop_event_attempt_projections (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  last_producer_sequence BIGINT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_event_attempt_projections_pkey PRIMARY KEY(tenant_id,attempt_id),
  CONSTRAINT arop_event_attempt_projections_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_attempt_projections_sequence_check CHECK(last_producer_sequence BETWEEN 0 AND 9007199254740991)
)
-- arop:statement
CREATE TABLE arop_event_run_projections (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  last_run_sequence BIGINT NOT NULL,
  terminal_state TEXT,
  terminal_result_json TEXT,
  terminal_event_id TEXT,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_event_run_projections_pkey PRIMARY KEY(tenant_id,run_id),
  CONSTRAINT arop_event_run_projections_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_run_projections_sequence_check CHECK(last_run_sequence BETWEEN 0 AND 9007199254740991),
  CONSTRAINT arop_event_run_projections_terminal_check CHECK((terminal_state IS NULL AND terminal_result_json IS NULL AND terminal_event_id IS NULL) OR (terminal_state IN ('succeeded','failed','cancelled','timed_out') AND jsonb_typeof(terminal_result_json::jsonb)='object' AND terminal_event_id COLLATE "C" ~ '^evt_[0-9a-f-]+$'))
)
-- arop:statement
CREATE TABLE arop_event_ledger (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_sequence BIGINT NOT NULL,
  attempt_id TEXT NOT NULL,
  producer_sequence BIGINT NOT NULL,
  event_id TEXT NOT NULL,
  source TEXT NOT NULL,
  event_type TEXT NOT NULL,
  event_digest TEXT NOT NULL,
  envelope_json TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  projection_applied BOOLEAN NOT NULL,
  CONSTRAINT arop_event_ledger_pkey PRIMARY KEY(tenant_id,run_id,run_sequence),
  CONSTRAINT arop_event_ledger_source_id_unique UNIQUE(tenant_id,source,event_id),
  CONSTRAINT arop_event_ledger_attempt_sequence_unique UNIQUE(tenant_id,attempt_id,producer_sequence),
  CONSTRAINT arop_event_ledger_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_ledger_sequence_check CHECK(run_sequence BETWEEN 1 AND 9007199254740991 AND producer_sequence BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_event_ledger_digest_check CHECK(event_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_event_ledger_envelope_check CHECK(jsonb_typeof(envelope_json::jsonb)='object')
)
-- arop:statement
CREATE INDEX arop_event_ledger_replay_idx ON arop_event_ledger(tenant_id,run_id,run_sequence)
-- arop:statement
CREATE TABLE arop_event_batches (
  tenant_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  batch_id TEXT NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  accepted_through_producer_sequence BIGINT NOT NULL,
  assigned_run_sequence BIGINT NOT NULL,
  duplicate_event_ids_json TEXT NOT NULL,
  run_state TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_event_batches_pkey PRIMARY KEY(tenant_id,attempt_id,batch_id),
  CONSTRAINT arop_event_batches_idempotency_unique UNIQUE(tenant_id,attempt_id,idempotency_key_digest),
  CONSTRAINT arop_event_batches_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_batches_digest_check CHECK(idempotency_key_digest COLLATE "C" ~ '^[0-9a-f]{64}$' AND request_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_event_batches_sequence_check CHECK(accepted_through_producer_sequence BETWEEN 1 AND 9007199254740991 AND assigned_run_sequence BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_event_batches_duplicate_check CHECK(jsonb_typeof(duplicate_event_ids_json::jsonb)='array')
)
-- arop:statement
CREATE TABLE arop_event_capacity_releases (
  tenant_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_sequence BIGINT NOT NULL,
  terminal_event_id TEXT NOT NULL,
  released_at TEXT NOT NULL,
  CONSTRAINT arop_event_capacity_releases_pkey PRIMARY KEY(tenant_id,attempt_id),
  CONSTRAINT arop_event_capacity_releases_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_event_capacity_releases_event_fkey FOREIGN KEY(tenant_id,run_id,run_sequence) REFERENCES arop_event_ledger(tenant_id,run_id,run_sequence) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT arop_event_capacity_releases_sequence_check CHECK(run_sequence BETWEEN 1 AND 9007199254740991)
)
-- arop:statement
CREATE FUNCTION arop_event_ledger_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_event_ledger is immutable'; END; $$
-- arop:statement
CREATE TRIGGER arop_event_ledger_no_update BEFORE UPDATE ON arop_event_ledger FOR EACH ROW EXECUTE FUNCTION arop_event_ledger_immutable()
-- arop:statement
CREATE TRIGGER arop_event_ledger_no_delete BEFORE DELETE ON arop_event_ledger FOR EACH ROW EXECUTE FUNCTION arop_event_ledger_immutable()
-- arop:statement
CREATE FUNCTION arop_event_capacity_releases_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_event_capacity_releases is immutable'; END; $$
-- arop:statement
CREATE TRIGGER arop_event_capacity_releases_no_update BEFORE UPDATE ON arop_event_capacity_releases FOR EACH ROW EXECUTE FUNCTION arop_event_capacity_releases_immutable()
-- arop:statement
CREATE TRIGGER arop_event_capacity_releases_no_delete BEFORE DELETE ON arop_event_capacity_releases FOR EACH ROW EXECUTE FUNCTION arop_event_capacity_releases_immutable()
