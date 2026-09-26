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

type CallerProvider interface {
	Authenticate(*http.Request, Operation) (Caller, error)
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
	record, err := handler.service.Create(request.Context(), CreateRequest{Caller: caller, Agent: AgentBinding{ID: string(wire.Agent.ID), Version: string(wire.Agent.Version), SkillID: string(wire.Agent.SkillID), ManifestDigest: string(wire.Agent.ManifestDigest)}, Input: input, ConversationRef: conversation, DeadlineAt: deadline.UTC(), Effects: effect, Metadata: metadataFromWire(wire.Trace.Traceparent), IdempotencyKey: idempotency})
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
	record, err := handler.service.Get(request.Context(), caller, runID, metadataFromHeader(request))
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
	record, err := handler.service.Cancel(request.Context(), CommandRequest{Caller: caller, RunID: runID, IdempotencyKey: idempotency, Command: Command{CommandID: wire.CommandID, ExpectedStateVersion: uint64(wire.ExpectedStateVersion), Type: wire.Type, Data: raw.Data}, Metadata: metadataFromHeader(request)})
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
	if err != nil || len(body) == 0 || len(body) > maxRunRequestBytes {
		return nil, errors.New("invalid body")
	}
	return body, nil
}
func writeStatus(writer http.ResponseWriter, record Run, status int) {
	wire := map[string]any{"schema_version": 1, "run_id": record.RunID, "state": record.State, "state_version": record.StateVersion, "agent": map[string]any{"id": record.Agent.ID, "version": record.Agent.Version, "skill_id": record.Agent.SkillID, "manifest_digest": record.Agent.ManifestDigest}, "authorization_snapshot_digest": record.AuthorizationSnapshotDigest, "created_at": record.CreatedAt.Format(time.RFC3339Nano), "updated_at": record.UpdatedAt.Format(time.RFC3339Nano), "deadline_at": record.DeadlineAt.Format(time.RFC3339Nano), "trace": map[string]any{"traceparent": record.Traceparent}}
	if record.CancelRequestedAt != nil {
		wire["cancel_requested_at"] = record.CancelRequestedAt.Format(time.RFC3339Nano)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(wire)
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
func metadataFromHeader(request *http.Request) platform.RequestMetadata {
	return metadataFromWire(request.Header.Get("traceparent"))
}
func metadataFromWire(traceparent string) platform.RequestMetadata {
	parts := strings.Split(traceparent, "-")
	metadata := platform.RequestMetadata{RequestID: "req_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceFlags: "01"}
	if len(parts) == 4 {
		metadata.TraceID, metadata.ParentSpanID, metadata.TraceFlags = parts[1], parts[2], parts[3]
	}
	return metadata
}
