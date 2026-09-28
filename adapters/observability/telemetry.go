package observability

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

var (
	spanIDPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	uuidV7Pattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	instanceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
)

func MapEvent(event eventwire.EventEnvelope, tracestate string, observedAt time.Time, policy ExportPolicy) (Telemetry, error) {
	if _, err := encodeValidatedEvent(event); err != nil {
		return Telemetry{}, fmt.Errorf("invalid AROP event: %w", err)
	}
	if err := validateTransportTrace(string(event.Traceparent), tracestate); err != nil {
		return Telemetry{}, err
	}
	traceID, spanID, flags, err := parseTraceparent(string(event.Traceparent))
	if err != nil {
		return Telemetry{}, err
	}
	family, err := eventFamily(event.Type)
	if err != nil {
		return Telemetry{}, err
	}
	metrics := []MetricPoint{{Name: MetricEventsIngested, Unit: "{event}", Value: 1, Attributes: map[string]string{"family": family}}}
	terminal, terminalStatus, err := terminalState(event)
	if err != nil {
		return Telemetry{}, err
	}
	if terminal {
		metrics = append(metrics, MetricPoint{Name: MetricRunsTerminal, Unit: "{run}", Value: 1, Attributes: map[string]string{"status": terminalStatus}})
	}
	for _, point := range metrics {
		if err := ValidateMetricPoint(point); err != nil {
			return Telemetry{}, err
		}
	}
	result := Telemetry{Metrics: metrics}
	if flags&1 == 0 && !policy.IncludeUnsampledTraceEvents {
		return result, nil
	}
	attributes := map[string]any{
		"cloudevents.event_id":           string(event.ID),
		"cloudevents.event_source":       string(event.Source),
		"cloudevents.event_spec_version": event.Specversion,
		"cloudevents.event_subject":      event.Subject,
		"cloudevents.event_type":         event.Type,
		"arop.run.id":                    string(event.Runid),
		"arop.attempt.id":                string(event.Attemptid),
		"arop.event.producer_sequence":   int64(event.Producersequence),
	}
	if event.Runsequence != nil {
		attributes["arop.event.run_sequence"] = int64(*event.Runsequence)
	}
	severity := 9
	if terminalStatus == "failed" {
		severity = 17
		attributes["error.type"] = "arop.run.failed"
	} else if terminalStatus == "cancelled" || terminalStatus == "timed_out" {
		severity = 13
	}
	record := &EventRecord{SchemaURL: OTelSchemaURL, EventName: TelemetryEventName, Timestamp: string(event.Time), SeverityNumber: severity, TraceID: traceID, SpanID: spanID, TraceFlags: flags, TraceState: tracestate, Attributes: attributes}
	if !observedAt.IsZero() {
		record.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
	}
	result.Event = record
	return result, nil
}

func MapOperation(input OperationInput) (SpanRecord, error) {
	if err := (protocolcore.TraceContext{Traceparent: input.Traceparent, Tracestate: input.Tracestate}).Validate(); err != nil {
		return SpanRecord{}, fmt.Errorf("invalid operation trace context: %w", err)
	}
	traceID, parentSpanID, flags, err := parseTraceparent(input.Traceparent)
	if err != nil {
		return SpanRecord{}, err
	}
	if !spanIDPattern.MatchString(input.SpanID) || input.SpanID == strings.Repeat("0", 16) {
		return SpanRecord{}, errors.New("operation span ID is invalid")
	}
	if input.StartedAt.IsZero() || input.EndedAt.IsZero() || input.EndedAt.Before(input.StartedAt) {
		return SpanRecord{}, errors.New("operation span interval is invalid")
	}
	requireAttempt := false
	switch input.Operation {
	case OperationCreateRun:
	case OperationProduceAsset:
	case OperationDispatchAttempt, OperationInvokeAgent, OperationInvokeTool, OperationDeliverEvents:
		requireAttempt = true
	default:
		return SpanRecord{}, errors.New("unsupported AROP span operation")
	}
	if input.RunID == "" || (requireAttempt && input.AttemptID == "") {
		return SpanRecord{}, errors.New("operation correlation is incomplete")
	}
	if !prefixedUUID("run_", input.RunID) || (input.AttemptID != "" && !prefixedUUID("att_", input.AttemptID)) || (input.DeploymentID != "" && !prefixedUUID("dep_", input.DeploymentID)) || (input.InstanceID != "" && (len(input.InstanceID) > 128 || !instanceIDPattern.MatchString(input.InstanceID))) {
		return SpanRecord{}, errors.New("operation correlation identifier is invalid")
	}
	attributes := map[string]any{"arop.run.id": input.RunID, "arop.operation": string(input.Operation)}
	if input.AttemptID != "" {
		attributes["arop.attempt.id"] = input.AttemptID
	}
	if input.DeploymentID != "" {
		attributes["arop.deployment.id"] = input.DeploymentID
	}
	if input.InstanceID != "" {
		attributes["arop.instance.id"] = input.InstanceID
	}
	status := "ok"
	if input.Failed {
		status = "error"
	}
	return SpanRecord{SchemaURL: OTelSchemaURL, Name: "arop." + string(input.Operation), TraceID: traceID, SpanID: input.SpanID, ParentSpanID: parentSpanID, TraceFlags: flags, TraceState: input.Tracestate, StartedAt: input.StartedAt.UTC().Format(time.RFC3339Nano), EndedAt: input.EndedAt.UTC().Format(time.RFC3339Nano), Status: status, Sampled: flags&1 != 0, Attributes: attributes}, nil
}

func ValidateMetricPoint(point MetricPoint) error {
	if point.Value < 0 {
		return errors.New("metric value is invalid")
	}
	switch point.Name {
	case MetricEventsIngested:
		if point.Unit != "{event}" || len(point.Attributes) != 1 {
			return errors.New("event metric dimensions are invalid")
		}
		switch point.Attributes["family"] {
		case "run", "output", "progress", "usage":
		default:
			return errors.New("event metric family is unbounded")
		}
	case MetricRunsTerminal:
		if point.Unit != "{run}" || len(point.Attributes) != 1 {
			return errors.New("terminal metric dimensions are invalid")
		}
		switch point.Attributes["status"] {
		case "succeeded", "failed", "cancelled", "timed_out":
		default:
			return errors.New("terminal metric status is unbounded")
		}
	default:
		return errors.New("metric name is not part of the AROP mapping")
	}
	for key := range point.Attributes {
		if strings.Contains(key, "id") || strings.Contains(key, "source") || strings.Contains(key, "type") {
			return errors.New("high-cardinality metric attribute is forbidden")
		}
	}
	return nil
}

func parseTraceparent(value string) (string, string, uint8, error) {
	if err := protocolcore.ValidateTraceParent(value); err != nil {
		return "", "", 0, err
	}
	parts := strings.Split(value, "-")
	if len(parts) < 4 {
		return "", "", 0, errors.New("traceparent is incomplete")
	}
	flags, err := strconv.ParseUint(parts[3], 16, 8)
	if err != nil {
		return "", "", 0, errors.New("trace flags are invalid")
	}
	if _, err := hex.DecodeString(parts[1] + parts[2]); err != nil {
		return "", "", 0, errors.New("trace identifiers are invalid")
	}
	return parts[1], parts[2], uint8(flags), nil
}

func eventFamily(value string) (string, error) {
	const prefix = "io.kinglucky.arop."
	if !strings.HasPrefix(value, prefix) {
		return "", errors.New("event type is outside the AROP namespace")
	}
	family := strings.Split(strings.TrimPrefix(value, prefix), ".")[0]
	switch family {
	case "run", "output", "progress", "usage":
		return family, nil
	default:
		return "", errors.New("event type has unsupported family")
	}
}

func terminalState(event eventwire.EventEnvelope) (bool, string, error) {
	prefix := "io.kinglucky.arop.run."
	if !strings.HasPrefix(event.Type, prefix) {
		return false, "", nil
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(event.Type, prefix), ".v1")
	switch suffix {
	case "succeeded", "failed", "cancelled", "timed_out":
		var raw map[string]json.RawMessage
		encoded, err := json.Marshal(event.Data)
		if err != nil || protocolcore.DecodeAuthoring(encoded, &raw) != nil {
			return false, "", errors.New("terminal event data is invalid")
		}
		var state string
		if err := json.Unmarshal(raw["state"], &state); err != nil || state != suffix {
			return false, "", errors.New("terminal event type and state differ")
		}
		return true, suffix, nil
	default:
		return false, "", nil
	}
}

func prefixedUUID(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidV7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}
