CREATE TABLE arop_dispatch_deployments (
  tenant_id TEXT NOT NULL,
  service_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_dispatch_deployments_pkey PRIMARY KEY(tenant_id,service_id),
  CONSTRAINT arop_dispatch_deployments_id_unique UNIQUE(deployment_id),
  CONSTRAINT arop_dispatch_deployments_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]'),
  CONSTRAINT arop_dispatch_deployments_service_check CHECK(length(service_id) BETWEEN 1 AND 128 AND service_id NOT GLOB '*[^a-z0-9._-]*' AND substr(service_id,1,1) GLOB '[a-z]'),
  CONSTRAINT arop_dispatch_deployments_id_check CHECK(length(deployment_id)=40 AND substr(deployment_id,1,4)='dep_' AND substr(deployment_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(deployment_id,13,1)='-' AND substr(deployment_id,18,2)='-7' AND substr(deployment_id,24,1) IN ('8','9','a','b'))
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
  CONSTRAINT arop_dispatch_keys_id_check CHECK(length(key_id) BETWEEN 8 AND 128 AND key_id NOT GLOB '*[^A-Za-z0-9._-]*'),
  CONSTRAINT arop_dispatch_keys_status_check CHECK(key_status IN ('active','retiring')),
  CONSTRAINT arop_dispatch_keys_public_check CHECK(length(public_x)=43 AND public_x NOT GLOB '*[^A-Za-z0-9_-]*' AND length(public_y)=43 AND public_y NOT GLOB '*[^A-Za-z0-9_-]*')
)
-- arop:statement
CREATE UNIQUE INDEX arop_dispatch_keys_one_active_idx ON arop_dispatch_keys(key_status) WHERE key_status='active'
-- arop:statement
CREATE TABLE arop_dispatch_attempts (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  attempt_number INTEGER NOT NULL,
  fencing_token INTEGER NOT NULL,
  token_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  service_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  registry_resource_version INTEGER NOT NULL,
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
  CONSTRAINT arop_dispatch_attempts_endpoint_check CHECK(length(endpoint) BETWEEN 17 AND 2048 AND substr(endpoint,1,8)='https://' AND endpoint NOT GLOB '*[?#]*' AND substr(endpoint,-8)='/v1/runs'),
  CONSTRAINT arop_dispatch_attempts_audience_check CHECK(length(audience) BETWEEN 17 AND 2048 AND substr(audience,1,8)='https://' AND audience NOT GLOB '*[?#]*'),
  CONSTRAINT arop_dispatch_attempts_digest_check CHECK(length(idempotency_key_digest)=64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*' AND length(idempotency_request_digest)=71 AND substr(idempotency_request_digest,1,7)='sha256:')
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
  CONSTRAINT arop_dispatch_idempotency_digest_check CHECK(length(key_digest)=64 AND key_digest NOT GLOB '*[^0-9a-f]*' AND length(request_digest)=71 AND substr(request_digest,1,7)='sha256:')
)
-- arop:statement
CREATE TRIGGER arop_dispatch_attempts_no_delete BEFORE DELETE ON arop_dispatch_attempts BEGIN SELECT RAISE(ABORT,'arop_dispatch_attempts is append-only'); END
-- arop:statement
CREATE TRIGGER arop_dispatch_keys_no_delete BEFORE DELETE ON arop_dispatch_keys BEGIN SELECT RAISE(ABORT,'arop_dispatch_keys is retained for ticket verification'); END
