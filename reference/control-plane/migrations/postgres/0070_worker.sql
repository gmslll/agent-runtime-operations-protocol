CREATE TABLE arop_worker_claims (
  tenant_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  worker_id TEXT NOT NULL,
  worker_session_id TEXT NOT NULL,
  worker_generation BIGINT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  fencing_token BIGINT NOT NULL,
  lease_token_digest TEXT NOT NULL,
  claim_state TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  claimed_at TEXT NOT NULL,
  renewed_at TEXT,
  closed_at TEXT,
  CONSTRAINT arop_worker_claims_pkey PRIMARY KEY(tenant_id,claim_id),
  CONSTRAINT arop_worker_claims_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_claims_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_claims_worker_check CHECK(worker_id COLLATE "C" ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(worker_id) BETWEEN 1 AND 128),
  CONSTRAINT arop_worker_claims_generation_check CHECK(worker_generation BETWEEN 1 AND 9007199254740991 AND fencing_token BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_worker_claims_token_check CHECK(lease_token_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_worker_claims_state_check CHECK(claim_state IN ('active','completed','released','expired')),
  CONSTRAINT arop_worker_claims_closed_check CHECK((claim_state='active' AND closed_at IS NULL) OR (claim_state<>'active' AND closed_at IS NOT NULL))
)
-- arop:statement
CREATE UNIQUE INDEX arop_worker_claims_active_attempt_idx ON arop_worker_claims(tenant_id,attempt_id) WHERE claim_state='active'
-- arop:statement
CREATE INDEX arop_worker_claims_lease_idx ON arop_worker_claims(tenant_id,worker_id,claim_state,lease_expires_at,claim_id)
-- arop:statement
CREATE TABLE arop_worker_completions (
  tenant_id TEXT NOT NULL,
  completion_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_digest TEXT NOT NULL,
  terminal_event_id TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_worker_completions_pkey PRIMARY KEY(tenant_id,completion_id),
  CONSTRAINT arop_worker_completions_idempotency_unique UNIQUE(tenant_id,idempotency_key_digest),
  CONSTRAINT arop_worker_completions_claim_fkey FOREIGN KEY(tenant_id,claim_id) REFERENCES arop_worker_claims(tenant_id,claim_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_completions_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_completions_key_check CHECK(idempotency_key_digest COLLATE "C" ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_worker_completions_digest_check CHECK(request_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$' AND result_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_worker_completions_event_check CHECK(terminal_event_id COLLATE "C" ~ '^evt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
)
-- arop:statement
CREATE TABLE arop_worker_outbox (
  tenant_id TEXT NOT NULL,
  outbox_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  published_at TEXT,
  CONSTRAINT arop_worker_outbox_pkey PRIMARY KEY(tenant_id,outbox_id),
  CONSTRAINT arop_worker_outbox_claim_fkey FOREIGN KEY(tenant_id,claim_id) REFERENCES arop_worker_claims(tenant_id,claim_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_outbox_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_outbox_kind_check CHECK(event_kind IN ('worker.claimed','worker.renewed','worker.completed','worker.released','worker.expired')),
  CONSTRAINT arop_worker_outbox_payload_check CHECK(jsonb_typeof(payload_json::jsonb)='object')
)
-- arop:statement
CREATE INDEX arop_worker_outbox_unpublished_idx ON arop_worker_outbox(published_at,created_at,outbox_id)
-- arop:statement
CREATE FUNCTION arop_worker_completions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_worker_completions is immutable'; END; $$
-- arop:statement
CREATE TRIGGER arop_worker_completions_no_update BEFORE UPDATE ON arop_worker_completions FOR EACH ROW EXECUTE FUNCTION arop_worker_completions_immutable()
-- arop:statement
CREATE TRIGGER arop_worker_completions_no_delete BEFORE DELETE ON arop_worker_completions FOR EACH ROW EXECUTE FUNCTION arop_worker_completions_immutable()
-- arop:statement
CREATE FUNCTION arop_worker_outbox_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_worker_outbox is append-only'; END; $$
-- arop:statement
CREATE TRIGGER arop_worker_outbox_no_delete BEFORE DELETE ON arop_worker_outbox FOR EACH ROW EXECUTE FUNCTION arop_worker_outbox_no_delete()
