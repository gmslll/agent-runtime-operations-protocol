package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

var binaryAttributes = []string{"specversion", "id", "source", "type", "subject", "time", "dataschema", "runid", "attemptid", "producersequence", "runsequence", "traceparent"}

func EncodeStructured(event eventwire.EventEnvelope, tracestate string) (HTTPMessage, error) {
	body, err := encodeValidatedEvent(event)
	if err != nil {
		return HTTPMessage{}, fmt.Errorf("encode AROP event: %w", err)
	}
	if len(body) > MaxStructuredEventBytes {
		return HTTPMessage{}, errors.New("CloudEvent exceeds structured event limit")
	}
	if err := validateTransportTrace(string(event.Traceparent), tracestate); err != nil {
		return HTTPMessage{}, err
	}
	headers := make(http.Header)
	headers.Set("Content-Type", StructuredContentType)
	headers.Set("traceparent", string(event.Traceparent))
	if tracestate != "" {
		headers.Set("tracestate", tracestate)
	}
	return HTTPMessage{Headers: headers, Body: body}, nil
}

func DecodeStructured(headers http.Header, body []byte) (eventwire.EventEnvelope, string, error) {
	if len(body) == 0 || len(body) > MaxStructuredEventBytes {
		return eventwire.EventEnvelope{}, "", errors.New("invalid structured CloudEvent size")
	}
	contentType, err := singleHeader(headers, "content-type", true)
	if err != nil || contentType != StructuredContentType {
		return eventwire.EventEnvelope{}, "", errors.New("structured CloudEvent content type is invalid")
	}
	if err := rejectCEHeaders(headers); err != nil {
		return eventwire.EventEnvelope{}, "", err
	}
	event, err := eventwire.DecodeEventEnvelope(body)
	if err != nil {
		return eventwire.EventEnvelope{}, "", fmt.Errorf("decode structured CloudEvent: %w", err)
	}
	traceparent, err := singleHeader(headers, "traceparent", true)
	if err != nil || traceparent != string(event.Traceparent) {
		return eventwire.EventEnvelope{}, "", errors.New("HTTP and CloudEvent traceparent differ")
	}
	tracestate, err := singleHeader(headers, "tracestate", false)
	if err != nil {
		return eventwire.EventEnvelope{}, "", err
	}
	if err := validateTransportTrace(traceparent, tracestate); err != nil {
		return eventwire.EventEnvelope{}, "", err
	}
	return event, tracestate, nil
}

func EncodeBinary(event eventwire.EventEnvelope, tracestate string) (HTTPMessage, error) {
	if _, err := encodeValidatedEvent(event); err != nil {
		return HTTPMessage{}, fmt.Errorf("encode AROP event: %w", err)
	}
	if err := validateTransportTrace(string(event.Traceparent), tracestate); err != nil {
		return HTTPMessage{}, err
	}
	body, err := json.Marshal(event.Data)
	if err != nil {
		return HTTPMessage{}, errors.New("encode CloudEvent data")
	}
	if len(body) == 0 || len(body) > MaxStructuredEventBytes {
		return HTTPMessage{}, errors.New("CloudEvent data exceeds binary event limit")
	}
	headers := make(http.Header)
	headers.Set("Content-Type", JSONContentType)
	values := map[string]string{
		"specversion":      event.Specversion,
		"id":               string(event.ID),
		"source":           string(event.Source),
		"type":             event.Type,
		"subject":          event.Subject,
		"time":             string(event.Time),
		"dataschema":       string(event.Dataschema),
		"runid":            string(event.Runid),
		"attemptid":        string(event.Attemptid),
		"producersequence": strconv.FormatInt(int64(event.Producersequence), 10),
		"traceparent":      string(event.Traceparent),
	}
	if event.Runsequence != nil {
		values["runsequence"] = strconv.FormatInt(int64(*event.Runsequence), 10)
	}
	for key, value := range values {
		headers.Set("ce-"+key, value)
	}
	headers.Set("traceparent", string(event.Traceparent))
	if tracestate != "" {
		headers.Set("tracestate", tracestate)
	}
	return HTTPMessage{Headers: headers, Body: body}, nil
}

func DecodeBinary(headers http.Header, body []byte) (eventwire.EventEnvelope, string, error) {
	if len(body) == 0 || len(body) > MaxStructuredEventBytes {
		return eventwire.EventEnvelope{}, "", errors.New("invalid binary CloudEvent data size")
	}
	contentType, err := singleHeader(headers, "content-type", true)
	if err != nil || contentType != JSONContentType {
		return eventwire.EventEnvelope{}, "", errors.New("binary CloudEvent content type is invalid")
	}
	allowed := map[string]bool{}
	for _, name := range binaryAttributes {
		allowed["ce-"+name] = true
	}
	for key := range headers {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "ce-") && !allowed[lower] {
			return eventwire.EventEnvelope{}, "", fmt.Errorf("unsupported CloudEvent attribute header %s", lower)
		}
	}
	values := map[string]string{}
	for _, name := range binaryAttributes {
		value, err := singleHeader(headers, "ce-"+name, name != "runsequence")
		if err != nil {
			return eventwire.EventEnvelope{}, "", err
		}
		if value != "" {
			values[name] = value
		}
	}
	traceparent, err := singleHeader(headers, "traceparent", true)
	if err != nil || traceparent != values["traceparent"] {
		return eventwire.EventEnvelope{}, "", errors.New("HTTP and CloudEvent traceparent differ")
	}
	tracestate, err := singleHeader(headers, "tracestate", false)
	if err != nil {
		return eventwire.EventEnvelope{}, "", err
	}
	if err := validateTransportTrace(traceparent, tracestate); err != nil {
		return eventwire.EventEnvelope{}, "", err
	}
	producerSequence, err := strconv.ParseUint(values["producersequence"], 10, 64)
	if err != nil {
		return eventwire.EventEnvelope{}, "", errors.New("producersequence is invalid")
	}
	object := map[string]json.RawMessage{}
	for _, name := range []string{"specversion", "id", "source", "type", "subject", "time", "dataschema", "runid", "attemptid", "traceparent"} {
		object[name], _ = json.Marshal(values[name])
	}
	object["datacontenttype"], _ = json.Marshal(JSONContentType)
	object["producersequence"], _ = json.Marshal(producerSequence)
	object["data"] = append(json.RawMessage(nil), body...)
	if encoded := values["runsequence"]; encoded != "" {
		runSequence, err := strconv.ParseUint(encoded, 10, 64)
		if err != nil {
			return eventwire.EventEnvelope{}, "", errors.New("runsequence is invalid")
		}
		object["runsequence"], _ = json.Marshal(runSequence)
	}
	document, err := json.Marshal(object)
	if err != nil {
		return eventwire.EventEnvelope{}, "", errors.New("assemble binary CloudEvent")
	}
	event, err := eventwire.DecodeEventEnvelope(document)
	if err != nil {
		return eventwire.EventEnvelope{}, "", fmt.Errorf("decode binary CloudEvent: %w", err)
	}
	return event, tracestate, nil
}

func EncodeBatch(events []eventwire.EventEnvelope) ([]byte, error) {
	if len(events) == 0 || len(events) > MaxBatchEvents {
		return nil, errors.New("CloudEvents batch size is invalid")
	}
	items := make([]json.RawMessage, len(events))
	for index, event := range events {
		encoded, err := encodeValidatedEvent(event)
		if err != nil {
			return nil, fmt.Errorf("encode CloudEvent batch item %d: %w", index, err)
		}
		items[index] = encoded
	}
	document, err := json.Marshal(items)
	if err != nil {
		return nil, errors.New("encode CloudEvents batch")
	}
	if len(document) > MaxBatchBytes {
		return nil, errors.New("CloudEvents batch exceeds byte limit")
	}
	return document, nil
}

func DecodeBatch(document []byte) ([]eventwire.EventEnvelope, error) {
	if len(document) == 0 || len(document) > MaxBatchBytes {
		return nil, errors.New("invalid CloudEvents batch size")
	}
	var items []json.RawMessage
	if err := protocolcore.DecodeAuthoring(document, &items); err != nil {
		return nil, fmt.Errorf("decode CloudEvents batch: %w", err)
	}
	if len(items) == 0 || len(items) > MaxBatchEvents {
		return nil, errors.New("CloudEvents batch item count is invalid")
	}
	events := make([]eventwire.EventEnvelope, len(items))
	for index, item := range items {
		event, err := eventwire.DecodeEventEnvelope(item)
		if err != nil {
			return nil, fmt.Errorf("decode CloudEvents batch item %d: %w", index, err)
		}
		events[index] = event
	}
	return events, nil
}

func validateTransportTrace(traceparent, tracestate string) error {
	if err := (protocolcore.TraceContext{Traceparent: traceparent, Tracestate: tracestate}).Validate(); err != nil {
		return fmt.Errorf("invalid trace context: %w", err)
	}
	return nil
}

func encodeValidatedEvent(event eventwire.EventEnvelope) ([]byte, error) {
	encoded, err := eventwire.EncodeEventEnvelope(event)
	if err != nil {
		return nil, err
	}
	if _, err := eventwire.DecodeEventEnvelope(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func singleHeader(headers http.Header, name string, required bool) (string, error) {
	values := []string{}
	for key, items := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, items...)
		}
	}
	if len(values) == 0 {
		if required {
			return "", fmt.Errorf("missing HTTP header %s", name)
		}
		return "", nil
	}
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) {
		return "", fmt.Errorf("HTTP header %s must have exactly one canonical value", name)
	}
	return values[0], nil
}

func rejectCEHeaders(headers http.Header) error {
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), "ce-") {
			return errors.New("structured CloudEvent must not carry binary ce-* headers")
		}
	}
	return nil
}

func semanticEqual(left, right eventwire.EventEnvelope) bool {
	leftWire, leftErr := encodeValidatedEvent(left)
	rightWire, rightErr := encodeValidatedEvent(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftWire, rightWire)
}
