CREATE TABLE arop_registry_meta (
  singleton BIGINT NOT NULL,
  revision BIGINT NOT NULL,
  compaction_watermark BIGINT NOT NULL,
  CONSTRAINT arop_registry_meta_pkey PRIMARY KEY(singleton),
  CONSTRAINT arop_registry_meta_singleton_check CHECK(singleton = 1),
  CONSTRAINT arop_registry_meta_revision_check CHECK(revision BETWEEN 0 AND 9007199254740991),
  CONSTRAINT arop_registry_meta_watermark_check CHECK(compaction_watermark BETWEEN 0 AND revision)
)
-- arop:statement
INSERT INTO arop_registry_meta(singleton, revision, compaction_watermark) VALUES(1, 0, 0)
-- arop:statement
CREATE TABLE arop_registry_instances (
  tenant_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  service_id TEXT NOT NULL,
  environment TEXT NOT NULL,
  generation BIGINT NOT NULL,
  resource_version BIGINT NOT NULL,
  registry_revision BIGINT NOT NULL,
  lease_id TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  heartbeat_sequence BIGINT NOT NULL,
  endpoint_base_url TEXT NOT NULL,
  endpoint_health_path TEXT NOT NULL,
  bindings_json TEXT NOT NULL,
  runtime_json TEXT NOT NULL,
  operator_json TEXT NOT NULL,
  draining BOOLEAN NOT NULL,
  drain_deadline_at TEXT,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_registry_instances_pkey PRIMARY KEY(tenant_id, instance_id),
  CONSTRAINT arop_registry_instances_lease_unique UNIQUE(lease_id),
  CONSTRAINT arop_registry_instances_tenant_check CHECK(tenant_id ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(tenant_id) <= 128),
  CONSTRAINT arop_registry_instances_instance_check CHECK(instance_id ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(instance_id) <= 128),
  CONSTRAINT arop_registry_instances_session_check CHECK(session_id ~ '^ses_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_registry_instances_service_check CHECK(service_id ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(service_id) <= 128),
  CONSTRAINT arop_registry_instances_environment_check CHECK(environment ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(environment) <= 64),
  CONSTRAINT arop_registry_instances_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_resource_check CHECK(resource_version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_revision_check CHECK(registry_revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_lease_check CHECK(lease_id ~ '^lease_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_registry_instances_heartbeat_check CHECK(heartbeat_sequence BETWEEN 0 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_bindings_check CHECK(bindings_json::jsonb IS NOT NULL AND jsonb_typeof(bindings_json::jsonb)='array' AND jsonb_array_length(bindings_json::jsonb) BETWEEN 1 AND 256),
  CONSTRAINT arop_registry_instances_runtime_check CHECK(jsonb_typeof(runtime_json::jsonb)='object'),
  CONSTRAINT arop_registry_instances_operator_check CHECK(jsonb_typeof(operator_json::jsonb)='object'),
  CONSTRAINT arop_registry_instances_status_check CHECK(status IN ('registered','expired','deregistered')),
  CONSTRAINT arop_registry_instances_time_check CHECK(lease_expires_at ~ '^.{19,34}Z$' AND created_at ~ '^.{19,34}Z$' AND updated_at ~ '^.{19,34}Z$' AND (drain_deadline_at IS NULL OR drain_deadline_at ~ '^.{19,34}Z$'))
)
-- arop:statement
CREATE INDEX arop_registry_instances_discovery_idx ON arop_registry_instances(tenant_id, status, lease_expires_at, draining, instance_id)
-- arop:statement
CREATE TABLE arop_registry_sessions (
  tenant_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_registry_sessions_pkey PRIMARY KEY(tenant_id, instance_id, session_id),
  CONSTRAINT arop_registry_sessions_instance_fkey FOREIGN KEY(tenant_id, instance_id) REFERENCES arop_registry_instances(tenant_id, instance_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_registry_sessions_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991)
)
-- arop:statement
CREATE TABLE arop_registry_events (
  revision BIGINT NOT NULL,
  event_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation BIGINT NOT NULL,
  occurred_at TEXT NOT NULL,
  CONSTRAINT arop_registry_events_pkey PRIMARY KEY(revision),
  CONSTRAINT arop_registry_events_event_unique UNIQUE(event_id),
  CONSTRAINT arop_registry_events_revision_check CHECK(revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_events_event_check CHECK(event_id ~ '^evt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_registry_events_type_check CHECK(event_type IN ('registered','keepalive','updated','draining','deregistered','expired')),
  CONSTRAINT arop_registry_events_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991)
)
-- arop:statement
CREATE INDEX arop_registry_events_tenant_revision_idx ON arop_registry_events(tenant_id, revision)
-- arop:statement
CREATE TABLE arop_registry_idempotency (
  tenant_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  result_revision BIGINT NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_registry_idempotency_pkey PRIMARY KEY(tenant_id, operation, key_digest),
  CONSTRAINT arop_registry_idempotency_operation_check CHECK(operation = 'register'),
  CONSTRAINT arop_registry_idempotency_key_check CHECK(key_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_registry_idempotency_request_check CHECK(request_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_registry_idempotency_result_check CHECK(jsonb_typeof(result_json::jsonb)='object'),
  CONSTRAINT arop_registry_idempotency_revision_check CHECK(result_revision BETWEEN 1 AND 9007199254740991)
)
