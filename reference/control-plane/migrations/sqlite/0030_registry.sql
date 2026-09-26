CREATE TABLE arop_registry_meta (
  singleton INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  compaction_watermark INTEGER NOT NULL,
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
  generation INTEGER NOT NULL,
  resource_version INTEGER NOT NULL,
  registry_revision INTEGER NOT NULL,
  lease_id TEXT NOT NULL,
  lease_expires_at TEXT NOT NULL,
  heartbeat_sequence INTEGER NOT NULL,
  endpoint_base_url TEXT NOT NULL,
  endpoint_health_path TEXT NOT NULL,
  bindings_json TEXT NOT NULL,
  runtime_json TEXT NOT NULL,
  operator_json TEXT NOT NULL,
  draining INTEGER NOT NULL,
  drain_deadline_at TEXT,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CONSTRAINT arop_registry_instances_pkey PRIMARY KEY(tenant_id, instance_id),
  CONSTRAINT arop_registry_instances_lease_unique UNIQUE(lease_id),
  CONSTRAINT arop_registry_instances_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_instances_instance_check CHECK(length(instance_id) BETWEEN 1 AND 128 AND instance_id NOT GLOB '*[^a-z0-9._-]*' AND substr(instance_id,1,1) GLOB '[a-z]' AND substr(instance_id,-1,1) GLOB '[a-z0-9]' AND instance_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_instances_session_check CHECK(length(session_id) = 40 AND substr(session_id,1,4) = 'ses_' AND substr(session_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,13,1)='-' AND substr(session_id,14,4) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,18,2)='-7' AND substr(session_id,20,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,23,1)='-' AND substr(session_id,24,1) IN ('8','9','a','b') AND substr(session_id,25,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,28,1)='-' AND substr(session_id,29,12) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_instances_service_check CHECK(length(service_id) BETWEEN 1 AND 128 AND service_id NOT GLOB '*[^a-z0-9._-]*' AND substr(service_id,1,1) GLOB '[a-z]' AND substr(service_id,-1,1) GLOB '[a-z0-9]' AND service_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_instances_environment_check CHECK(length(environment) BETWEEN 1 AND 64 AND environment NOT GLOB '*[^a-z0-9._-]*' AND substr(environment,1,1) GLOB '[a-z]' AND substr(environment,-1,1) GLOB '[a-z0-9]' AND environment NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_instances_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_resource_check CHECK(resource_version BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_revision_check CHECK(registry_revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_lease_check CHECK(length(lease_id) = 42 AND substr(lease_id,1,6) = 'lease_' AND substr(lease_id,7,8) NOT GLOB '*[^0-9a-f]*' AND substr(lease_id,15,1)='-' AND substr(lease_id,16,4) NOT GLOB '*[^0-9a-f]*' AND substr(lease_id,20,2)='-7' AND substr(lease_id,22,3) NOT GLOB '*[^0-9a-f]*' AND substr(lease_id,25,1)='-' AND substr(lease_id,26,1) IN ('8','9','a','b') AND substr(lease_id,27,3) NOT GLOB '*[^0-9a-f]*' AND substr(lease_id,30,1)='-' AND substr(lease_id,31,12) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_instances_heartbeat_check CHECK(heartbeat_sequence BETWEEN 0 AND 9007199254740991),
  CONSTRAINT arop_registry_instances_endpoint_check CHECK(length(endpoint_base_url) BETWEEN 9 AND 2048 AND substr(endpoint_base_url,1,8)='https://' AND endpoint_base_url NOT GLOB '*[?#]*' AND length(endpoint_health_path) BETWEEN 1 AND 512 AND substr(endpoint_health_path,1,1)='/' AND endpoint_health_path NOT GLOB '*[?#]*' AND instr(endpoint_health_path,'..')=0),
  CONSTRAINT arop_registry_instances_bindings_check CHECK(json_valid(bindings_json) AND json_type(bindings_json) = 'array' AND json_array_length(bindings_json) BETWEEN 1 AND 256),
  CONSTRAINT arop_registry_instances_runtime_check CHECK(json_valid(runtime_json) AND json_type(runtime_json) = 'object'),
  CONSTRAINT arop_registry_instances_operator_check CHECK(json_valid(operator_json) AND json_type(operator_json) = 'object'),
  CONSTRAINT arop_registry_instances_draining_check CHECK(draining IN (0,1)),
  CONSTRAINT arop_registry_instances_status_check CHECK(status IN ('registered','expired','deregistered')),
  CONSTRAINT arop_registry_instances_time_check CHECK(length(lease_expires_at)=30 AND substr(lease_expires_at,5,1)='-' AND substr(lease_expires_at,8,1)='-' AND substr(lease_expires_at,11,1)='T' AND substr(lease_expires_at,14,1)=':' AND substr(lease_expires_at,17,1)=':' AND substr(lease_expires_at,20,1)='.' AND substr(lease_expires_at,30,1)='Z' AND lease_expires_at NOT GLOB '*[^0-9TZ:.-]*' AND length(created_at)=30 AND substr(created_at,5,1)='-' AND substr(created_at,8,1)='-' AND substr(created_at,11,1)='T' AND substr(created_at,14,1)=':' AND substr(created_at,17,1)=':' AND substr(created_at,20,1)='.' AND substr(created_at,30,1)='Z' AND created_at NOT GLOB '*[^0-9TZ:.-]*' AND length(updated_at)=30 AND substr(updated_at,5,1)='-' AND substr(updated_at,8,1)='-' AND substr(updated_at,11,1)='T' AND substr(updated_at,14,1)=':' AND substr(updated_at,17,1)=':' AND substr(updated_at,20,1)='.' AND substr(updated_at,30,1)='Z' AND updated_at NOT GLOB '*[^0-9TZ:.-]*' AND (drain_deadline_at IS NULL OR length(drain_deadline_at)=30 AND substr(drain_deadline_at,5,1)='-' AND substr(drain_deadline_at,8,1)='-' AND substr(drain_deadline_at,11,1)='T' AND substr(drain_deadline_at,14,1)=':' AND substr(drain_deadline_at,17,1)=':' AND substr(drain_deadline_at,20,1)='.' AND substr(drain_deadline_at,30,1)='Z' AND drain_deadline_at NOT GLOB '*[^0-9TZ:.-]*'))
)
-- arop:statement
CREATE INDEX arop_registry_instances_discovery_idx ON arop_registry_instances(tenant_id, status, lease_expires_at, draining, instance_id)
-- arop:statement
CREATE TABLE arop_registry_sessions (
  tenant_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_registry_sessions_pkey PRIMARY KEY(tenant_id, instance_id, session_id),
  CONSTRAINT arop_registry_sessions_instance_fkey FOREIGN KEY(tenant_id, instance_id) REFERENCES arop_registry_instances(tenant_id, instance_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_registry_sessions_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_sessions_instance_check CHECK(length(instance_id) BETWEEN 1 AND 128 AND instance_id NOT GLOB '*[^a-z0-9._-]*' AND substr(instance_id,1,1) GLOB '[a-z]' AND substr(instance_id,-1,1) GLOB '[a-z0-9]' AND instance_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_sessions_session_check CHECK(length(session_id) = 40 AND substr(session_id,1,4) = 'ses_' AND substr(session_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,13,1)='-' AND substr(session_id,14,4) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,18,2)='-7' AND substr(session_id,20,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,23,1)='-' AND substr(session_id,24,1) IN ('8','9','a','b') AND substr(session_id,25,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,28,1)='-' AND substr(session_id,29,12) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_sessions_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_sessions_time_check CHECK(length(created_at)=30 AND substr(created_at,5,1)='-' AND substr(created_at,8,1)='-' AND substr(created_at,11,1)='T' AND substr(created_at,14,1)=':' AND substr(created_at,17,1)=':' AND substr(created_at,20,1)='.' AND substr(created_at,30,1)='Z' AND created_at NOT GLOB '*[^0-9TZ:.-]*')
)
-- arop:statement
CREATE TABLE arop_registry_events (
  revision INTEGER NOT NULL,
  event_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  occurred_at TEXT NOT NULL,
  CONSTRAINT arop_registry_events_pkey PRIMARY KEY(revision),
  CONSTRAINT arop_registry_events_event_unique UNIQUE(event_id),
  CONSTRAINT arop_registry_events_revision_check CHECK(revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_events_event_check CHECK(length(event_id) = 40 AND substr(event_id,1,4) = 'evt_' AND substr(event_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(event_id,13,1)='-' AND substr(event_id,14,4) NOT GLOB '*[^0-9a-f]*' AND substr(event_id,18,2)='-7' AND substr(event_id,20,3) NOT GLOB '*[^0-9a-f]*' AND substr(event_id,23,1)='-' AND substr(event_id,24,1) IN ('8','9','a','b') AND substr(event_id,25,3) NOT GLOB '*[^0-9a-f]*' AND substr(event_id,28,1)='-' AND substr(event_id,29,12) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_events_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_events_type_check CHECK(event_type IN ('registered','keepalive','updated','draining','deregistered','expired')),
  CONSTRAINT arop_registry_events_instance_check CHECK(length(instance_id) BETWEEN 1 AND 128 AND instance_id NOT GLOB '*[^a-z0-9._-]*' AND substr(instance_id,1,1) GLOB '[a-z]' AND substr(instance_id,-1,1) GLOB '[a-z0-9]' AND instance_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_events_session_check CHECK(length(session_id) = 40 AND substr(session_id,1,4) = 'ses_' AND substr(session_id,5,8) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,13,1)='-' AND substr(session_id,14,4) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,18,2)='-7' AND substr(session_id,20,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,23,1)='-' AND substr(session_id,24,1) IN ('8','9','a','b') AND substr(session_id,25,3) NOT GLOB '*[^0-9a-f]*' AND substr(session_id,28,1)='-' AND substr(session_id,29,12) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_events_generation_check CHECK(generation BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_events_time_check CHECK(length(occurred_at)=30 AND substr(occurred_at,5,1)='-' AND substr(occurred_at,8,1)='-' AND substr(occurred_at,11,1)='T' AND substr(occurred_at,14,1)=':' AND substr(occurred_at,17,1)=':' AND substr(occurred_at,20,1)='.' AND substr(occurred_at,30,1)='Z' AND occurred_at NOT GLOB '*[^0-9TZ:.-]*')
)
-- arop:statement
CREATE INDEX arop_registry_events_tenant_revision_idx ON arop_registry_events(tenant_id, revision)
-- arop:statement
CREATE TRIGGER arop_registry_events_no_update BEFORE UPDATE ON arop_registry_events BEGIN SELECT RAISE(ABORT, 'arop_registry_events is append-only'); END
-- arop:statement
CREATE TRIGGER arop_registry_events_no_delete BEFORE DELETE ON arop_registry_events BEGIN SELECT RAISE(ABORT, 'arop_registry_events is append-only'); END
-- arop:statement
CREATE TABLE arop_registry_idempotency (
  tenant_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  key_digest TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  result_json TEXT NOT NULL,
  result_revision INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  CONSTRAINT arop_registry_idempotency_pkey PRIMARY KEY(tenant_id, operation, key_digest),
  CONSTRAINT arop_registry_idempotency_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_registry_idempotency_operation_check CHECK(operation = 'register'),
  CONSTRAINT arop_registry_idempotency_key_check CHECK(length(key_digest)=64 AND key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_idempotency_request_check CHECK(length(request_digest)=64 AND request_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_registry_idempotency_result_check CHECK(json_valid(result_json) AND json_type(result_json)='object'),
  CONSTRAINT arop_registry_idempotency_revision_check CHECK(result_revision BETWEEN 1 AND 9007199254740991),
  CONSTRAINT arop_registry_idempotency_time_check CHECK(length(created_at)=30 AND substr(created_at,5,1)='-' AND substr(created_at,8,1)='-' AND substr(created_at,11,1)='T' AND substr(created_at,14,1)=':' AND substr(created_at,17,1)=':' AND substr(created_at,20,1)='.' AND substr(created_at,30,1)='Z' AND created_at NOT GLOB '*[^0-9TZ:.-]*')
)
