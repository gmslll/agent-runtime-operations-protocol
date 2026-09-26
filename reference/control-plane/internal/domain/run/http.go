package run

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	generated "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

const maxRunRequestBytes = 1 << 20

var errRunRequestTooLarge = errors.New("run request too large")

type CallerProvider interface {
	Authenticate(*http.Request, Operation) (Caller, error)
}
type requestMetadataProvider interface {
	Metadata(*http.Request) (platform.RequestMetadata, bool)
}
type HTTPHandler struct {
	service *Service
	callers CallerProvider
}

func NewHTTPHandler(service *Service, callers CallerProvider) (http.Handler, error) {
	if service == nil || callers == nil {
		return nil, errors.New("run HTTP dependencies are required")
	}
	return &HTTPHandler{service: service, callers: callers}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	path := strings.TrimSuffix(request.URL.Path, "/")
	if path == "/v1/agent-runs" && request.Method == http.MethodPost {
		handler.create(writer, request)
		return
	}
	if !strings.HasPrefix(path, "/v1/agent-runs/") {
		http.NotFound(writer, request)
		return
	}
	tail := strings.TrimPrefix(path, "/v1/agent-runs/")
	parts := strings.Split(tail, "/")
	if len(parts) == 1 && request.Method == http.MethodGet {
		handler.get(writer, request, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "commands" && request.Method == http.MethodPost {
		handler.command(writer, request, parts[0])
		return
	}
	writer.Header().Set("Allow", allowedMethod(parts))
	writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), http.StatusMethodNotAllowed)
}

func (handler *HTTPHandler) create(writer http.ResponseWriter, request *http.Request) {
	if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), http.StatusUnsupportedMediaType)
		return
	}
	caller, err := handler.callers.Authenticate(request, OperationCreate)
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	idempotency, ok := singleHeader(request.Header, "Idempotency-Key")
	if !ok {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	body, err := readBody(request)
	if err != nil {
		if errors.Is(err, errRunRequestTooLarge) {
			writeTyped(writer, NewError(CategoryCapacity, ReasonRunRequestTooLarge))
			return
		}
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	wire, err := generated.DecodeRunRequest(body)
	if err != nil {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	input, _ := json.Marshal(wire.Input)
	effect := EffectIntent{}
	switch {
	case wire.Effects.None != nil:
		effect.Level = EffectNone
	case wire.Effects.Read != nil:
		effect.Level = EffectRead
	case wire.Effects.Write != nil:
		effect.Level = EffectWrite
		effect.EffectID = string(wire.Effects.Write.EffectID)
	case wire.Effects.Irreversible != nil:
		effect.Level = EffectIrreversible
		effect.EffectID = string(wire.Effects.Irreversible.EffectID)
	}
	deadline, err := time.Parse(time.RFC3339Nano, string(wire.DeadlineAt))
	if err != nil {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	conversation := ""
	if wire.ConversationRef != nil {
		conversation = string(*wire.ConversationRef)
	}
	var labels json.RawMessage
	if wire.Labels != nil {
		labels, err = json.Marshal(wire.Labels)
		if err != nil {
			writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
			return
		}
	}
	tracestate := ""
	if wire.Trace.Tracestate != nil {
		tracestate = *wire.Trace.Tracestate
	}
	record, err := handler.service.Create(request.Context(), CreateRequest{Caller: caller, Agent: AgentBinding{ID: string(wire.Agent.ID), Version: string(wire.Agent.Version), SkillID: string(wire.Agent.SkillID), ManifestDigest: string(wire.Agent.ManifestDigest)}, Input: input, Labels: labels, ConversationRef: conversation, DeadlineAt: deadline.UTC(), Effects: effect, Metadata: handler.metadata(request, wire.Trace.Traceparent), Tracestate: tracestate, IdempotencyKey: idempotency})
	if err != nil {
		writeTyped(writer, err)
		return
	}
	writer.Header().Set("Location", "/v1/agent-runs/"+record.RunID)
	writeStatus(writer, record, 201)
}
func (handler *HTTPHandler) get(writer http.ResponseWriter, request *http.Request, runID string) {
	caller, err := handler.callers.Authenticate(request, OperationRead)
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	record, err := handler.service.Get(request.Context(), caller, runID, handler.metadata(request, request.Header.Get("traceparent")))
	if err != nil {
		writeTyped(writer, err)
		return
	}
	writeStatus(writer, record, 200)
}
func (handler *HTTPHandler) command(writer http.ResponseWriter, request *http.Request, runID string) {
	if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), http.StatusUnsupportedMediaType)
		return
	}
	caller, err := handler.callers.Authenticate(request, OperationCommand)
	if err != nil {
		writeAuthError(writer, err)
		return
	}
	idempotency, ok := singleHeader(request.Header, "Idempotency-Key")
	if !ok {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	body, err := readBody(request)
	if err != nil {
		if errors.Is(err, errRunRequestTooLarge) {
			writeTyped(writer, NewError(CategoryCapacity, ReasonRunRequestTooLarge))
			return
		}
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	wire, err := generated.DecodeRunCommand(body)
	if err != nil {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &raw) != nil {
		writeRunError(writer, NewError(CategoryValidation, ReasonInvalidRequest), 400)
		return
	}
	record, err := handler.service.Cancel(request.Context(), CommandRequest{Caller: caller, RunID: runID, IdempotencyKey: idempotency, Command: Command{CommandID: wire.CommandID, ExpectedStateVersion: uint64(wire.ExpectedStateVersion), Type: wire.Type, Data: raw.Data}, Metadata: handler.metadata(request, request.Header.Get("traceparent"))})
	if err != nil {
		writeTyped(writer, err)
		return
	}
	writeStatus(writer, record, 200)
}

func readBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, errors.New("body required")
	}
	defer request.Body.Close()
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRunRequestBytes+1))
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) || len(body) > maxRunRequestBytes {
		return nil, errRunRequestTooLarge
	}
	if err != nil || len(body) == 0 {
		return nil, errors.New("invalid body")
	}
	return body, nil
}
func writeStatus(writer http.ResponseWriter, record Run, status int) {
	wire := generated.RunStatus{
		SchemaVersion: 1,
		RunID:         generated.RunId(record.RunID),
		State:         string(record.State),
		StateVersion:  generated.SafeInteger(record.StateVersion),
		Agent: generated.AgentBinding{
			ID:             generated.AgentId(record.Agent.ID),
			Version:        generated.SemanticVersion(record.Agent.Version),
			SkillID:        generated.SkillId(record.Agent.SkillID),
			ManifestDigest: generated.Sha256Digest(record.Agent.ManifestDigest),
		},
		AuthorizationSnapshotDigest: generated.Sha256Digest(record.AuthorizationSnapshotDigest),
		CreatedAt:                   generated.DateTime(record.CreatedAt.Format(time.RFC3339Nano)),
		UpdatedAt:                   generated.DateTime(record.UpdatedAt.Format(time.RFC3339Nano)),
		DeadlineAt:                  generated.DateTime(record.DeadlineAt.Format(time.RFC3339Nano)),
		Trace:                       generated.AROPV1W3CTraceContext{Traceparent: record.Traceparent},
	}
	if record.Tracestate != "" {
		wire.Trace.Tracestate = &record.Tracestate
	}
	if record.CancelRequestedAt != nil {
		value := generated.DateTime(record.CancelRequestedAt.Format(time.RFC3339Nano))
		wire.CancelRequestedAt = &value
	}
	encoded, err := generated.EncodeRunStatus(wire)
	if err != nil {
		writeTyped(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
func writeTyped(writer http.ResponseWriter, err error) {
	failure, ok := AsError(err)
	if !ok {
		failure = Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
	}
	status := 500
	switch failure.Category {
	case CategoryValidation:
		status = 400
	case CategoryAuthentication:
		status = 401
	case CategoryAuthorization:
		status = 403
	case CategoryNotFound:
		status = 404
	case CategoryConflict:
		status = 409
	case CategoryCapacity:
		status = http.StatusRequestEntityTooLarge
	case CategoryTimeout:
		status = 408
	case CategoryDependency:
		status = 503
	}
	if status == 401 {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="arop"`)
	}
	if status == 503 {
		writer.Header().Set("Retry-After", "1")
	}
	writeRunError(writer, failure, status)
}
func writeAuthError(writer http.ResponseWriter, err error) {
	failure, ok := AsError(err)
	if !ok || failure.Category != CategoryDependency {
		failure = Error{Category: CategoryAuthentication, Reason: ReasonAuthenticationRequired}
	}
	writeTyped(writer, failure)
}
func writeRunError(writer http.ResponseWriter, err error, status int) {
	failure, ok := AsError(err)
	if !ok {
		failure = Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
	}
	body := map[string]any{"code": strings.ToUpper(strings.ReplaceAll(string(failure.Reason), "-", "_")), "category": failure.Category, "message": failure.Reason, "retryable": failure.Retryable}
	if status == 503 {
		body["retry_after_seconds"] = 1
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
func singleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	return first(values), len(values) == 1 && validIdempotencyKey(first(values))
}
func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
func allowedMethod(parts []string) string {
	if len(parts) == 1 {
		return http.MethodGet
	}
	if len(parts) == 2 && parts[1] == "commands" {
		return http.MethodPost
	}
	return ""
}
func (handler *HTTPHandler) metadata(request *http.Request, traceparent string) platform.RequestMetadata {
	metadata := metadataFromWire(traceparent)
	if provider, ok := handler.callers.(requestMetadataProvider); ok {
		if outer, found := provider.Metadata(request); found {
			metadata.RequestID = outer.RequestID
			if traceparent == "" {
				metadata = outer
			}
		}
	}
	return metadata
}
func metadataFromWire(traceparent string) platform.RequestMetadata {
	parts := strings.Split(traceparent, "-")
	metadata := platform.RequestMetadata{RequestID: "req_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceFlags: "01"}
	if len(parts) == 4 {
		metadata.TraceID, metadata.ParentSpanID, metadata.TraceFlags = parts[1], parts[2], parts[3]
	}
	return metadata
}
