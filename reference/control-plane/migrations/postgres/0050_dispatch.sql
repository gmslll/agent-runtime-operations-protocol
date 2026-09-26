CREATE TABLE arop_dispatch_deployments (
  tenant_id TEXT NOT NULL,
  service_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_dispatch_deployments_pkey PRIMARY KEY(tenant_id,service_id),
  CONSTRAINT arop_dispatch_deployments_id_unique UNIQUE(deployment_id),
  CONSTRAINT arop_dispatch_deployments_tenant_check CHECK(tenant_id COLLATE "C" ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(tenant_id)<=128),
  CONSTRAINT arop_dispatch_deployments_service_check CHECK(service_id COLLATE "C" ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(service_id)<=128),
  CONSTRAINT arop_dispatch_deployments_id_check CHECK(deployment_id COLLATE "C" ~ '^dep_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$')
)
-- arop:statement
CREATE TABLE arop_dispatch_keys (
  key_id TEXT NOT NULL,
  key_status TEXT NOT NULL,
  public_x TEXT NOT NULL,
  public_y TEXT NOT NULL,
  not_before TEXT NOT NULL,
  sign_until TEXT NOT NULL,
  verify_until TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_dispatch_keys_pkey PRIMARY KEY(key_id),
  CONSTRAINT arop_dispatch_keys_id_check CHECK(key_id COLLATE "C" ~ '^[A-Za-z0-9._-]{8,128}$'),
  CONSTRAINT arop_dispatch_keys_status_check CHECK(key_status IN ('active','retiring')),
  CONSTRAINT arop_dispatch_keys_public_check CHECK(public_x COLLATE "C" ~ '^[A-Za-z0-9_-]{43}$' AND public_y COLLATE "C" ~ '^[A-Za-z0-9_-]{43}$')
)
-- arop:statement
CREATE UNIQUE INDEX arop_dispatch_keys_one_active_idx ON arop_dispatch_keys(key_status) WHERE key_status='active'
-- arop:statement
CREATE TABLE arop_dispatch_attempts (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  attempt_number BIGINT NOT NULL,
  fencing_token BIGINT NOT NULL,
  token_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  service_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  registry_resource_version BIGINT NOT NULL,
  attempt_state TEXT NOT NULL,
  transport_profile TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  audience TEXT NOT NULL,
  signing_key_id TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  ticket_expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  accepted_at TEXT,
  closed_at TEXT,
  failure_code TEXT,
  traceparent TEXT NOT NULL,
  tracestate TEXT,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  CONSTRAINT arop_dispatch_attempts_pkey PRIMARY KEY(tenant_id,attempt_id),
  CONSTRAINT arop_dispatch_attempts_run_ordinal_unique UNIQUE(tenant_id,run_id,attempt_number),
  CONSTRAINT arop_dispatch_attempts_run_fence_unique UNIQUE(tenant_id,run_id,fencing_token),
  CONSTRAINT arop_dispatch_attempts_token_unique UNIQUE(token_id),
  CONSTRAINT arop_dispatch_attempts_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_dispatch_attempts_deployment_fkey FOREIGN KEY(deployment_id) REFERENCES arop_dispatch_deployments(deployment_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_dispatch_attempts_key_fkey FOREIGN KEY(signing_key_id) REFERENCES arop_dispatch_keys(key_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_dispatch_attempts_number_check CHECK(attempt_number BETWEEN 1 AND 9007199254740991 AND fencing_token BETWEEN 1 AND 9007199254740991 AND generation BETWEEN 1 AND 9007199254740991 AND registry_resource_version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_dispatch_attempts_state_check CHECK(attempt_state IN ('issued','accepted','failed','expired','fenced','cancelled')),
  CONSTRAINT arop_dispatch_attempts_transport_check CHECK(transport_profile IN ('direct','proxy','worker_pull')),
  CONSTRAINT arop_dispatch_attempts_endpoint_check CHECK(endpoint COLLATE "C" ~ '^https://[^?#]+/v1/runs$' AND length(endpoint)<=2048),
  CONSTRAINT arop_dispatch_attempts_audience_check CHECK(audience COLLATE "C" ~ '^https://[^?#]+$' AND length(audience)<=2048),
  CONSTRAINT arop_dispatch_attempts_digest_check CHECK(idempotency_key_digest COLLATE "C" ~ '^[0-9a-f]{64}$' AND idempotency_request_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$')
)
-- arop:statement
CREATE UNIQUE INDEX arop_dispatch_attempts_one_active_run_idx ON arop_dispatch_attempts(tenant_id,run_id) WHERE attempt_state IN ('issued','accepted')
-- arop:statement
CREATE INDEX arop_dispatch_attempts_capacity_idx ON arop_dispatch_attempts(tenant_id,instance_id,session_id,generation,registry_resource_version,attempt_state,lease_expires_at)
-- arop:statement
CREATE TABLE arop_dispatch_idempotency (
  tenant_id TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_dispatch_idempotency_pkey PRIMARY KEY(tenant_id,key_digest),
  CONSTRAINT arop_dispatch_idempotency_attempt_fkey FOREIGN KEY(tenant_id,attempt_id) REFERENCES arop_dispatch_attempts(tenant_id,attempt_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_dispatch_idempotency_digest_check CHECK(key_digest COLLATE "C" ~ '^[0-9a-f]{64}$' AND request_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$')
)
-- arop:statement
CREATE FUNCTION arop_dispatch_attempts_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_dispatch_attempts is append-only'; END; $$
-- arop:statement
CREATE TRIGGER arop_dispatch_attempts_no_delete BEFORE DELETE ON arop_dispatch_attempts FOR EACH ROW EXECUTE FUNCTION arop_dispatch_attempts_no_delete()
-- arop:statement
CREATE FUNCTION arop_dispatch_keys_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_dispatch_keys is retained for ticket verification'; END; $$
-- arop:statement
CREATE TRIGGER arop_dispatch_keys_no_delete BEFORE DELETE ON arop_dispatch_keys FOR EACH ROW EXECUTE FUNCTION arop_dispatch_keys_no_delete()
