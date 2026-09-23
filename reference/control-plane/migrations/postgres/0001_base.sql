CREATE TABLE arop_observations (
  audit_id TEXT NOT NULL,
  occurred_at_ns BIGINT NOT NULL,
  request_id TEXT NOT NULL,
  trace_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  outcome TEXT NOT NULL,
  http_status INTEGER NOT NULL,
  span_id TEXT NOT NULL,
  parent_span_id TEXT NOT NULL DEFAULT '',
  started_at_ns BIGINT NOT NULL,
  ended_at_ns BIGINT NOT NULL,
  span_status TEXT NOT NULL,
  CONSTRAINT arop_observations_pkey PRIMARY KEY(audit_id),
  CONSTRAINT arop_observations_trace_span_unique UNIQUE(trace_id, span_id),
  CONSTRAINT arop_observations_audit_id_check CHECK(audit_id ~ '^aud_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_observations_request_id_check CHECK(request_id ~ '^req_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
  CONSTRAINT arop_observations_trace_id_check CHECK(trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> '00000000000000000000000000000000'),
  CONSTRAINT arop_observations_span_id_check CHECK(span_id ~ '^[0-9a-f]{16}$' AND span_id <> '0000000000000000'),
  CONSTRAINT arop_observations_parent_span_id_check CHECK(parent_span_id = '' OR (parent_span_id ~ '^[0-9a-f]{16}$' AND parent_span_id <> '0000000000000000')),
  CONSTRAINT arop_observations_operation_check CHECK(operation ~ '^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$' AND length(operation) <= 100),
  CONSTRAINT arop_observations_outcome_check CHECK(outcome IN ('succeeded','rejected','failed')),
  CONSTRAINT arop_observations_span_status_check CHECK(span_status IN ('ok','error')),
  CONSTRAINT arop_observations_http_status_check CHECK(http_status BETWEEN 100 AND 599),
  CONSTRAINT arop_observations_timing_check CHECK(started_at_ns > 0 AND ended_at_ns >= started_at_ns),
  CONSTRAINT arop_observations_occurred_check CHECK(occurred_at_ns = ended_at_ns),
  CONSTRAINT arop_observations_truth_check CHECK((http_status < 400 AND outcome = 'succeeded' AND span_status = 'ok') OR
        (http_status BETWEEN 400 AND 499 AND outcome = 'rejected' AND span_status = 'error') OR
        (http_status >= 500 AND outcome = 'failed' AND span_status = 'error'))
)
-- arop:statement
CREATE INDEX arop_observations_request_idx ON arop_observations(request_id, occurred_at_ns, audit_id)
-- arop:statement
CREATE INDEX arop_observations_trace_idx ON arop_observations(trace_id, occurred_at_ns, audit_id)
-- arop:statement
CREATE INDEX arop_observations_operation_idx ON arop_observations(operation, occurred_at_ns, audit_id)
