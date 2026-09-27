CREATE TABLE arop_worker_claims (
  tenant_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  worker_id TEXT NOT NULL,
  worker_session_id TEXT NOT NULL,
  worker_generation INTEGER NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  fencing_token INTEGER NOT NULL,
  lease_token_digest TEXT NOT NULL,
  claim_state TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  claimed_at TEXT NOT NULL,
  renewed_at TEXT,
  closed_at TEXT,
  CONSTRAINT arop_worker_claims_pkey PRIMARY KEY(tenant_id,claim_id),
  CONSTRAINT arop_worker_claims_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_claims_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_worker_claims_worker_check CHECK(length(worker_id) BETWEEN 1 AND 128 AND worker_id NOT GLOB '*[^a-z0-9._-]*' AND substr(worker_id,1,1) GLOB '[a-z]'),
  CONSTRAINT arop_worker_claims_generation_check CHECK(worker_generation BETWEEN 1 AND 9007199254740991 AND fencing_token BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_worker_claims_token_check CHECK(length(lease_token_digest)=71 AND substr(lease_token_digest,1,7)='sha256:' AND substr(lease_token_digest,8) NOT GLOB '*[^0-9a-f]*'),
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
  CONSTRAINT arop_worker_completions_key_check CHECK(length(idempotency_key_digest)=64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_worker_completions_digest_check CHECK(length(request_digest)=71 AND substr(request_digest,1,7)='sha256:' AND length(result_digest)=71 AND substr(result_digest,1,7)='sha256:'),
  CONSTRAINT arop_worker_completions_event_check CHECK(length(terminal_event_id)=40 AND substr(terminal_event_id,1,4)='evt_')
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
  CONSTRAINT arop_worker_outbox_payload_check CHECK(json_valid(payload_json) AND json_type(payload_json)='object')
)
-- arop:statement
CREATE INDEX arop_worker_outbox_unpublished_idx ON arop_worker_outbox(published_at,created_at,outbox_id)
-- arop:statement
CREATE TRIGGER arop_worker_completions_no_update BEFORE UPDATE ON arop_worker_completions BEGIN SELECT RAISE(ABORT,'arop_worker_completions is immutable'); END
-- arop:statement
CREATE TRIGGER arop_worker_completions_no_delete BEFORE DELETE ON arop_worker_completions BEGIN SELECT RAISE(ABORT,'arop_worker_completions is append-only'); END
-- arop:statement
CREATE TRIGGER arop_worker_outbox_no_delete BEFORE DELETE ON arop_worker_outbox BEGIN SELECT RAISE(ABORT,'arop_worker_outbox is append-only'); END
