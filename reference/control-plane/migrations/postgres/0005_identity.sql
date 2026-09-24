CREATE TABLE arop_dev_principals (
  principal_id TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at_ns BIGINT NOT NULL,
  updated_at_ns BIGINT NOT NULL,
  revision BIGINT NOT NULL,
  CONSTRAINT arop_dev_principals_pkey PRIMARY KEY(principal_id),
  CONSTRAINT arop_dev_principals_subject_unique UNIQUE(subject_id),
  CONSTRAINT arop_dev_principals_principal_id_check CHECK(length(principal_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_dev_principals_subject_id_check CHECK(length(subject_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_dev_principals_status_check CHECK(status IN ('active','disabled')),
  CONSTRAINT arop_dev_principals_created_check CHECK(created_at_ns > 0),
  CONSTRAINT arop_dev_principals_updated_check CHECK(updated_at_ns >= created_at_ns),
  CONSTRAINT arop_dev_principals_revision_check CHECK(revision > 0)
)
-- arop:statement
CREATE TABLE arop_credentials (
  credential_id TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  credential_kind TEXT NOT NULL,
  audience TEXT NOT NULL,
  scope_canonical TEXT NOT NULL,
  secret_verifier TEXT NOT NULL,
  issued_at_ns BIGINT NOT NULL,
  not_before_at_ns BIGINT NOT NULL,
  expires_at_ns BIGINT NOT NULL,
  status TEXT NOT NULL,
  revoked_at_ns BIGINT,
  replaced_at_ns BIGINT,
  replacement_credential_id TEXT,
  revision BIGINT NOT NULL,
  idempotency_key_digest TEXT NOT NULL,
  idempotency_request_digest TEXT NOT NULL,
  CONSTRAINT arop_credentials_pkey PRIMARY KEY(credential_id),
  CONSTRAINT arop_credentials_principal_fkey FOREIGN KEY(principal_id) REFERENCES arop_dev_principals(principal_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_credentials_replacement_fkey FOREIGN KEY(replacement_credential_id) REFERENCES arop_credentials(credential_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT arop_credentials_verifier_unique UNIQUE(secret_verifier),
  CONSTRAINT arop_credentials_idempotency_unique UNIQUE(idempotency_key_digest),
  CONSTRAINT arop_credentials_replacement_unique UNIQUE(replacement_credential_id),
  CONSTRAINT arop_credentials_id_check CHECK(length(credential_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_credentials_kind_check CHECK(length(credential_kind) BETWEEN 1 AND 100),
  CONSTRAINT arop_credentials_audience_check CHECK(length(audience) BETWEEN 1 AND 500),
  CONSTRAINT arop_credentials_scope_check CHECK(length(scope_canonical) BETWEEN 1 AND 4096),
  CONSTRAINT arop_credentials_verifier_check CHECK(secret_verifier ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_credentials_idempotency_check CHECK(idempotency_key_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_credentials_idempotency_request_check CHECK(idempotency_request_digest ~ '^[0-9a-f]{64}$'),
  CONSTRAINT arop_credentials_time_check CHECK(issued_at_ns > 0 AND not_before_at_ns >= issued_at_ns AND expires_at_ns > not_before_at_ns),
  CONSTRAINT arop_credentials_status_check CHECK(status IN ('active','revoked','replaced')),
  CONSTRAINT arop_credentials_lifecycle_check CHECK(
    (status = 'active' AND revoked_at_ns IS NULL AND replaced_at_ns IS NULL AND replacement_credential_id IS NULL) OR
    (status = 'revoked' AND revoked_at_ns IS NOT NULL AND revoked_at_ns >= issued_at_ns AND replaced_at_ns IS NULL AND replacement_credential_id IS NULL) OR
    (status = 'replaced' AND revoked_at_ns IS NULL AND replaced_at_ns IS NOT NULL AND replaced_at_ns >= issued_at_ns AND replacement_credential_id IS NOT NULL AND replacement_credential_id <> credential_id)
  ),
  CONSTRAINT arop_credentials_revision_check CHECK(revision > 0)
)
-- arop:statement
CREATE INDEX arop_credentials_principal_status_expiry_idx ON arop_credentials(principal_id, status, expires_at_ns)
-- arop:statement
CREATE INDEX arop_credentials_audience_status_expiry_idx ON arop_credentials(audience, status, expires_at_ns)
