package event

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
)

type HTTPMetadataProvider interface {
	Metadata(*http.Request) (platform.RequestMetadata, bool)
	TenantID(*http.Request) (string, bool)
}
type HTTPHandler struct {
	service  *Service
	metadata HTTPMetadataProvider
}

func NewHTTPHandler(service *Service, metadata HTTPMetadataProvider) (http.Handler, error) {
	if service == nil || metadata == nil {
		return nil, errors.New("event HTTP dependencies are required")
	}
	return &HTTPHandler{service: service, metadata: metadata}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" {
		http.NotFound(writer, request)
		return
	}
	prefix := "/v1/agent-runs/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		http.NotFound(writer, request)
		return
	}
	rest := strings.TrimPrefix(request.URL.Path, prefix)
	metadata, ok := handler.metadata.Metadata(request)
	if !ok {
		writeEventError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	switch {
	case strings.HasSuffix(rest, "/event-session"):
		runID := strings.TrimSuffix(rest, "/event-session")
		if runID == "" || strings.Contains(runID, "/") {
			writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		handler.session(writer, request, runID, metadata)
	case strings.HasSuffix(rest, "/events:batch"):
		runID := strings.TrimSuffix(rest, "/events:batch")
		if runID == "" || strings.Contains(runID, "/") {
			writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		handler.batch(writer, request, runID, metadata)
	default:
		http.NotFound(writer, request)
	}
}

func (handler *HTTPHandler) session(writer http.ResponseWriter, request *http.Request, runID string, metadata platform.RequestMetadata) {
	if !jsonContent(request) {
		writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	tenantID, ok := handler.metadata.TenantID(request)
	if !ok {
		writeEventError(writer, NewError(CategoryAuthentication, ReasonAuthentication))
		return
	}
	body, err := readEventBody(request, 64<<10)
	if err != nil {
		writeEventError(writer, err)
		return
	}
	wire, err := eventwire.DecodeEventSessionRequest(body)
	clear(body)
	if err != nil {
		writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	session, err := handler.service.CreateSession(request.Context(), SessionRequest{TenantID: tenantID, RunID: runID, AttemptID: string(wire.AttemptID), DeploymentID: string(wire.DeploymentID), Generation: uint64(wire.Generation), FencingToken: uint64(wire.FencingToken), Metadata: metadata})
	if err != nil {
		writeEventError(writer, err)
		return
	}
	response := eventwire.EventSession{EventBatchURL: eventwire.URIReference(handler.service.BatchURL(runID)), EventToken: session.Token, ExpiresAt: eventwire.DateTime(session.ExpiresAt.Format(time.RFC3339Nano)), MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes}
	encoded, err := eventwire.EncodeEventSession(response)
	if err != nil {
		writeEventError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusCreated)
	_, _ = writer.Write(encoded)
}

func (handler *HTTPHandler) batch(writer http.ResponseWriter, request *http.Request, runID string, metadata platform.RequestMetadata) {
	if !jsonContent(request) {
		writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	token, err := bearer(request.Header.Values("Authorization"))
	if err != nil || !strings.HasPrefix(token, "evtcap_") {
		writeEventError(writer, NewError(CategoryAuthentication, ReasonAuthentication))
		return
	}
	keys := request.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	body, err := readEventBody(request, MaxBatchBytes)
	if err != nil {
		writeEventError(writer, err)
		return
	}
	wire, err := eventwire.DecodeEventBatch(body)
	clear(body)
	if err != nil {
		writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	events := make([]Envelope, 0, len(wire.Events))
	for _, item := range wire.Events {
		// runsequence is assigned by the Control Plane after the append is
		// committed. A producer must never be able to select or spoof it.
		if item.Runsequence != nil {
			writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		data, marshalErr := json.Marshal(item.Data)
		occurred, parseErr := time.Parse(time.RFC3339Nano, string(item.Time))
		if marshalErr != nil || parseErr != nil {
			writeEventError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		events = append(events, Envelope{SpecVersion: item.Specversion, ID: string(item.ID), Source: string(item.Source), Type: item.Type, Subject: item.Subject, Time: occurred, DataContentType: item.Datacontenttype, DataSchema: string(item.Dataschema), RunID: string(item.Runid), AttemptID: string(item.Attemptid), ProducerSequence: uint64(item.Producersequence), Traceparent: string(item.Traceparent), Data: data})
	}
	ack, err := handler.service.Append(request.Context(), BatchRequest{Token: token, RunID: runID, BatchID: wire.BatchID, AttemptID: string(wire.AttemptID), FencingToken: uint64(wire.FencingToken), IdempotencyKey: keys[0], Events: events, Metadata: metadata})
	if err != nil {
		writeEventError(writer, err)
		return
	}
	duplicates := make([]eventwire.EventId, len(ack.DuplicateEventIDs))
	for index, id := range ack.DuplicateEventIDs {
		duplicates[index] = eventwire.EventId(id)
	}
	response := eventwire.EventBatchAck{AcceptedThroughProducerSequence: eventwire.EventSessionPositiveSafeInteger(ack.AcceptedThroughProducerSequence), AssignedRunSequence: eventwire.EventSessionPositiveSafeInteger(ack.AssignedRunSequence), DuplicateEventIDs: duplicates, RunState: string(ack.RunState)}
	encoded, err := eventwire.EncodeEventBatchAck(response)
	if err != nil {
		writeEventError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

func readEventBody(request *http.Request, limit int64) ([]byte, error) {
	if request.Body == nil {
		return nil, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	defer request.Body.Close()
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return nil, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if int64(len(body)) > limit {
		return nil, NewError(CategoryCapacity, ReasonBatchTooLarge)
	}
	if len(body) == 0 {
		return nil, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return body, nil
}
func jsonContent(request *http.Request) bool {
	values := request.Header.Values("Content-Type")
	return len(values) == 1 && values[0] == "application/json"
}
func bearer(values []string) (string, error) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", NewError(CategoryAuthentication, ReasonAuthentication)
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n,") {
		return "", NewError(CategoryAuthentication, ReasonAuthentication)
	}
	return token, nil
}

func writeEventError(writer http.ResponseWriter, err error) {
	failure, ok := AsError(err)
	if !ok {
		failure = Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
	}
	status := statusFor(failure)
	if status == http.StatusUnauthorized {
		writer.Header().Set("WWW-Authenticate", "Bearer")
	}
	var retryAfter *controlplane.SafeInteger
	if status == http.StatusServiceUnavailable {
		value := controlplane.SafeInteger(1)
		retryAfter = &value
		writer.Header().Set("Retry-After", "1")
	}
	code := strings.ToUpper(strings.ReplaceAll(string(failure.Reason), "-", "_"))
	encoded, encodeErr := controlplane.EncodeAROPError(controlplane.AROPError{Code: code, Category: string(failure.Category), Message: code, Retryable: failure.Retryable, RetryAfterSeconds: retryAfter})
	if encodeErr != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"code":"INTERNAL_ERROR","category":"internal","message":"INTERNAL_ERROR","retryable":false}`)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
