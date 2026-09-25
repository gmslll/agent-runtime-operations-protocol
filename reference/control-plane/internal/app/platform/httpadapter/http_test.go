package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
)

type publicationStub struct {
	publish func(context.Context, publication.PublishRequest) (publication.PublishResult, error)
	get     func(context.Context, publication.GetRequest) (publication.GetResult, error)
}

func (stub publicationStub) Publish(ctx context.Context, request publication.PublishRequest) (publication.PublishResult, error) {
	return stub.publish(ctx, request)
}
func (stub publicationStub) Get(ctx context.Context, request publication.GetRequest) (publication.GetResult, error) {
	return stub.get(ctx, request)
}

func TestPublicationRoutesEnforceScopeUoWBoundaryAndBodyLimit(t *testing.T) {
	clock := platform.RealClock{}
	store, err := memory.New(100, 100)
	if err != nil {
		t.Fatal(err)
	}
	config := platform.DefaultConfig()
	config.MaxBodyBytes = publication.MaxBundleBytes
	application, err := platform.New(config, platform.Dependencies{Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{}, UoW: &platform.SerialUnitOfWork{}, Observability: store}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	var requestedScopes []string
	authenticate := func(ctx context.Context, credential string, _ platform.RequestMetadata) (AuthenticatedPrincipal, error) {
		requestedScopes = RequiredScopesFromContext(ctx)
		switch credential {
		case "valid-token":
			return AuthenticatedPrincipal{TenantID: "tenant-a", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000002", Scopes: append([]string(nil), requestedScopes...)}, nil
		case "dependency-down":
			return AuthenticatedPrincipal{}, ErrAuthenticationUnavailable
		default:
			return AuthenticatedPrincipal{}, errors.New("rejected")
		}
	}
	var publishRequest publication.PublishRequest
	service := publicationStub{
		publish: func(_ context.Context, request publication.PublishRequest) (publication.PublishResult, error) {
			publishRequest = request
			return publication.PublishResult{AgentID: request.AgentID, Version: "1.0.0", ManifestDigest: "sha256:" + strings.Repeat("a", 64), Location: "/v1/agent-definitions/hello.agent/versions/1.0.0", ETag: `"sha256:` + strings.Repeat("a", 64) + `"`}, nil
		},
		get: func(_ context.Context, _ publication.GetRequest) (publication.GetResult, error) {
			return publication.GetResult{}, publication.NewError(publication.CategoryAuthorization, publication.ReasonPublicationForbidden)
		},
	}
	handler, err := NewPublicationHandler(application, authenticate, service)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-definitions/hello.agent/versions", bytes.NewReader([]byte("bundle")))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/vnd.arop.agent-version-bundle+zip")
	request.Header.Set("Idempotency-Key", "request-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !slices.Equal(requestedScopes, []string{"agent:publish"}) || !slices.Equal(publishRequest.Caller.Scopes, []string{"agent:publish"}) || publishRequest.Metadata.ParentSpanID == "" || publishRequest.Metadata.SpanID == "" || publishRequest.Metadata.ParentSpanID == publishRequest.Metadata.SpanID {
		t.Fatalf("publish route boundary failed: status=%d scopes=%v request=%+v", response.Code, requestedScopes, publishRequest)
	}

	get := httptest.NewRequest(http.MethodGet, "/v1/agent-definitions/hello.agent/versions/1.0.0", nil)
	get.Header.Set("Authorization", "Bearer valid-token")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusForbidden || !slices.Equal(requestedScopes, []string{"agent:read"}) || !strings.Contains(getResponse.Body.String(), `"code":"PUBLICATION_FORBIDDEN"`) {
		t.Fatalf("get authorization mapping failed: status=%d scopes=%v body=%s", getResponse.Code, requestedScopes, getResponse.Body.String())
	}
	for credential, status := range map[string]int{"rejected": http.StatusUnauthorized, "dependency-down": http.StatusServiceUnavailable} {
		probe := httptest.NewRequest(http.MethodGet, "/v1/agent-definitions/hello.agent/versions/1.0.0", nil)
		probe.Header.Set("Authorization", "Bearer "+credential)
		probeResponse := httptest.NewRecorder()
		handler.ServeHTTP(probeResponse, probe)
		if probeResponse.Code != status || strings.Contains(probeResponse.Body.String(), credential) {
			t.Fatalf("authentication mapping status=%d want=%d body=%s", probeResponse.Code, status, probeResponse.Body.String())
		}
	}

	for _, size := range []int{publication.MaxBundleBytes - 1, publication.MaxBundleBytes} {
		probe := httptest.NewRequest(http.MethodPost, "/v1/agent-definitions/hello.agent/versions", bytes.NewReader(bytes.Repeat([]byte{'x'}, size)))
		probe.Header.Set("Authorization", "Bearer valid-token")
		probe.Header.Set("Content-Type", "application/vnd.arop.agent-version-bundle+zip")
		probe.Header.Set("Idempotency-Key", "request-0001")
		probeResponse := httptest.NewRecorder()
		handler.ServeHTTP(probeResponse, probe)
		if probeResponse.Code != http.StatusCreated {
			t.Fatalf("body size %d rejected: %d", size, probeResponse.Code)
		}
	}
	for _, chunked := range []bool{false, true} {
		probe := httptest.NewRequest(http.MethodPost, "/v1/agent-definitions/hello.agent/versions", bytes.NewReader(bytes.Repeat([]byte{'x'}, publication.MaxBundleBytes+1)))
		probe.Header.Set("Authorization", "Bearer valid-token")
		probe.Header.Set("Content-Type", "application/vnd.arop.agent-version-bundle+zip")
		probe.Header.Set("Idempotency-Key", "request-0001")
		if chunked {
			probe.ContentLength = -1
			probe.TransferEncoding = []string{"chunked"}
		}
		probeResponse := httptest.NewRecorder()
		handler.ServeHTTP(probeResponse, probe)
		if probeResponse.Code != http.StatusRequestEntityTooLarge || !strings.Contains(probeResponse.Body.String(), `"code":"BUNDLE_TOO_LARGE"`) {
			t.Fatalf("oversize chunked=%t status=%d body=%s", chunked, probeResponse.Code, probeResponse.Body.String())
		}
	}
}

func TestHandlerChainPropagatesCorrelatesAndRedacts(t *testing.T) {
	t.Parallel()
	store, err := memory.New(50, 50)
	if err != nil {
		t.Fatal(err)
	}
	clock := platform.RealClock{}
	application, err := platform.New(platform.DefaultConfig(), platform.Dependencies{
		Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{},
		UoW: &platform.SerialUnitOfWork{}, Observability: store,
	}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(application)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("authenticated-chain-fails-closed-before-usecase", func(t *testing.T) {
		calls, nextCalls := 0, 0
		authenticate := func(_ context.Context, credential string, metadata platform.RequestMetadata) (AuthenticatedPrincipal, error) {
			calls++
			if metadata.RequestID == "" || metadata.TraceID == "" || metadata.SpanID == "" {
				t.Fatal("authentication ran before request/trace metadata")
			}
			switch credential {
			case "valid-reference-token":
				return AuthenticatedPrincipal{TenantID: "reference", PrincipalID: "principal", SubjectID: "subject", CredentialID: "credential"}, nil
			case "dependency-down":
				return AuthenticatedPrincipal{}, ErrAuthenticationUnavailable
			default:
				return AuthenticatedPrincipal{}, errors.New("credential rejected")
			}
		}
		next := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			nextCalls++
			if request.URL.Path == "/v1/health/live" {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			principal, ok := PrincipalFromContext(request.Context())
			if !ok || principal.TenantID != "reference" || principal.CredentialID != "credential" {
				t.Fatal("authenticated principal missing from request context")
			}
			writer.WriteHeader(http.StatusNoContent)
		})
		authenticated := instrument(application, next, authenticate)
		for _, test := range []struct {
			name    string
			headers []string
			status  int
		}{
			{"missing", nil, http.StatusUnauthorized},
			{"wrong-scheme", []string{"Basic value"}, http.StatusUnauthorized},
			{"empty", []string{"Bearer "}, http.StatusUnauthorized},
			{"whitespace", []string{"Bearer invalid value"}, http.StatusUnauthorized},
			{"duplicate", []string{"Bearer valid-reference-token", "Bearer other"}, http.StatusUnauthorized},
			{"rejected", []string{"Bearer rejected"}, http.StatusUnauthorized},
			{"unavailable", []string{"Bearer dependency-down"}, http.StatusServiceUnavailable},
			{"valid", []string{"Bearer valid-reference-token"}, http.StatusNoContent},
		} {
			t.Run(test.name, func(t *testing.T) {
				beforeNext := nextCalls
				request := httptest.NewRequest(http.MethodGet, "/v1/reference-only", nil)
				for _, value := range test.headers {
					request.Header.Add("Authorization", value)
				}
				response := httptest.NewRecorder()
				authenticated.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if test.name != "valid" && nextCalls != beforeNext {
					t.Fatal("unauthenticated request reached use case")
				}
				if strings.Contains(response.Body.String(), "rejected") || strings.Contains(response.Body.String(), "dependency-down") {
					t.Fatal("authentication response leaked credential detail")
				}
			})
		}
		beforeCalls := calls
		health := httptest.NewRecorder()
		authenticated.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health/live", nil))
		if health.Code != http.StatusNoContent || calls != beforeCalls {
			t.Fatal("health request did not bypass authentication")
		}
	})

	callerRequestID := "req_01956e7b-9abc-7def-8abc-0123456789ab"
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	parentSpan := "00f067aa0ba902b7"
	request := httptest.NewRequest(http.MethodGet, "/v1/health/live?token=p08-secret-value", nil)
	request.Header.Set(requestIDHeader, callerRequestID)
	request.Header.Set(traceparentHeader, "00-"+traceID+"-"+parentSpan+"-01")
	request.Header.Set("Authorization", "Bearer p08-bearer-value")
	request.Header.Set("Cookie", "session=p08-cookie-value")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("live status=%d body=%s", response.Code, response.Body.String())
	}
	serverRequestID := response.Header().Get(requestIDHeader)
	if serverRequestID == "" || serverRequestID == callerRequestID {
		t.Fatalf("server did not mint an authoritative request ID: %q", serverRequestID)
	}
	if got := response.Header().Get(traceparentHeader); !strings.HasPrefix(got, "00-"+traceID+"-") || !strings.HasSuffix(got, "-01") {
		t.Fatalf("trace context was not propagated: %q", got)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
	t.Run("incoming-client-request-id-not-authoritative", func(t *testing.T) {
		first := httptest.NewRequest(http.MethodGet, "/v1/health/live", nil)
		first.Header.Set(requestIDHeader, callerRequestID)
		firstResponse := httptest.NewRecorder()
		handler.ServeHTTP(firstResponse, first)
		second := httptest.NewRequest(http.MethodGet, "/v1/health/live", nil)
		second.Header.Set(requestIDHeader, callerRequestID)
		secondResponse := httptest.NewRecorder()
		handler.ServeHTTP(secondResponse, second)
		firstID, secondID := firstResponse.Header().Get(requestIDHeader), secondResponse.Header().Get(requestIDHeader)
		if firstResponse.Code != http.StatusOK || secondResponse.Code != http.StatusOK || firstID == "" || secondID == "" || firstID == secondID || firstID == callerRequestID || secondID == callerRequestID {
			t.Fatalf("caller correlation ID influenced server authority: status=%d/%d first=%q second=%q", firstResponse.Code, secondResponse.Code, firstID, secondID)
		}
	})

	readyRequest := httptest.NewRequest(http.MethodGet, "/v1/health/ready", nil)
	readyResponse := httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, readyRequest)
	if readyResponse.Code != http.StatusOK || !strings.Contains(readyResponse.Body.String(), `"durability":"ephemeral"`) {
		t.Fatalf("ready response=%d %s", readyResponse.Code, readyResponse.Body.String())
	}

	badTrace := httptest.NewRequest(http.MethodGet, "/v1/health/live", nil)
	badTrace.Header.Set(traceparentHeader, "00-00000000000000000000000000000000-00f067aa0ba902b7-01")
	badTraceResponse := httptest.NewRecorder()
	handler.ServeHTTP(badTraceResponse, badTrace)
	if badTraceResponse.Code != http.StatusBadRequest {
		t.Fatalf("bad trace status=%d", badTraceResponse.Code)
	}

	badRequestID := httptest.NewRequest(http.MethodGet, "/v1/health/live", nil)
	badRequestID.Header.Add(requestIDHeader, callerRequestID)
	badRequestID.Header.Add(requestIDHeader, callerRequestID)
	badRequestIDResponse := httptest.NewRecorder()
	handler.ServeHTTP(badRequestIDResponse, badRequestID)
	if badRequestIDResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate request ID status=%d", badRequestIDResponse.Code)
	}

	audits, err := application.AuditForRequest(context.Background(), serverRequestID)
	if err != nil || len(audits) != 1 || audits[0].TraceID != traceID || audits[0].Operation != "health.live" {
		t.Fatalf("correlated audit=%#v err=%v", audits, err)
	}
	spans, err := application.TracesForRequest(context.Background(), serverRequestID)
	if err != nil || len(spans) != 1 || spans[0].TraceID != traceID || spans[0].ParentSpanID != parentSpan {
		t.Fatalf("correlated trace=%#v err=%v", spans, err)
	}
	encoded, err := json.Marshal(struct {
		Audits   any
		Spans    any
		Response string
	}{audits, spans, response.Body.String()})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"p08-secret-value", "p08-bearer-value", "p08-cookie-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("observability or response leaked %q: %s", secret, encoded)
		}
	}

	application.SetDraining(true)
	draining := httptest.NewRecorder()
	handler.ServeHTTP(draining, httptest.NewRequest(http.MethodGet, "/v1/health/ready", nil))
	if draining.Code != http.StatusServiceUnavailable || !strings.Contains(draining.Body.String(), `"status":"not_ready"`) {
		t.Fatalf("draining response=%d %s", draining.Code, draining.Body.String())
	}

	timeoutConfig := platform.DefaultConfig()
	timeoutConfig.RequestTimeout = time.Millisecond
	timeoutStore, err := memory.New(10, 10)
	if err != nil {
		t.Fatal(err)
	}
	timeoutApplication, err := platform.New(timeoutConfig, platform.Dependencies{
		Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{},
		UoW: &platform.SerialUnitOfWork{}, Observability: timeoutStore,
		Checks: []platform.ReadinessCheck{platform.ReadinessCheckFunc{CheckName: "deadline", Func: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
	}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	timeoutHandler, err := NewHandler(timeoutApplication)
	if err != nil {
		t.Fatal(err)
	}
	timedOut := httptest.NewRecorder()
	timeoutHandler.ServeHTTP(timedOut, httptest.NewRequest(http.MethodGet, "/v1/health/ready", nil))
	if timedOut.Code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out readiness status=%d body=%s", timedOut.Code, timedOut.Body.String())
	}

	t.Run("health-body-boundary-empty-vs-chunked-byte", func(t *testing.T) {
		emptyBodyResponse := httptest.NewRecorder()
		handler.ServeHTTP(emptyBodyResponse, httptest.NewRequest(http.MethodGet, "/v1/health/live", nil))
		if emptyBodyResponse.Code != http.StatusOK {
			t.Fatalf("health N body status=%d body=%s", emptyBodyResponse.Code, emptyBodyResponse.Body.String())
		}
		chunkedBody := httptest.NewRequest(http.MethodGet, "/v1/health/live", strings.NewReader("x"))
		chunkedBody.ContentLength = -1
		chunkedBody.TransferEncoding = []string{"chunked"}
		chunkedBodyResponse := httptest.NewRecorder()
		handler.ServeHTTP(chunkedBodyResponse, chunkedBody)
		if chunkedBodyResponse.Code != http.StatusBadRequest {
			t.Fatalf("health N+1 chunked body status=%d body=%s", chunkedBodyResponse.Code, chunkedBodyResponse.Body.String())
		}
	})
}
