CREATE TABLE arop_assets (
  tenant_id TEXT NOT NULL,
  asset_id TEXT NOT NULL,
  name TEXT NOT NULL,
  media_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  content_digest TEXT NOT NULL,
  object_key TEXT NOT NULL,
  status TEXT NOT NULL,
  revision INTEGER NOT NULL,
  created_at_ns INTEGER NOT NULL,
  updated_at_ns INTEGER NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  CONSTRAINT arop_assets_pkey PRIMARY KEY(tenant_id, asset_id),
  CONSTRAINT arop_assets_idempotency_unique UNIQUE(tenant_id, idempotency_key_digest),
  CONSTRAINT arop_assets_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_assets_asset_id_check CHECK(length(asset_id) = 42 AND substr(asset_id,1,6) = 'asset_' AND substr(asset_id,7) NOT GLOB '*[^0-9a-f-]*' AND substr(asset_id,15,1) = '-' AND substr(asset_id,20,1) = '-' AND substr(asset_id,25,1) = '-' AND substr(asset_id,30,1) = '-'),
  CONSTRAINT arop_assets_name_check CHECK(length(name) BETWEEN 1 AND 512 AND name NOT GLOB '*..*'),
  CONSTRAINT arop_assets_media_type_check CHECK(length(media_type) BETWEEN 3 AND 127 AND media_type NOT GLOB '*[^a-z0-9!#$&^_.+/-]*' AND media_type GLOB '*/*' AND media_type NOT GLOB '*/*/*'),
  CONSTRAINT arop_assets_size_check CHECK(size_bytes >= 0 AND size_bytes <= 104857600),
  CONSTRAINT arop_assets_digest_check CHECK(length(content_digest) = 71 AND substr(content_digest,1,7) = 'sha256:' AND substr(content_digest,8) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_assets_object_key_check CHECK(length(object_key) BETWEEN 1 AND 256 AND object_key NOT GLOB '*[^a-z0-9._/-]*' AND substr(object_key,1,1) GLOB '[a-z0-9]' AND object_key NOT GLOB '*..*' AND object_key NOT GLOB '/*'),
  CONSTRAINT arop_assets_status_check CHECK(status IN ('pending','available','isolated')),
  CONSTRAINT arop_assets_revision_check CHECK(revision > 0),
  CONSTRAINT arop_assets_created_check CHECK(created_at_ns > 0),
  CONSTRAINT arop_assets_updated_check CHECK(updated_at_ns >= created_at_ns),
  CONSTRAINT arop_assets_idempotency_key_check CHECK(length(idempotency_key_digest) = 64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_assets_idempotency_request_check CHECK(length(idempotency_request_digest) = 64 AND idempotency_request_digest NOT GLOB '*[^0-9a-f]*')
)
-- arop:statement
CREATE INDEX arop_assets_tenant_status_idx ON arop_assets(tenant_id, status, asset_id)
-- arop:statement
CREATE INDEX arop_assets_tenant_digest_idx ON arop_assets(tenant_id, content_digest)
-- arop:statement
CREATE TABLE arop_asset_grants (
  tenant_id TEXT NOT NULL,
  grant_id TEXT NOT NULL,
  asset_id TEXT NOT NULL,
  run_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  token_digest TEXT NOT NULL,
  use_limit INTEGER NOT NULL,
  use_count INTEGER NOT NULL,
  expires_at_ns INTEGER NOT NULL,
  status TEXT NOT NULL,
  revision INTEGER NOT NULL,
  created_at_ns INTEGER NOT NULL,
  updated_at_ns INTEGER NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  CONSTRAINT arop_asset_grants_pkey PRIMARY KEY(tenant_id, grant_id),
  CONSTRAINT arop_asset_grants_idempotency_unique UNIQUE(tenant_id, idempotency_key_digest),
  CONSTRAINT arop_asset_grants_token_unique UNIQUE(tenant_id, token_digest),
  CONSTRAINT arop_asset_grants_asset_fkey FOREIGN KEY(tenant_id, asset_id) REFERENCES arop_assets(tenant_id, asset_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_asset_grants_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_asset_grants_grant_id_check CHECK(length(grant_id) = 41 AND substr(grant_id,1,5) = 'grnt_' AND substr(grant_id,6) NOT GLOB '*[^0-9a-f-]*' AND substr(grant_id,14,1) = '-' AND substr(grant_id,19,1) = '-' AND substr(grant_id,24,1) = '-' AND substr(grant_id,29,1) = '-'),
  CONSTRAINT arop_asset_grants_asset_id_check CHECK(length(asset_id) = 42 AND substr(asset_id,1,6) = 'asset_' AND substr(asset_id,7) NOT GLOB '*[^0-9a-f-]*'),
  CONSTRAINT arop_asset_grants_run_id_check CHECK(length(run_id) = 40 AND substr(run_id,1,4) = 'run_' AND substr(run_id,5) NOT GLOB '*[^0-9a-f-]*' AND substr(run_id,13,1) = '-' AND substr(run_id,18,1) = '-' AND substr(run_id,23,1) = '-' AND substr(run_id,28,1) = '-'),
  CONSTRAINT arop_asset_grants_operation_check CHECK(operation IN ('upload','download')),
  CONSTRAINT arop_asset_grants_token_check CHECK(length(token_digest) = 64 AND token_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_asset_grants_use_limit_check CHECK(use_limit BETWEEN 1 AND 1000),
  CONSTRAINT arop_asset_grants_use_count_check CHECK(use_count >= 0 AND use_count <= use_limit),
  CONSTRAINT arop_asset_grants_active_check CHECK(status != 'active' OR use_count < use_limit),
  CONSTRAINT arop_asset_grants_consumed_check CHECK(status != 'consumed' OR use_count = use_limit),
  CONSTRAINT arop_asset_grants_expires_check CHECK(expires_at_ns > 0),
  CONSTRAINT arop_asset_grants_status_check CHECK(status IN ('active','consumed','revoked')),
  CONSTRAINT arop_asset_grants_revision_check CHECK(revision > 0),
  CONSTRAINT arop_asset_grants_created_check CHECK(created_at_ns > 0),
  CONSTRAINT arop_asset_grants_updated_check CHECK(updated_at_ns >= created_at_ns),
  CONSTRAINT arop_asset_grants_idempotency_key_check CHECK(length(idempotency_key_digest) = 64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_asset_grants_idempotency_request_check CHECK(length(idempotency_request_digest) = 64 AND idempotency_request_digest NOT GLOB '*[^0-9a-f]*')
)
-- arop:statement
CREATE INDEX arop_asset_grants_tenant_asset_idx ON arop_asset_grants(tenant_id, asset_id, status)
-- arop:statement
CREATE INDEX arop_asset_grants_tenant_run_idx ON arop_asset_grants(tenant_id, run_id, status)
