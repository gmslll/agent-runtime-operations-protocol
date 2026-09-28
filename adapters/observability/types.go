// Package observability maps authoritative AROP events to CloudEvents HTTP
// bindings and bounded OpenTelemetry records. The mapped telemetry is derived
// output: it never replaces the Event ledger, Run state, or durable Audit log.
package observability

import (
	"net/http"
	"time"
)

const (
	CloudEventsVersion      = "1.0.2"
	CloudEventsCommit       = "fc1f6f31f5f011a72183f1bcea20c987cb683ade"
	OTelSemConvVersion      = "1.44.0"
	OTelSemConvCommit       = "e10a930844c6951757a43b849d364f7d056ac32b"
	OTelSchemaURL           = "https://opentelemetry.io/schemas/1.44.0"
	StructuredContentType   = "application/cloudevents+json"
	BatchContentType        = "application/cloudevents-batch+json"
	JSONContentType         = "application/json"
	MaxBatchEvents          = 100
	MaxBatchBytes           = 262144
	MaxStructuredEventBytes = 4 << 20
	TelemetryEventName      = "arop.event"
	MetricEventsIngested    = "arop.events_ingested_total"
	MetricRunsTerminal      = "runs_terminal_total"
)

type HTTPMessage struct {
	Headers http.Header
	Body    []byte
}

type ExportPolicy struct {
	// IncludeUnsampledTraceEvents is intentionally opt-in. Metrics remain
	// available for unsampled traces and never gain high-cardinality labels.
	IncludeUnsampledTraceEvents bool
}

type EventRecord struct {
	SchemaURL      string         `json:"schema_url"`
	EventName      string         `json:"event_name"`
	Timestamp      string         `json:"timestamp"`
	ObservedAt     string         `json:"observed_at,omitempty"`
	SeverityNumber int            `json:"severity_number"`
	TraceID        string         `json:"trace_id"`
	SpanID         string         `json:"span_id"`
	TraceFlags     uint8          `json:"trace_flags"`
	TraceState     string         `json:"trace_state,omitempty"`
	Attributes     map[string]any `json:"attributes"`
}

type MetricPoint struct {
	Name       string            `json:"name"`
	Unit       string            `json:"unit"`
	Value      int64             `json:"value"`
	Attributes map[string]string `json:"attributes"`
}

type Telemetry struct {
	Event   *EventRecord  `json:"event,omitempty"`
	Metrics []MetricPoint `json:"metrics"`
}

type Operation string

const (
	OperationCreateRun       Operation = "create_run"
	OperationDispatchAttempt Operation = "dispatch_attempt"
	OperationInvokeAgent     Operation = "invoke_agent"
	OperationInvokeTool      Operation = "invoke_tool"
	OperationProduceAsset    Operation = "produce_asset"
	OperationDeliverEvents   Operation = "deliver_events"
)

type OperationInput struct {
	Operation    Operation
	Traceparent  string
	Tracestate   string
	SpanID       string
	RunID        string
	AttemptID    string
	DeploymentID string
	InstanceID   string
	StartedAt    time.Time
	EndedAt      time.Time
	Failed       bool
}

type SpanRecord struct {
	SchemaURL    string         `json:"schema_url"`
	Name         string         `json:"name"`
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	TraceFlags   uint8          `json:"trace_flags"`
	TraceState   string         `json:"trace_state,omitempty"`
	StartedAt    string         `json:"started_at"`
	EndedAt      string         `json:"ended_at"`
	Status       string         `json:"status"`
	Sampled      bool           `json:"sampled"`
	Attributes   map[string]any `json:"attributes"`
}
