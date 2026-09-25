CREATE TABLE arop_publications (
  tenant_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  version TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  bundle_semantic_digest TEXT NOT NULL,
  canonical_manifest BLOB NOT NULL,
  publisher_principal_id TEXT NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  published_at_ns INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  CONSTRAINT arop_publications_pkey PRIMARY KEY(tenant_id, agent_id, version),
  CONSTRAINT arop_publications_idempotency_unique UNIQUE(tenant_id, idempotency_key_digest),
  CONSTRAINT arop_publications_tenant_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_publications_agent_check CHECK(length(agent_id) BETWEEN 1 AND 200 AND agent_id NOT GLOB '*[^a-z0-9._-]*' AND substr(agent_id,1,1) GLOB '[a-z]' AND substr(agent_id,-1,1) GLOB '[a-z0-9]' AND agent_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_publications_version_check CHECK(length(version) BETWEEN 5 AND 200),
  CONSTRAINT arop_publications_manifest_digest_check CHECK(length(manifest_digest) = 71 AND substr(manifest_digest,1,7) = 'sha256:' AND substr(manifest_digest,8) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_publications_bundle_digest_check CHECK(length(bundle_semantic_digest) = 71 AND substr(bundle_semantic_digest,1,7) = 'sha256:' AND substr(bundle_semantic_digest,8) NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_publications_manifest_check CHECK(typeof(canonical_manifest) = 'blob' AND length(canonical_manifest) BETWEEN 2 AND 10485760),
  CONSTRAINT arop_publications_publisher_check CHECK(length(publisher_principal_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_publications_idempotency_key_check CHECK(length(idempotency_key_digest) = 64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_publications_idempotency_request_check CHECK(length(idempotency_request_digest) = 64 AND idempotency_request_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_publications_published_check CHECK(published_at_ns > 0),
  CONSTRAINT arop_publications_revision_check CHECK(revision = 1)
)
-- arop:statement
CREATE INDEX arop_publications_tenant_published_idx ON arop_publications(tenant_id, published_at_ns, agent_id, version)
-- arop:statement
CREATE INDEX arop_publications_manifest_digest_idx ON arop_publications(tenant_id, manifest_digest)
