package httpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
)

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
