package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
)

const (
	testTraceSampled   = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	testTraceUnsampled = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
)

func TestStructuredBinaryAndBatchRoundTripPreserveAROPEntity(t *testing.T) {
	event := fixtureEvent(t)
	runSequence := eventwire.PositiveSafeInteger(42)
	event.Runsequence = &runSequence

	structured, err := EncodeStructured(event, "vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	if structured.Headers.Get("Content-Type") != StructuredContentType || structured.Headers.Get("traceparent") != testTraceSampled {
		t.Fatalf("headers=%v", structured.Headers)
	}
	structuredEvent, tracestate, err := DecodeStructured(structured.Headers, structured.Body)
	if err != nil || tracestate != "vendor=value" || !semanticEqual(event, structuredEvent) {
		t.Fatalf("event=%+v tracestate=%q err=%v", structuredEvent, tracestate, err)
	}

	binary, err := EncodeBinary(event, "vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	if binary.Headers.Get("ce-runsequence") != "42" || binary.Headers.Get("ce-producersequence") != "1" || binary.Headers.Get("Content-Type") != JSONContentType {
		t.Fatalf("headers=%v", binary.Headers)
	}
	binaryEvent, tracestate, err := DecodeBinary(binary.Headers, binary.Body)
	if err != nil || tracestate != "vendor=value" || !semanticEqual(event, binaryEvent) {
		t.Fatalf("event=%+v tracestate=%q err=%v", binaryEvent, tracestate, err)
	}

	batch, err := EncodeBatch([]eventwire.EventEnvelope{event, event})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBatch(batch)
	if err != nil || len(decoded) != 2 || !semanticEqual(event, decoded[0]) || !semanticEqual(event, decoded[1]) {
		t.Fatalf("events=%+v err=%v", decoded, err)
	}
	for _, item := range decoded {
		if string(item.ID) != string(event.ID) || int64(item.Producersequence) != 1 || item.Runsequence == nil || int64(*item.Runsequence) != 42 {
			t.Fatalf("AROP authority fields changed: %+v", item)
		}
	}
}

func TestCloudEventsBindingsRejectAmbiguousOrMalformedInputs(t *testing.T) {
	event := fixtureEvent(t)
	binary, err := EncodeBinary(event, "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		headers http.Header
		body    []byte
	}{
		{"duplicate-id", cloneHeaders(binary.Headers), binary.Body},
		{"unknown-ce-header", cloneHeaders(binary.Headers), binary.Body},
		{"trace-mismatch", cloneHeaders(binary.Headers), binary.Body},
		{"duplicate-data-key", cloneHeaders(binary.Headers), []byte(`{"output_id":"answer","output_id":"other","offset":0,"delta":"x"}`)},
		{"multiple-content-type", cloneHeaders(binary.Headers), binary.Body},
	}
	cases[0].headers["Ce-Id"] = append(cases[0].headers["Ce-Id"], "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef99")
	cases[1].headers.Set("ce-secret", "not-allowed")
	cases[2].headers.Set("traceparent", testTraceUnsampled)
	cases[4].headers["Content-Type"] = append(cases[4].headers["Content-Type"], JSONContentType)
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if _, _, err := DecodeBinary(item.headers, item.body); err == nil {
				t.Fatal("invalid binary CloudEvent accepted")
			}
		})
	}

	structured, err := EncodeStructured(event, "")
	if err != nil {
		t.Fatal(err)
	}
	withBinaryHeader := cloneHeaders(structured.Headers)
	withBinaryHeader.Set("ce-id", string(event.ID))
	if _, _, err := DecodeStructured(withBinaryHeader, structured.Body); err == nil {
		t.Fatal("mixed structured and binary modes accepted")
	}
	mismatch := cloneHeaders(structured.Headers)
	mismatch.Set("traceparent", testTraceUnsampled)
	if _, _, err := DecodeStructured(mismatch, structured.Body); err == nil {
		t.Fatal("mismatched traceparent accepted")
	}
	if _, _, err := DecodeStructured(structured.Headers, append(structured.Body, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing structured document accepted")
	}
	for _, invalid := range [][]byte{[]byte(`[] {}`), []byte(`[]`), make([]byte, MaxBatchBytes+1)} {
		if _, err := DecodeBatch(invalid); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	invalidEvent := event
	invalidEvent.Data = nil
	if _, err := EncodeStructured(invalidEvent, ""); err == nil {
		t.Fatal("constructed event with null data accepted")
	}
	if _, err := EncodeBinary(invalidEvent, ""); err == nil {
		t.Fatal("constructed binary event with null data accepted")
	}
	if _, err := EncodeBatch([]eventwire.EventEnvelope{invalidEvent}); err == nil {
		t.Fatal("constructed batch event with null data accepted")
	}
}

func TestOpenTelemetryMappingPropagatesContextSamplesAndRedactsPayload(t *testing.T) {
	event := fixtureEvent(t)
	event.Data["prompt"] = json.RawMessage(`"top-secret-prompt"`)
	event.Data["authorization"] = json.RawMessage(`"Bearer top-secret-token"`)
	observed := time.Date(2026, 9, 28, 3, 4, 5, 0, time.UTC)
	telemetry, err := MapEvent(event, "vendor=value", observed, ExportPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if telemetry.Event == nil || telemetry.Event.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || telemetry.Event.SpanID != "00f067aa0ba902b7" || telemetry.Event.TraceFlags != 1 || telemetry.Event.TraceState != "vendor=value" || telemetry.Event.EventName != TelemetryEventName {
		t.Fatalf("telemetry=%+v", telemetry)
	}
	if len(telemetry.Metrics) != 1 || telemetry.Metrics[0].Attributes["family"] != "output" {
		t.Fatalf("metrics=%+v", telemetry.Metrics)
	}
	wire, err := json.Marshal(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"top-secret-prompt", "top-secret-token", `"prompt"`, `"authorization"`, `"body"`} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("telemetry leaked %q: %s", secret, wire)
		}
	}

	event.Traceparent = eventwire.Traceparent(testTraceUnsampled)
	unsampled, err := MapEvent(event, "", time.Time{}, ExportPolicy{})
	if err != nil || unsampled.Event != nil || len(unsampled.Metrics) != 1 {
		t.Fatalf("unsampled=%+v err=%v", unsampled, err)
	}
	included, err := MapEvent(event, "", time.Time{}, ExportPolicy{IncludeUnsampledTraceEvents: true})
	if err != nil || included.Event == nil || included.Event.TraceFlags != 0 {
		t.Fatalf("included=%+v err=%v", included, err)
	}
}

func TestTerminalMetricUsesOnlyBoundedStatusAndRejectsTypeStateMismatch(t *testing.T) {
	event := fixtureEvent(t)
	event.Type = "io.kinglucky.arop.run.failed.v1"
	event.Dataschema = "https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json"
	event.Data = map[string]json.RawMessage{"state": json.RawMessage(`"failed"`), "error": json.RawMessage(`{"code":"SECRET","message":"password=hidden"}`)}
	telemetry, err := MapEvent(event, "", time.Time{}, ExportPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if len(telemetry.Metrics) != 2 || telemetry.Metrics[1].Name != MetricRunsTerminal || telemetry.Metrics[1].Attributes["status"] != "failed" || telemetry.Event.SeverityNumber != 17 || telemetry.Event.Attributes["error.type"] != "arop.run.failed" {
		t.Fatalf("telemetry=%+v", telemetry)
	}
	wire, _ := json.Marshal(telemetry)
	if strings.Contains(string(wire), "password") || strings.Contains(string(wire), "SECRET") {
		t.Fatalf("terminal telemetry leaked error payload: %s", wire)
	}
	event.Data["state"] = json.RawMessage(`"succeeded"`)
	if _, err := MapEvent(event, "", time.Time{}, ExportPolicy{}); err == nil {
		t.Fatal("terminal type/state mismatch accepted")
	}
	for _, invalid := range []MetricPoint{
		{Name: MetricEventsIngested, Unit: "{event}", Value: 1, Attributes: map[string]string{"family": "run", "run_id": "x"}},
		{Name: MetricEventsIngested, Unit: "{event}", Value: 1, Attributes: map[string]string{"family": "custom"}},
		{Name: MetricRunsTerminal, Unit: "{run}", Value: 1, Attributes: map[string]string{"status": "agent-defined"}},
	} {
		if ValidateMetricPoint(invalid) == nil {
			t.Fatalf("unbounded metric accepted: %+v", invalid)
		}
	}
}

func TestOperationSpanHierarchyIsExplicitAndPayloadFree(t *testing.T) {
	started := time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC)
	input := OperationInput{Operation: OperationInvokeAgent, Traceparent: testTraceSampled, Tracestate: "vendor=value", SpanID: "0123456789abcdef", RunID: "run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10", AttemptID: "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12", DeploymentID: "dep_01932f13-0cd2-7a82-8fa3-1cb5ce13ef13", InstanceID: "runtime-a", StartedAt: started, EndedAt: started.Add(time.Second)}
	span, err := MapOperation(input)
	if err != nil {
		t.Fatal(err)
	}
	if span.Name != "arop.invoke_agent" || span.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || span.ParentSpanID != "00f067aa0ba902b7" || span.SpanID != input.SpanID || !span.Sampled || span.Attributes["arop.run.id"] != input.RunID {
		t.Fatalf("span=%+v", span)
	}
	for _, invalid := range []OperationInput{
		{Operation: OperationInvokeTool, Traceparent: testTraceSampled, SpanID: input.SpanID, RunID: input.RunID, StartedAt: started, EndedAt: started.Add(time.Second)},
		{Operation: OperationInvokeAgent, Traceparent: testTraceSampled, SpanID: strings.Repeat("0", 16), RunID: input.RunID, AttemptID: input.AttemptID, StartedAt: started, EndedAt: started.Add(time.Second)},
		{Operation: "custom", Traceparent: testTraceSampled, SpanID: input.SpanID, RunID: input.RunID, StartedAt: started, EndedAt: started.Add(time.Second)},
		{Operation: OperationCreateRun, Traceparent: testTraceSampled, SpanID: input.SpanID, RunID: input.RunID, StartedAt: started.Add(time.Second), EndedAt: started},
		{Operation: OperationCreateRun, Traceparent: testTraceSampled, SpanID: input.SpanID, RunID: "password=secret", StartedAt: started, EndedAt: started.Add(time.Second)},
	} {
		if _, err := MapOperation(invalid); err == nil {
			t.Fatalf("invalid operation accepted: %+v", invalid)
		}
	}
}

func TestExporterFailureCannotBecomeEventOrAuditAuthority(t *testing.T) {
	telemetry, err := MapEvent(fixtureEvent(t), "", time.Time{}, ExportPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if outcome := Emit(context.Background(), nil, telemetry); outcome != ExportDisabled {
		t.Fatalf("disabled outcome=%s", outcome)
	}
	failing := &captureExporter{err: errors.New("Bearer top-secret exporter failure")}
	if outcome := Emit(context.Background(), failing, telemetry); outcome != ExportFailed {
		t.Fatalf("failed outcome=%s", outcome)
	}
	if failing.value.Event == nil {
		t.Fatal("exporter did not receive derived telemetry")
	}
	failing.value.Event.Attributes["arop.run.id"] = "mutated"
	if telemetry.Event.Attributes["arop.run.id"] == "mutated" {
		t.Fatal("exporter mutated caller telemetry")
	}
	success := &captureExporter{}
	if outcome := Emit(context.Background(), success, telemetry); outcome != ExportSucceeded {
		t.Fatalf("success outcome=%s", outcome)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if outcome := Emit(ctx, success, telemetry); outcome != ExportCancelled {
		t.Fatalf("cancel outcome=%s", outcome)
	}
}

func TestPinnedCompatibilityAndBindingFixtures(t *testing.T) {
	compatibility, err := os.ReadFile("testdata/compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{CloudEventsVersion, CloudEventsCommit, OTelSemConvVersion, OTelSemConvCommit} {
		if !strings.Contains(string(compatibility), pin) {
			t.Fatalf("compatibility pin %q missing", pin)
		}
	}
	structured, err := os.ReadFile("testdata/structured.json")
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventwire.DecodeEventEnvelope(structured)
	if err != nil {
		t.Fatal(err)
	}
	binaryFixture, err := os.ReadFile("testdata/binary.json")
	if err != nil {
		t.Fatal(err)
	}
	var binary struct {
		Headers http.Header     `json:"headers"`
		Body    json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(binaryFixture, &binary); err != nil {
		t.Fatal(err)
	}
	decoded, _, err := DecodeBinary(binary.Headers, binary.Body)
	if err != nil || !semanticEqual(event, decoded) {
		t.Fatalf("binary fixture err=%v event=%+v", err, decoded)
	}
	batch, err := os.ReadFile("testdata/batch.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := DecodeBatch(batch)
	if err != nil || len(items) != 2 {
		t.Fatalf("batch items=%d err=%v", len(items), err)
	}
}

func fixtureEvent(t *testing.T) eventwire.EventEnvelope {
	t.Helper()
	document, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixtures", "events", "envelope.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventwire.DecodeEventEnvelope(document)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func cloneHeaders(source http.Header) http.Header {
	result := make(http.Header, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

type captureExporter struct {
	value Telemetry
	err   error
}

func (exporter *captureExporter) Export(_ context.Context, value Telemetry) error {
	exporter.value = value
	return exporter.err
}
