CREATE TABLE arop_publications (
  tenant_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  version TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  bundle_semantic_digest TEXT NOT NULL,
  canonical_manifest BYTEA NOT NULL,
  publisher_principal_id TEXT NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  published_at_ns BIGINT NOT NULL,
  revision BIGINT NOT NULL,
  CONSTRAINT arop_publications_pkey PRIMARY KEY(tenant_id, agent_id, version),
  CONSTRAINT arop_publications_idempotency_unique UNIQUE(tenant_id, idempotency_key_digest),
  CONSTRAINT arop_publications_tenant_check CHECK(tenant_id ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(tenant_id) <= 128),
  CONSTRAINT arop_publications_agent_check CHECK(agent_id ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(agent_id) <= 200),
  CONSTRAINT arop_publications_version_check CHECK(length(version) BETWEEN 5 AND 200),
  CONSTRAINT arop_publications_manifest_digest_check CHECK(manifest_digest ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_publications_bundle_digest_check CHECK(bundle_semantic_digest ~ '^sha256:[0-9a-f]{64}$'),
  CONSTRAINT arop_publications_manifest_check CHECK(octet_length(canonical_manifest) BETWEEN 2 AND 10485760),
  CONSTRAINT arop_publications_publisher_check CHECK(length(publisher_principal_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_publications_idempotency_key_check CHECK(idempotency_key_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_publications_idempotency_request_check CHECK(idempotency_request_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_publications_published_check CHECK(published_at_ns > 0),
  CONSTRAINT arop_publications_revision_check CHECK(revision = 1)
)
-- arop:statement
CREATE INDEX arop_publications_tenant_published_idx ON arop_publications(tenant_id, published_at_ns, agent_id, version)
-- arop:statement
CREATE INDEX arop_publications_manifest_digest_idx ON arop_publications(tenant_id, manifest_digest)
