CREATE TABLE provider_schema_history (
  version INTEGER PRIMARY KEY CHECK (version = 1),
  checksum TEXT NOT NULL CHECK (checksum GLOB 'sha256:[0-9a-f]*' AND length(checksum) = 71),
  applied_at TEXT NOT NULL
) STRICT;

CREATE TABLE provider_inbox (
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  request_digest TEXT NOT NULL CHECK (request_digest GLOB 'sha256:[0-9a-f]*' AND length(request_digest) = 71),
  authorization_digest TEXT NOT NULL CHECK (authorization_digest GLOB 'sha256:[0-9a-f]*' AND length(authorization_digest) = 71),
  request_json BLOB NOT NULL,
  result_json BLOB,
  agent_id TEXT NOT NULL,
  agent_version TEXT NOT NULL,
  skill_id TEXT NOT NULL,
  deployment_id TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  generation INTEGER NOT NULL CHECK (generation BETWEEN 1 AND 9007199254740991),
  fencing_token INTEGER NOT NULL CHECK (fencing_token BETWEEN 1 AND 9007199254740991),
  state_version INTEGER NOT NULL CHECK (state_version BETWEEN 1 AND 9007199254740991),
  traceparent TEXT NOT NULL,
  tracestate TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('accepted','running','cancel_requested','succeeded','failed','cancelled','timed_out')),
  deadline_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancel_requested_at TEXT,
  PRIMARY KEY (run_id, attempt_id),
  CHECK (
    (state IN ('succeeded','failed','cancelled','timed_out') AND result_json IS NOT NULL)
    OR
    (state IN ('accepted','running','cancel_requested') AND result_json IS NULL)
  )
) STRICT, WITHOUT ROWID;

CREATE INDEX provider_inbox_recovery
ON provider_inbox (state, deadline_at, updated_at, run_id, attempt_id);

CREATE TABLE provider_effects (
  effect_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  request_digest TEXT NOT NULL CHECK (request_digest GLOB 'sha256:[0-9a-f]*' AND length(request_digest) = 71),
  state TEXT NOT NULL CHECK (state IN ('started','completed')),
  result BLOB,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (run_id, attempt_id) REFERENCES provider_inbox(run_id, attempt_id) ON DELETE RESTRICT,
  CHECK ((state = 'started' AND result IS NULL) OR (state = 'completed' AND result IS NOT NULL))
) STRICT, WITHOUT ROWID;

CREATE TABLE provider_outbox (
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
  event_id TEXT NOT NULL UNIQUE,
  event_type TEXT NOT NULL,
  envelope BLOB NOT NULL,
  created_at TEXT NOT NULL,
  delivered_at TEXT,
  PRIMARY KEY (run_id, attempt_id, sequence),
  FOREIGN KEY (run_id, attempt_id) REFERENCES provider_inbox(run_id, attempt_id) ON DELETE RESTRICT
) STRICT, WITHOUT ROWID;

CREATE INDEX provider_outbox_delivery
ON provider_outbox (run_id, attempt_id, delivered_at, sequence);

CREATE TABLE worker_completions (
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  claim_id TEXT NOT NULL,
  completion_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  result_json BLOB NOT NULL,
  effect_ids_json BLOB NOT NULL,
  PRIMARY KEY (run_id, attempt_id)
) STRICT, WITHOUT ROWID;
