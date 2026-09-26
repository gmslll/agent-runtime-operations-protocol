CREATE TABLE arop_runs (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  agent_version TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  input_json TEXT NOT NULL,
  labels_json TEXT NOT NULL,
  conversation_ref TEXT,
  effect_level TEXT NOT NULL,
  effect_id TEXT,
  state TEXT NOT NULL,
  state_version INTEGER NOT NULL,
  authorization_snapshot_json TEXT NOT NULL,
  authorization_snapshot_digest TEXT NOT NULL,
  traceparent TEXT NOT NULL,
  tracestate TEXT,
  deadline_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancel_requested_at TEXT,
  usage_input_tokens INTEGER NOT NULL DEFAULT 0,
  usage_output_tokens INTEGER NOT NULL DEFAULT 0,
  usage_duration_ms INTEGER NOT NULL DEFAULT 0,
  usage_billable_units INTEGER NOT NULL DEFAULT 0,
  CONSTRAINT arop_runs_pkey PRIMARY KEY(tenant_id,run_id),
  CONSTRAINT arop_runs_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]'),
  CONSTRAINT arop_runs_run_check CHECK(length(run_id)=40 AND substr(run_id,1,4)='run_' AND substr(run_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(run_id,13,1)='-' AND substr(run_id,18,2)='-7' AND substr(run_id,24,1) IN ('8','9','a','b')),
  CONSTRAINT arop_runs_input_check CHECK(json_valid(input_json) AND json_type(input_json)='array' AND json_array_length(input_json) BETWEEN 1 AND 256),
  CONSTRAINT arop_runs_labels_check CHECK(json_valid(labels_json) AND json_type(labels_json)='object'),
  CONSTRAINT arop_runs_effect_check CHECK((effect_level IN ('none','read') AND effect_id IS NULL) OR (effect_level IN ('write','irreversible') AND effect_id IS NOT NULL AND substr(effect_id,1,4)='eff_')),
  CONSTRAINT arop_runs_state_check CHECK(state IN ('queued','dispatching','running','waiting_input','cancel_requested','succeeded','failed','cancelled','timed_out')),
  CONSTRAINT arop_runs_version_check CHECK(state_version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_runs_auth_check CHECK(json_valid(authorization_snapshot_json) AND json_type(authorization_snapshot_json)='object' AND length(authorization_snapshot_digest)=71 AND substr(authorization_snapshot_digest,1,7)='sha256:'),
  CONSTRAINT arop_runs_usage_check CHECK(usage_input_tokens BETWEEN 0 AND 9007199254740991 AND usage_output_tokens BETWEEN 0 AND 9007199254740991 AND usage_duration_ms BETWEEN 0 AND 9007199254740991 AND usage_billable_units BETWEEN 0 AND 9007199254740991)
)
-- arop:statement
CREATE INDEX arop_runs_deadline_idx ON arop_runs(tenant_id,state,deadline_at,run_id)
-- arop:statement
CREATE TABLE arop_run_idempotency (
  tenant_id TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  run_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_run_idempotency_pkey PRIMARY KEY(tenant_id,key_digest),
  CONSTRAINT arop_run_idempotency_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_run_idempotency_digest_check CHECK(length(key_digest)=64 AND key_digest NOT GLOB '*[^0-9a-f]*' AND length(request_digest)=71 AND substr(request_digest,1,7)='sha256:')
)
-- arop:statement
CREATE TABLE arop_run_commands (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  command_digest TEXT NOT NULL,
  result_state_version INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_run_commands_pkey PRIMARY KEY(tenant_id,run_id,command_id),
  CONSTRAINT arop_run_commands_key_unique UNIQUE(tenant_id,run_id,key_digest),
  CONSTRAINT arop_run_commands_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_run_commands_version_check CHECK(result_state_version BETWEEN 1 AND 9007199254740991)
)
-- arop:statement
CREATE TABLE arop_run_outbox (
  outbox_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  state_version INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  published_at TEXT,
  CONSTRAINT arop_run_outbox_pkey PRIMARY KEY(outbox_id),
  CONSTRAINT arop_run_outbox_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_run_outbox_payload_check CHECK(json_valid(payload_json) AND json_type(payload_json)='object'),
  CONSTRAINT arop_run_outbox_version_check CHECK(state_version BETWEEN 1 AND 9007199254740991)
)
-- arop:statement
CREATE INDEX arop_run_outbox_unpublished_idx ON arop_run_outbox(published_at,created_at,outbox_id)
-- arop:statement
CREATE TABLE arop_run_effects (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  effect_id TEXT NOT NULL,
  semantic_digest TEXT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_run_effects_pkey PRIMARY KEY(tenant_id,effect_id),
  CONSTRAINT arop_run_effects_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_run_effects_effect_check CHECK(substr(effect_id,1,4)='eff_' AND length(semantic_digest)=71 AND substr(semantic_digest,1,7)='sha256:')
)
-- arop:statement
CREATE TRIGGER arop_run_outbox_no_delete BEFORE DELETE ON arop_run_outbox BEGIN SELECT RAISE(ABORT,'arop_run_outbox is append-only'); END
-- arop:statement
CREATE TRIGGER arop_run_effects_no_update BEFORE UPDATE ON arop_run_effects BEGIN SELECT RAISE(ABORT,'arop_run_effects is immutable'); END
-- arop:statement
CREATE TRIGGER arop_run_effects_no_delete BEFORE DELETE ON arop_run_effects BEGIN SELECT RAISE(ABORT,'arop_run_effects is immutable'); END
