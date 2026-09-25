// Package httpadapter binds the P08 platform application to net/http. It owns
// transport concerns; the parent platform package remains HTTP-independent.
package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

const (
	requestIDHeader   = "X-Request-ID"
	traceparentHeader = "Traceparent"
	tracestateHeader  = "Tracestate"
)

type healthResponse struct {
	Status     string                     `json:"status"`
	Service    string                     `json:"service"`
	Version    string                     `json:"version"`
	Scope      string                     `json:"scope"`
	Durability string                     `json:"durability"`
	Checks     []platform.ReadinessResult `json:"checks,omitempty"`
}

type errorResponse struct {
	Status string `json:"status"`
}

var ErrAuthenticationUnavailable = errors.New("authentication unavailable")

type AuthenticatedPrincipal struct {
	TenantID, PrincipalID, SubjectID, CredentialID string
	Scopes                                         []string
}

type AuthenticateFunc func(context.Context, string, platform.RequestMetadata) (AuthenticatedPrincipal, error)

type principalContextKey struct{}
type metadataContextKey struct{}
type requiredScopesContextKey struct{}

func PrincipalFromContext(ctx context.Context) (AuthenticatedPrincipal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(AuthenticatedPrincipal)
	return principal, ok
}

func MetadataFromContext(ctx context.Context) (platform.RequestMetadata, bool) {
	metadata, ok := ctx.Value(metadataContextKey{}).(platform.RequestMetadata)
	return metadata, ok
}

func RequiredScopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(requiredScopesContextKey{}).([]string)
	return slices.Clone(scopes)
}

func NewHandler(application *platform.Platform) (http.Handler, error) {
	return newHandler(application, nil)
}

func NewAuthenticatedHandler(application *platform.Platform, authenticate AuthenticateFunc) (http.Handler, error) {
	if authenticate == nil {
		return nil, errors.New("authentication function is required")
	}
	return newHandler(application, authenticate)
}

func NewPublicationHandler(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService) (http.Handler, error) {
	if authenticate == nil || service == nil {
		return nil, errors.New("publication authentication and service are required")
	}
	return newHandlerWithPublication(application, authenticate, service)
}

func newHandler(application *platform.Platform, authenticate AuthenticateFunc) (http.Handler, error) {
	return newHandlerWithPublication(application, authenticate, nil)
}

func newHandlerWithPublication(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService) (http.Handler, error) {
	if application == nil {
		return nil, errors.New("platform application is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Service: application.Service(), Version: application.Version(), Scope: platform.ReadinessScope, Durability: application.Durability()})
	})
	mux.HandleFunc("GET /v1/health/ready", func(writer http.ResponseWriter, request *http.Request) {
		snapshot := application.Readiness(request.Context())
		status := http.StatusOK
		state := "ready"
		if !snapshot.Ready {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		writeJSON(writer, status, healthResponse{Status: state, Service: application.Service(), Version: application.Version(), Scope: snapshot.Scope, Durability: snapshot.Durability, Checks: snapshot.Checks})
	})
	if service != nil {
		mux.HandleFunc("POST /v1/agent-definitions/{agent_id}/versions", publicationPublish(application, service))
		mux.HandleFunc("GET /v1/agent-definitions/{agent_id}/versions/{version}", publicationGet(application, service))
	}
	return instrument(application, mux, authenticate), nil
}

func publicationPublish(application *platform.Platform, service publication.PublicationService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != "application/vnd.arop.agent-version-bundle+zip" || len(request.Header.Values("Content-Type")) != 1 {
			writePublicationError(writer, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "validation", false)
			return
		}
		keys := request.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writePublicationError(writer, http.StatusBadRequest, "INVALID_PUBLICATION_REQUEST", "validation", false)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writePublicationError(writer, http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE", "capacity", false)
			return
		}
		caller, metadata, ok := publicationContext(application, request)
		if !ok {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		result, err := service.Publish(request.Context(), publication.PublishRequest{AgentID: request.PathValue("agent_id"), IdempotencyKey: keys[0], Bundle: body, Caller: caller, Metadata: metadata})
		if err != nil {
			writeDomainError(writer, err)
			return
		}
		writer.Header().Set("Location", result.Location)
		writer.Header().Set("ETag", result.ETag)
		writer.WriteHeader(http.StatusCreated)
	}
}

func publicationGet(application *platform.Platform, service publication.PublicationService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		caller, metadata, ok := publicationContext(application, request)
		if !ok {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		result, err := service.Get(request.Context(), publication.GetRequest{AgentID: request.PathValue("agent_id"), Version: request.PathValue("version"), Caller: caller, Metadata: metadata})
		if err != nil {
			writeDomainError(writer, err)
			return
		}
		manifest, err := controlplane.DecodeAgentManifest(result.CanonicalManifest)
		if err != nil {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		encoded, err := controlplane.EncodeAgentManifest(manifest)
		if err != nil {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", result.ETag)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(encoded)
	}
}

func publicationContext(application *platform.Platform, request *http.Request) (publication.Caller, platform.RequestMetadata, bool) {
	principal, principalOK := PrincipalFromContext(request.Context())
	metadata, metadataOK := MetadataFromContext(request.Context())
	if !principalOK || !metadataOK {
		return publication.Caller{}, platform.RequestMetadata{}, false
	}
	childSpan, err := application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return publication.Caller{}, platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return publication.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes)}, metadata, true
}

func writeDomainError(writer http.ResponseWriter, err error) {
	typed, ok := publication.AsError(err)
	if !ok {
		writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
		return
	}
	status, code := http.StatusBadRequest, "INVALID_PUBLICATION_REQUEST"
	switch typed.Category {
	case publication.CategoryAuthentication:
		status, code = http.StatusUnauthorized, "AUTHENTICATION_REQUIRED"
	case publication.CategoryAuthorization:
		status, code = http.StatusForbidden, "PUBLICATION_FORBIDDEN"
	case publication.CategoryNotFound:
		status, code = http.StatusNotFound, "AGENT_VERSION_NOT_FOUND"
	case publication.CategoryConflict:
		status, code = http.StatusConflict, "AGENT_VERSION_CONFLICT"
	case publication.CategoryCapacity:
		status, code = http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE"
	case publication.CategoryDependency:
		status, code = http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"
	}
	writePublicationError(writer, status, code, string(typed.Category), typed.Retryable)
}

func writePublicationError(writer http.ResponseWriter, status int, code, category string, retryable bool) {
	var retryAfter *controlplane.SafeInteger
	if status == http.StatusUnauthorized {
		writer.Header().Set("WWW-Authenticate", "Bearer")
	}
	if status == http.StatusServiceUnavailable {
		writer.Header().Set("Retry-After", "1")
		value := controlplane.SafeInteger(1)
		retryAfter = &value
	}
	encoded, err := controlplane.EncodeAROPError(controlplane.AROPError{Category: category, Code: code, Message: code, Retryable: retryable, RetryAfterSeconds: retryAfter})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Status: "error"})
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func NewServer(application *platform.Platform, handler http.Handler) *http.Server {
	config := application.Config()
	return &http.Server{
		Addr: config.ListenAddress, Handler: handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout, ReadTimeout: config.ReadTimeout,
		WriteTimeout: config.WriteTimeout, IdleTimeout: config.IdleTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}
}

func instrument(application *platform.Platform, next http.Handler, authenticate AuthenticateFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		config := application.Config()
		ctx, cancel := context.WithTimeout(request.Context(), config.RequestTimeout)
		defer cancel()
		request = request.WithContext(ctx)
		request.Body = http.MaxBytesReader(writer, request.Body, config.MaxBodyBytes)

		operation := classifyOperation(request.Method, request.URL.Path)
		startedAt := application.Now()
		metadata, metadataErr := application.BeginRequest(ctx, request.Header.Values(requestIDHeader), request.Header.Values(traceparentHeader), request.Header.Values(tracestateHeader))
		capture := newResponseCapture()
		if metadata.RequestID != "" {
			capture.Header().Set(requestIDHeader, metadata.RequestID)
		}
		if metadata.Traceparent() != "" {
			capture.Header().Set(traceparentHeader, metadata.Traceparent())
		}
		setSecurityHeaders(capture.Header())
		emptyHealthBody := true
		var healthBodyErr error
		if operation == "health.live" || operation == "health.ready" {
			emptyHealthBody, healthBodyErr = bodyIsEmpty(request.Body)
		}

		switch {
		case metadataErr != nil:
			writeJSON(capture, http.StatusBadRequest, errorResponse{Status: "rejected"})
		case healthBodyErr != nil || !emptyHealthBody:
			writeJSON(capture, http.StatusBadRequest, errorResponse{Status: "rejected"})
		case request.ContentLength > config.MaxBodyBytes:
			if operation == "publication.publish" {
				writePublicationError(capture, http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE", "capacity", false)
			} else {
				writeJSON(capture, http.StatusRequestEntityTooLarge, errorResponse{Status: "rejected"})
			}
		default:
			if authenticate != nil && operation != "health.live" && operation != "health.ready" {
				requiredScopes := scopesForOperation(operation)
				authenticationContext := context.WithValue(ctx, requiredScopesContextKey{}, requiredScopes)
				principal, authenticationErr := authenticateBearer(authenticationContext, request.Header.Values("Authorization"), metadata, authenticate)
				switch {
				case authenticationErr == nil:
					requestContext := context.WithValue(request.Context(), principalContextKey{}, principal)
					requestContext = context.WithValue(requestContext, metadataContextKey{}, metadata)
					request = request.WithContext(requestContext)
					invoke(application, capture, request, next)
				case errors.Is(authenticationErr, ErrAuthenticationUnavailable):
					if isPublicationOperation(operation) {
						writePublicationError(capture, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
					} else {
						writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
					}
				default:
					if isPublicationOperation(operation) {
						writePublicationError(capture, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authentication", false)
					} else {
						writeJSON(capture, http.StatusUnauthorized, errorResponse{Status: "unauthorized"})
					}
				}
			} else {
				invoke(application, capture, request, next)
			}
		}
		if metadata.RequestID != "" {
			capture.Header().Set(requestIDHeader, metadata.RequestID)
		}
		if metadata.Traceparent() != "" {
			capture.Header().Set(traceparentHeader, metadata.Traceparent())
		}

		endedAt := application.Now()
		statusCode := capture.statusCode()
		if metadata.RequestID != "" && metadata.TraceID != "" && metadata.SpanID != "" {
			recordContext, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), config.RequestTimeout)
			recordErr := application.RecordObservation(recordContext, platform.Observation{Metadata: metadata, Operation: operation, StartedAt: startedAt, EndedAt: endedAt, HTTPStatus: statusCode})
			recordCancel()
			if recordErr != nil && statusCode < 500 {
				capture = newResponseCapture()
				setSecurityHeaders(capture.Header())
				capture.Header().Set(requestIDHeader, metadata.RequestID)
				capture.Header().Set(traceparentHeader, metadata.Traceparent())
				if isPublicationOperation(operation) {
					writePublicationError(capture, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
				} else {
					writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
				}
			}
		}
		copyResponse(writer, capture)
	})
}

func isPublicationOperation(operation string) bool {
	return operation == "publication.publish" || operation == "publication.get"
}

func authenticateBearer(ctx context.Context, values []string, metadata platform.RequestMetadata, authenticate AuthenticateFunc) (AuthenticatedPrincipal, error) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return AuthenticatedPrincipal{}, errors.New("invalid authorization")
	}
	credential := strings.TrimPrefix(values[0], "Bearer ")
	if credential == "" || strings.TrimSpace(credential) != credential || strings.ContainsAny(credential, " \t\r\n,") {
		return AuthenticatedPrincipal{}, errors.New("invalid authorization")
	}
	return authenticate(ctx, credential, metadata)
}

func bodyIsEmpty(body io.Reader) (bool, error) {
	if body == nil {
		return true, nil
	}
	content, err := io.ReadAll(io.LimitReader(body, 1))
	if err != nil {
		return false, err
	}
	return len(content) == 0, nil
}

func invoke(application *platform.Platform, writer http.ResponseWriter, request *http.Request, next http.Handler) {
	defer func() {
		if recover() != nil {
			resetResponse(writer)
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Status: "error"})
		}
	}()
	operation := classifyOperation(request.Method, request.URL.Path)
	if operation == "health.live" || operation == "health.ready" || operation == "publication.publish" || operation == "publication.get" {
		next.ServeHTTP(writer, request)
		return
	}
	if err := application.Execute(request.Context(), func(ctx context.Context) error {
		next.ServeHTTP(writer, request.WithContext(ctx))
		return nil
	}); err != nil {
		resetResponse(writer)
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
		return
	}
}

func classifyOperation(method, path string) string {
	switch path {
	case "/v1/health/live":
		return "health.live"
	case "/v1/health/ready":
		return "health.ready"
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "agent-definitions" && segments[3] == "versions" {
		return "publication.publish"
	}
	if method == http.MethodGet && len(segments) == 5 && segments[0] == "v1" && segments[1] == "agent-definitions" && segments[3] == "versions" {
		return "publication.get"
	}
	return "http.unmatched"
}

func scopesForOperation(operation string) []string {
	switch operation {
	case "publication.publish":
		return []string{"agent:publish"}
	case "publication.get":
		return []string{"agent:read"}
	default:
		return []string{"secret.read"}
	}
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseCapture() *responseCapture           { return &responseCapture{header: make(http.Header)} }
func (capture *responseCapture) Header() http.Header { return capture.header }
func (capture *responseCapture) WriteHeader(status int) {
	if capture.status == 0 {
		capture.status = status
	}
}
func (capture *responseCapture) Write(data []byte) (int, error) {
	if capture.status == 0 {
		capture.status = http.StatusOK
	}
	return capture.body.Write(data)
}
func (capture *responseCapture) statusCode() int {
	if capture.status == 0 {
		return http.StatusOK
	}
	return capture.status
}

func resetResponse(writer http.ResponseWriter) {
	if capture, ok := writer.(*responseCapture); ok {
		capture.header = make(http.Header)
		capture.status = 0
		capture.body.Reset()
		setSecurityHeaders(capture.header)
	}
}

func copyResponse(writer http.ResponseWriter, capture *responseCapture) {
	for key, values := range capture.Header() {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(capture.statusCode())
	_, _ = io.Copy(writer, &capture.body)
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
