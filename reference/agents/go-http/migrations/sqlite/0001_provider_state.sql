CREATE TABLE provider_schema_history (
  version INTEGER PRIMARY KEY CHECK (version = 1),
  checksum TEXT NOT NULL CHECK (length(checksum) = 71 AND checksum GLOB 'sha256:[0-9a-f]*'),
  applied_at TEXT NOT NULL
) STRICT;

CREATE TABLE provider_inbox (
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  authorization_digest TEXT NOT NULL,
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
  tracestate TEXT,
  state TEXT NOT NULL CHECK (state IN ('accepted','running','cancel_requested','succeeded','failed','cancelled','timed_out')),
  deadline_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancel_requested_at TEXT,
  PRIMARY KEY (run_id, attempt_id)
) STRICT;

CREATE INDEX provider_inbox_recovery_idx
  ON provider_inbox (state, deadline_at, updated_at, run_id, attempt_id);

CREATE TABLE provider_effects (
  effect_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  request_digest TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('started','completed')),
  result_json BLOB,
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (run_id, attempt_id) REFERENCES provider_inbox(run_id, attempt_id) ON DELETE RESTRICT,
  CHECK ((state = 'started' AND result_json IS NULL) OR (state = 'completed' AND result_json IS NOT NULL))
) STRICT;

CREATE TABLE provider_outbox (
  run_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence BETWEEN 1 AND 9007199254740991),
  event_id TEXT NOT NULL UNIQUE,
  event_type TEXT NOT NULL,
  envelope_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  delivered_at TEXT,
  PRIMARY KEY (run_id, attempt_id, sequence),
  FOREIGN KEY (run_id, attempt_id) REFERENCES provider_inbox(run_id, attempt_id) ON DELETE RESTRICT
) STRICT;

CREATE INDEX provider_outbox_delivery_idx
  ON provider_outbox (run_id, attempt_id, delivered_at, sequence);
