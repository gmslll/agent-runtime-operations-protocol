package dispatch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	generated "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
)

type testHTTPCallers struct{ reject error }

func (provider testHTTPCallers) Authenticate(*http.Request) (Caller, error) {
	if provider.reject != nil {
		return Caller{}, provider.reject
	}
	return validDispatchRequest().Caller, nil
}

func (testHTTPCallers) Metadata(*http.Request) (platform.RequestMetadata, bool) {
	return validDispatchRequest().Metadata, true
}

func TestDispatchHTTPContractAndPublicJWKS(t *testing.T) {
	service, _, _, _ := testService(t)
	handler, err := NewHTTPHandler(service, testHTTPCallers{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+validDispatchRequest().RunID+":dispatch", nil)
	request.Header.Set("Idempotency-Key", validDispatchRequest().IdempotencyKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Location") == "" {
		t.Fatalf("dispatch response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	ticket, err := generated.DecodeDispatchTicket(response.Body.Bytes())
	if err != nil || string(ticket.RunID) != validDispatchRequest().RunID || ticket.RunToken == "" {
		t.Fatalf("ticket=%#v err=%v", ticket, err)
	}
	jwksRequest := httptest.NewRequest(http.MethodGet, "/.well-known/arop-jwks.json", nil)
	jwksResponse := httptest.NewRecorder()
	handler.ServeHTTP(jwksResponse, jwksRequest)
	if jwksResponse.Code != http.StatusOK || jwksResponse.Header().Get("Cache-Control") == "" {
		t.Fatalf("JWKS response=%d headers=%v body=%s", jwksResponse.Code, jwksResponse.Header(), jwksResponse.Body.String())
	}
	jwks, err := generated.DecodeJWKSMetadata(jwksResponse.Body.Bytes())
	if err != nil || len(jwks.Keys) != 1 || jwks.Keys[0].Kid == "" {
		t.Fatalf("JWKS=%#v err=%v", jwks, err)
	}
}

func TestDispatchHTTPRejectsAuthenticationDuplicateHeadersBodyAndQuery(t *testing.T) {
	service, _, _, _ := testService(t)
	tests := []struct {
		name        string
		callers     testHTTPCallers
		target      string
		body        string
		idempotency []string
		status      int
	}{
		{name: "authentication", callers: testHTTPCallers{reject: NewError(CategoryAuthentication, ReasonAuthenticationRequired)}, target: "/v1/agent-runs/" + validDispatchRequest().RunID + ":dispatch", idempotency: []string{validDispatchRequest().IdempotencyKey}, status: 401},
		{name: "duplicate-idempotency", target: "/v1/agent-runs/" + validDispatchRequest().RunID + ":dispatch", idempotency: []string{"dispatch-key-001", "dispatch-key-002"}, status: 400},
		{name: "body", target: "/v1/agent-runs/" + validDispatchRequest().RunID + ":dispatch", body: `{}`, idempotency: []string{validDispatchRequest().IdempotencyKey}, status: 400},
		{name: "query", target: "/v1/agent-runs/" + validDispatchRequest().RunID + ":dispatch?x=1", idempotency: []string{validDispatchRequest().IdempotencyKey}, status: 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := NewHTTPHandler(service, test.callers)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.body))
			for _, value := range test.idempotency {
				request.Header.Add("Idempotency-Key", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status == 401 && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("401 response omitted challenge")
			}
		})
	}
}
