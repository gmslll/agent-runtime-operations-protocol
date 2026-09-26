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
  state_version BIGINT NOT NULL,
  authorization_snapshot_json TEXT NOT NULL,
  authorization_snapshot_digest TEXT NOT NULL,
  traceparent TEXT NOT NULL,
  tracestate TEXT,
  deadline_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancel_requested_at TEXT,
  usage_input_tokens BIGINT NOT NULL DEFAULT 0,
  usage_output_tokens BIGINT NOT NULL DEFAULT 0,
  usage_duration_ms BIGINT NOT NULL DEFAULT 0,
  usage_billable_units BIGINT NOT NULL DEFAULT 0,
  CONSTRAINT arop_runs_pkey PRIMARY KEY(tenant_id,run_id),
  CONSTRAINT arop_runs_tenant_check CHECK(tenant_id COLLATE "C" ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(tenant_id)<=128),
  CONSTRAINT arop_runs_run_check CHECK(run_id COLLATE "C" ~ '^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_runs_input_check CHECK(jsonb_typeof(input_json::jsonb)='array' AND jsonb_array_length(input_json::jsonb) BETWEEN 1 AND 256),
  CONSTRAINT arop_runs_labels_check CHECK(jsonb_typeof(labels_json::jsonb)='object'),
  CONSTRAINT arop_runs_effect_check CHECK((effect_level IN ('none','read') AND effect_id IS NULL) OR (effect_level IN ('write','irreversible') AND effect_id COLLATE "C" ~ '^eff_[A-Za-z0-9._:-]+$')),
  CONSTRAINT arop_runs_state_check CHECK(state IN ('queued','dispatching','running','waiting_input','cancel_requested','succeeded','failed','cancelled','timed_out')),
  CONSTRAINT arop_runs_version_check CHECK(state_version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_runs_auth_check CHECK(jsonb_typeof(authorization_snapshot_json::jsonb)='object' AND authorization_snapshot_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$'),
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
  CONSTRAINT arop_run_idempotency_digest_check CHECK(key_digest COLLATE "C" ~ '^[0-9a-f]{64}$' AND request_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$')
)
-- arop:statement
CREATE TABLE arop_run_commands (
  tenant_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  command_digest TEXT NOT NULL,
  result_state_version BIGINT NOT NULL,
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
  state_version BIGINT NOT NULL,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  published_at TEXT,
  CONSTRAINT arop_run_outbox_pkey PRIMARY KEY(outbox_id),
  CONSTRAINT arop_run_outbox_run_fkey FOREIGN KEY(tenant_id,run_id) REFERENCES arop_runs(tenant_id,run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_run_outbox_payload_check CHECK(jsonb_typeof(payload_json::jsonb)='object'),
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
  CONSTRAINT arop_run_effects_effect_check CHECK(effect_id COLLATE "C" ~ '^eff_[A-Za-z0-9._:-]+$' AND semantic_digest COLLATE "C" ~ '^sha256:[0-9a-f]{64}$')
)
-- arop:statement
CREATE FUNCTION arop_run_outbox_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_run_outbox is append-only'; END; $$
-- arop:statement
CREATE TRIGGER arop_run_outbox_no_delete BEFORE DELETE ON arop_run_outbox FOR EACH ROW EXECUTE FUNCTION arop_run_outbox_no_delete()
-- arop:statement
CREATE FUNCTION arop_run_effects_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'arop_run_effects is immutable'; END; $$
-- arop:statement
CREATE TRIGGER arop_run_effects_no_update BEFORE UPDATE ON arop_run_effects FOR EACH ROW EXECUTE FUNCTION arop_run_effects_immutable()
-- arop:statement
CREATE TRIGGER arop_run_effects_no_delete BEFORE DELETE ON arop_run_effects FOR EACH ROW EXECUTE FUNCTION arop_run_effects_immutable()
