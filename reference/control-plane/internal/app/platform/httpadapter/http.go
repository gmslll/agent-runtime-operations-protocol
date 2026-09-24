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
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
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
}

type AuthenticateFunc func(context.Context, string, platform.RequestMetadata) (AuthenticatedPrincipal, error)

type principalContextKey struct{}

func PrincipalFromContext(ctx context.Context) (AuthenticatedPrincipal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(AuthenticatedPrincipal)
	return principal, ok
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

func newHandler(application *platform.Platform, authenticate AuthenticateFunc) (http.Handler, error) {
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
	return instrument(application, mux, authenticate), nil
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

		operation := classifyOperation(request.URL.Path)
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
			writeJSON(capture, http.StatusRequestEntityTooLarge, errorResponse{Status: "rejected"})
		default:
			if authenticate != nil && operation != "health.live" && operation != "health.ready" {
				principal, authenticationErr := authenticateBearer(ctx, request.Header.Values("Authorization"), metadata, authenticate)
				switch {
				case authenticationErr == nil:
					request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
					invoke(application, capture, request, next)
				case errors.Is(authenticationErr, ErrAuthenticationUnavailable):
					writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
				default:
					writeJSON(capture, http.StatusUnauthorized, errorResponse{Status: "unauthorized"})
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
				writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
			}
		}
		copyResponse(writer, capture)
	})
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
	operation := classifyOperation(request.URL.Path)
	if operation == "health.live" || operation == "health.ready" {
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

func classifyOperation(path string) string {
	switch path {
	case "/v1/health/live":
		return "health.live"
	case "/v1/health/ready":
		return "health.ready"
	default:
		return "http.unmatched"
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
