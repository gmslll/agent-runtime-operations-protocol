package delivery

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	generatedrun "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

type HTTPCallerProvider interface {
	Authenticate(*http.Request) (dispatch.Caller, error)
}

type HTTPHandler struct {
	service *Service
	callers HTTPCallerProvider
}

func NewHTTPHandler(service *Service, callers HTTPCallerProvider) (http.Handler, error) {
	if service == nil || callers == nil {
		return nil, errors.New("delivery HTTP dependencies are required")
	}
	return &HTTPHandler{service: service, callers: callers}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	prefix, suffix := "/v1/agent-runs/", ":deliver"
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || !strings.HasPrefix(request.URL.Path, prefix) || !strings.HasSuffix(request.URL.Path, suffix) {
		http.NotFound(writer, request)
		return
	}
	runID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, prefix), suffix)
	if runID == "" || strings.Contains(runID, "/") || !singleHeaderValue(request.Header, "Content-Type", "application/json") {
		writeError(writer, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest))
		return
	}
	attemptID, attemptOK := singleHeader(request.Header, "Idempotency-Key")
	token, tokenOK := singleHeader(request.Header, "X-AROP-Run-Token")
	if !attemptOK || !tokenOK || len(token) < 96 {
		writeError(writer, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest))
		return
	}
	caller, err := handler.callers.Authenticate(request)
	if err != nil {
		writeError(writer, dispatch.NewError(dispatch.CategoryAuthentication, dispatch.ReasonAuthenticationRequired))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, (8<<20)+1))
	_ = request.Body.Close()
	if err != nil || len(body) > 8<<20 {
		writeError(writer, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest))
		return
	}
	status, err := handler.service.Deliver(request.Context(), Request{Caller: caller, RunID: runID, AttemptID: attemptID, RunToken: token, Body: body})
	if err != nil {
		writeError(writer, err)
		return
	}
	encoded, err := generatedrun.EncodeRunStatus(status)
	if err != nil {
		writeError(writer, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusAccepted)
	_, _ = writer.Write(encoded)
}

func writeError(writer http.ResponseWriter, err error) {
	failure, ok := dispatch.AsError(err)
	if !ok {
		failure = dispatch.Error{Category: dispatch.CategoryDependency, Reason: dispatch.ReasonDependencyUnavailable, Retryable: true}
	}
	status := http.StatusBadRequest
	switch failure.Category {
	case dispatch.CategoryAuthentication:
		status = http.StatusUnauthorized
		writer.Header().Set("WWW-Authenticate", "Bearer")
	case dispatch.CategoryAuthorization:
		status = http.StatusForbidden
	case dispatch.CategoryNotFound:
		status = http.StatusNotFound
	case dispatch.CategoryConflict:
		status = http.StatusConflict
	case dispatch.CategoryCapacity:
		status = http.StatusTooManyRequests
	case dispatch.CategoryDependency:
		status = http.StatusServiceUnavailable
	}
	var retry *controlplane.SafeInteger
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		writer.Header().Set("Retry-After", "1")
		value := controlplane.SafeInteger(1)
		retry = &value
	}
	code := strings.ToUpper(strings.ReplaceAll(string(failure.Reason), "-", "_"))
	encoded, encodeErr := controlplane.EncodeAROPError(controlplane.AROPError{Category: string(failure.Category), Code: code, Message: code, Retryable: failure.Retryable, RetryAfterSeconds: retry})
	if encodeErr != nil {
		status, encoded = http.StatusInternalServerError, []byte(`{"category":"internal","code":"INTERNAL_ERROR","message":"INTERNAL_ERROR","retryable":false}`)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func singleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(values[0], "\r\n") {
		return "", false
	}
	return values[0], true
}

func singleHeaderValue(header http.Header, name, expected string) bool {
	value, ok := singleHeader(header, name)
	return ok && strings.EqualFold(value, expected)
}

var _ http.Handler = (*HTTPHandler)(nil)
