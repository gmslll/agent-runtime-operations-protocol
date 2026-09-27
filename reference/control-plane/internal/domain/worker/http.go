package worker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

const maxWorkerRequestBytes = 1 << 20

type HTTPCallerProvider interface {
	Authenticate(*http.Request, Operation) (Caller, platform.RequestMetadata, error)
}

type HTTPHandler struct {
	service *Service
	callers HTTPCallerProvider
}

func NewHTTPHandler(service *Service, callers HTTPCallerProvider) (http.Handler, error) {
	if service == nil || callers == nil {
		return nil, errors.New("worker HTTP dependencies are required")
	}
	return &HTTPHandler{service: service, callers: callers}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || !strings.HasPrefix(request.URL.Path, "/v1/workers/") {
		http.NotFound(writer, request)
		return
	}
	tail := strings.TrimPrefix(request.URL.Path, "/v1/workers/")
	parts := strings.Split(tail, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "claims:next" {
		handler.claim(writer, request, parts[0])
		return
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] != "claims" || parts[2] == "" {
		http.NotFound(writer, request)
		return
	}
	action := ""
	claimID := parts[2]
	if index := strings.LastIndexByte(claimID, ':'); index >= 0 {
		action, claimID = claimID[index+1:], claimID[:index]
	}
	if claimID == "" || action == "" {
		http.NotFound(writer, request)
		return
	}
	switch action {
	case "renew":
		handler.renew(writer, request, parts[0], claimID)
	case "complete":
		handler.complete(writer, request, parts[0], claimID)
	case "release":
		handler.release(writer, request, parts[0], claimID)
	default:
		http.NotFound(writer, request)
	}
}

func (handler *HTTPHandler) claim(writer http.ResponseWriter, request *http.Request, workerID string) {
	caller, metadata, ok := handler.authorize(writer, request, OperationClaim)
	if !ok {
		return
	}
	body, ok := readWorkerJSON(writer, request)
	if !ok {
		return
	}
	wire, err := workerwire.DecodeWorkerClaimRequest(body)
	clear(body)
	if err != nil {
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	bindings := make([]Binding, len(wire.SupportedBindings))
	for index, item := range wire.SupportedBindings {
		bindings[index] = Binding{AgentID: string(item.ID), Version: string(item.Version), SkillID: string(item.SkillID), ManifestDigest: string(item.ManifestDigest)}
	}
	leaseSeconds := uint64(60)
	if wire.LeaseSeconds != nil {
		leaseSeconds = uint64(*wire.LeaseSeconds)
	}
	claim, found, err := handler.service.Claim(request.Context(), ClaimRequest{Caller: caller, WorkerID: workerID, SessionID: string(wire.SessionID), Generation: uint64(wire.Generation), AvailableSlots: uint64(wire.AvailableSlots), SupportedBindings: bindings, WaitSeconds: uint64(wire.WaitSeconds), LeaseSeconds: leaseSeconds, Metadata: metadata})
	if err != nil {
		writeWorkerError(writer, err)
		return
	}
	if !found {
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	encoded, err := claim.Wire()
	if err != nil {
		writeWorkerError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writeWorkerJSON(writer, http.StatusOK, encoded)
}

func (handler *HTTPHandler) renew(writer http.ResponseWriter, request *http.Request, workerID, claimID string) {
	caller, metadata, ok := handler.authorize(writer, request, OperationRenew)
	if !ok {
		return
	}
	body, ok := readWorkerJSON(writer, request)
	if !ok {
		return
	}
	wire, err := workerwire.DecodeWorkerRenewRequest(body)
	clear(body)
	if err != nil {
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	claim, err := handler.service.Renew(request.Context(), RenewRequest{Caller: caller, WorkerID: workerID, ClaimID: claimID, LeaseToken: string(wire.LeaseToken), FencingToken: uint64(wire.FencingToken), LeaseSeconds: uint64(wire.LeaseSeconds), Metadata: metadata})
	if err != nil {
		writeWorkerError(writer, err)
		return
	}
	encoded, err := claim.Wire()
	if err != nil {
		writeWorkerError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writeWorkerJSON(writer, http.StatusOK, encoded)
}

func (handler *HTTPHandler) complete(writer http.ResponseWriter, request *http.Request, workerID, claimID string) {
	caller, metadata, ok := handler.authorize(writer, request, OperationComplete)
	if !ok {
		return
	}
	keys := request.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !validIdempotencyKey(keys[0]) {
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	body, ok := readWorkerJSON(writer, request)
	if !ok {
		return
	}
	wire, err := workerwire.DecodeWorkerComplete(body)
	if err != nil || string(wire.ClaimID) != claimID {
		clear(body)
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	result, err := json.Marshal(wire.Result)
	clear(body)
	completedAt, parseErr := time.Parse(time.RFC3339Nano, string(wire.CompletedAt))
	if err != nil || parseErr != nil {
		clear(result)
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	effects := []string(nil)
	if wire.EffectIDs != nil {
		effects = make([]string, len(*wire.EffectIDs))
		for index, id := range *wire.EffectIDs {
			effects[index] = string(id)
		}
	}
	_, err = handler.service.Complete(request.Context(), CompleteRequest{Caller: caller, WorkerID: workerID, ClaimID: claimID, CompletionID: wire.CompletionID, AttemptID: string(wire.AttemptID), LeaseToken: string(wire.LeaseToken), IdempotencyKey: keys[0], FencingToken: uint64(wire.FencingToken), Result: result, EffectIDs: effects, CompletedAt: completedAt.UTC(), Metadata: metadata})
	clear(result)
	if err != nil {
		writeWorkerError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *HTTPHandler) release(writer http.ResponseWriter, request *http.Request, workerID, claimID string) {
	caller, metadata, ok := handler.authorize(writer, request, OperationRelease)
	if !ok {
		return
	}
	body, ok := readWorkerJSON(writer, request)
	if !ok {
		return
	}
	wire, err := workerwire.DecodeWorkerReleaseRequest(body)
	clear(body)
	if err != nil {
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	if err = handler.service.Release(request.Context(), ReleaseRequest{Caller: caller, WorkerID: workerID, ClaimID: claimID, LeaseToken: string(wire.LeaseToken), FencingToken: uint64(wire.FencingToken), Reason: wire.Reason, Metadata: metadata}); err != nil {
		writeWorkerError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

func (handler *HTTPHandler) authorize(writer http.ResponseWriter, request *http.Request, operation Operation) (Caller, platform.RequestMetadata, bool) {
	caller, metadata, err := handler.callers.Authenticate(request, operation)
	if err != nil {
		writeWorkerError(writer, normalizeHTTPAuthentication(err))
		return Caller{}, platform.RequestMetadata{}, false
	}
	return caller, metadata, true
}

func readWorkerJSON(writer http.ResponseWriter, request *http.Request) ([]byte, bool) {
	if values := request.Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		writeWorkerErrorStatus(writer, NewError(CategoryValidation, ReasonInvalidRequest), http.StatusUnsupportedMediaType)
		return nil, false
	}
	if request.Body == nil {
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return nil, false
	}
	defer request.Body.Close()
	body, err := io.ReadAll(io.LimitReader(request.Body, maxWorkerRequestBytes+1))
	if err != nil || len(body) == 0 {
		clear(body)
		writeWorkerError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return nil, false
	}
	if len(body) > maxWorkerRequestBytes {
		clear(body)
		writeWorkerErrorStatus(writer, NewError(CategoryCapacity, ReasonInvalidRequest), http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

func normalizeHTTPAuthentication(err error) error {
	if typed, ok := AsError(err); ok && (typed.Category == CategoryAuthentication || typed.Category == CategoryAuthorization || typed.Category == CategoryDependency) {
		return err
	}
	return NewError(CategoryAuthentication, ReasonAuthentication)
}

func writeWorkerJSON(writer http.ResponseWriter, status int, encoded []byte) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func writeWorkerError(writer http.ResponseWriter, err error) {
	writeWorkerErrorStatus(writer, err, statusFor(err))
}

func writeWorkerErrorStatus(writer http.ResponseWriter, err error, status int) {
	failure, ok := AsError(err)
	if !ok {
		failure = Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
		status = http.StatusServiceUnavailable
	}
	if status == http.StatusUnauthorized {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="arop"`)
	}
	var retryAfter *controlplane.SafeInteger
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
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
	writeWorkerJSON(writer, status, encoded)
}

var _ http.Handler = (*HTTPHandler)(nil)
