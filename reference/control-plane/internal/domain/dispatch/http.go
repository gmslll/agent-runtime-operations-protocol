package dispatch

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	generated "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
)

type HTTPCallerProvider interface {
	Authenticate(*http.Request) (Caller, error)
	Metadata(*http.Request) (platform.RequestMetadata, bool)
}

type HTTPHandler struct {
	service *Service
	callers HTTPCallerProvider
}

func NewHTTPHandler(service *Service, callers HTTPCallerProvider) (http.Handler, error) {
	if service == nil || callers == nil {
		return nil, errors.New("dispatch HTTP dependencies are required")
	}
	return &HTTPHandler{service: service, callers: callers}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		writeDispatchError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/.well-known/arop-jwks.json" {
		handler.jwks(writer, request)
		return
	}
	prefix, suffix := "/v1/agent-runs/", ":dispatch"
	if request.Method != http.MethodPost || !strings.HasPrefix(request.URL.Path, prefix) || !strings.HasSuffix(request.URL.Path, suffix) {
		http.NotFound(writer, request)
		return
	}
	runID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, prefix), suffix)
	if runID == "" || strings.Contains(runID, "/") || !emptyBody(request) {
		writeDispatchError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	values := request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		writeDispatchError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	caller, err := handler.callers.Authenticate(request)
	if err != nil {
		writeDispatchError(writer, normalizeHTTPAuthentication(err))
		return
	}
	metadata, ok := handler.callers.Metadata(request)
	if !ok {
		writeDispatchError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	ticket, attempt, err := handler.service.Dispatch(request.Context(), DispatchRequest{Caller: caller, RunID: runID, IdempotencyKey: values[0], Metadata: metadata})
	if err != nil {
		writeDispatchError(writer, err)
		return
	}
	wire := generated.DispatchTicket{
		SchemaVersion: 1,
		RunID:         generated.RunId(ticket.RunID), AttemptID: generated.AttemptId(ticket.AttemptID), FencingToken: generated.SafeInteger(ticket.FencingToken),
		Agent:    generated.AgentBinding{ID: generated.AgentId(ticket.Agent.ID), Version: generated.SemanticVersion(ticket.Agent.Version), SkillID: generated.SkillId(ticket.Agent.SkillID), ManifestDigest: generated.Sha256Digest(ticket.Agent.ManifestDigest)},
		Delivery: generated.Delivery{Mode: ticket.Delivery.Mode, DeploymentID: generated.DeploymentId(ticket.Delivery.DeploymentID), InstanceID: generated.InstanceId(ticket.Delivery.InstanceID), Generation: generated.SafeInteger(ticket.Delivery.Generation), Audience: generated.URIReference(ticket.Delivery.Audience), Endpoint: generated.URIReference(ticket.Delivery.Endpoint), ExpiresAt: generated.DateTime(ticket.Delivery.ExpiresAt.Format(time.RFC3339Nano))},
		RunToken: ticket.RunToken,
		Trace:    generated.AROPV1W3CTraceContext{Traceparent: ticket.Traceparent},
	}
	if ticket.Delivery.StreamEndpoint != "" {
		value := generated.URIReference(ticket.Delivery.StreamEndpoint)
		wire.Delivery.StreamEndpoint = &value
	}
	if ticket.Tracestate != "" {
		wire.Trace.Tracestate = &ticket.Tracestate
	}
	encoded, err := generated.EncodeDispatchTicket(wire)
	if err != nil {
		writeDispatchError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Location", "/v1/agent-runs/"+ticket.RunID+"/attempts/"+attempt.AttemptID)
	writer.WriteHeader(http.StatusCreated)
	_, _ = writer.Write(encoded)
}

func (handler *HTTPHandler) jwks(writer http.ResponseWriter, request *http.Request) {
	if !emptyBody(request) {
		writeDispatchError(writer, NewError(CategoryValidation, ReasonInvalidRequest))
		return
	}
	result, err := handler.service.JWKS(request.Context())
	if err != nil {
		writeDispatchError(writer, err)
		return
	}
	keys := make([]generated.Key, 0, len(result.Keys))
	for _, key := range result.Keys {
		signUntil := generated.DateTime(key.SignUntil.Format(time.RFC3339Nano))
		keys = append(keys, generated.Key{Kty: "EC", Crv: "P-256", Use: "sig", Alg: "ES256", Kid: key.KeyID, X: key.X, Y: key.Y, Status: string(key.Status), NotBefore: generated.DateTime(key.NotBefore.Format(time.RFC3339Nano)), SignUntil: signUntil, VerifyUntil: generated.DateTime(key.VerifyUntil.Format(time.RFC3339Nano)), CreatedAt: generated.DateTime(key.CreatedAt.Format(time.RFC3339Nano))})
	}
	wire := generated.JWKSMetadata{SchemaVersion: 1, Issuer: generated.URIReference(result.Issuer), CacheUntil: generated.DateTime(result.CacheUntil.Format(time.RFC3339Nano)), Keys: keys}
	encoded, err := generated.EncodeJWKSMetadata(wire)
	if err != nil {
		writeDispatchError(writer, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	maxAge := int(result.CacheUntil.Sub(handler.service.now()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	if maxAge > 9999 {
		maxAge = 9999
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

func emptyBody(request *http.Request) bool {
	if request.Body == nil {
		return request.ContentLength <= 0
	}
	defer request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(request.Body, 1))
	return err == nil && len(data) == 0 && request.ContentLength <= 0
}

func normalizeHTTPAuthentication(err error) error {
	if failure, ok := AsError(err); ok && (failure.Category == CategoryAuthentication || failure.Category == CategoryDependency) {
		return failure
	}
	return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
}

func writeDispatchError(writer http.ResponseWriter, err error) {
	failure, ok := AsError(err)
	if !ok {
		failure = Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
	}
	status := http.StatusBadRequest
	switch failure.Category {
	case CategoryAuthentication:
		status = http.StatusUnauthorized
		writer.Header().Set("WWW-Authenticate", "Bearer")
	case CategoryAuthorization:
		status = http.StatusForbidden
	case CategoryNotFound:
		status = http.StatusNotFound
	case CategoryConflict:
		status = http.StatusConflict
	case CategoryCapacity:
		status = http.StatusTooManyRequests
	case CategoryTimeout:
		status = http.StatusRequestTimeout
	case CategoryDependency:
		status = http.StatusServiceUnavailable
	}
	var retryAfter *controlplane.SafeInteger
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		writer.Header().Set("Retry-After", "1")
		value := controlplane.SafeInteger(1)
		retryAfter = &value
	}
	code := strings.ToUpper(strings.ReplaceAll(string(failure.Reason), "-", "_"))
	encoded, encodeErr := controlplane.EncodeAROPError(controlplane.AROPError{Code: code, Category: string(failure.Category), Message: code, Retryable: failure.Retryable, RetryAfterSeconds: retryAfter})
	if encodeErr != nil {
		status, encoded = http.StatusInternalServerError, []byte(`{"code":"INTERNAL_ERROR","category":"internal","message":"INTERNAL_ERROR","retryable":false}`)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
