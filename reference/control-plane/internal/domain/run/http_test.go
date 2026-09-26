package run

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	generated "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

type testCallerProvider struct{ failure error }

func (provider testCallerProvider) Authenticate(*http.Request, Operation) (Caller, error) {
	if provider.failure != nil {
		return Caller{}, provider.failure
	}
	return caller(), nil
}

func TestHTTPCreateGetCancelContract(t *testing.T) {
	service, _, repository, _ := newTestService(t)
	handler, err := NewHTTPHandler(service, testCallerProvider{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("../../../../../conformance/fixtures/run/run-request.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-key-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 201 {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	status, err := generated.DecodeRunStatus(response.Body.Bytes())
	if err != nil {
		t.Fatalf("invalid public status: %v body=%s", err, response.Body.String())
	}
	runID := string(status.RunID)
	if response.Header().Get("Location") != "/v1/agent-runs/"+runID {
		t.Fatal("location not bound to run")
	}
	stored := repository.runs[key("acme", runID)]
	if string(stored.Labels) != `{"priority":"normal"}` {
		t.Fatalf("wire labels were not preserved: %s", stored.Labels)
	}
	get := httptest.NewRequest(http.MethodGet, "/v1/agent-runs/"+runID, nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != 200 {
		t.Fatalf("get status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	if _, err = generated.DecodeRunStatus(getResponse.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	command := []byte(`{"schema_version":1,"command_id":"cmd_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","type":"run.cancel","expected_state_version":1,"data":{"reason":"operator"}}`)
	cancel := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+runID+"/commands", bytes.NewReader(command))
	cancel.Header.Set("Content-Type", "application/json")
	cancel.Header.Set("Idempotency-Key", "command-key-0001")
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, cancel)
	if cancelResponse.Code != 200 {
		t.Fatalf("cancel status=%d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}
	cancelled, err := generated.DecodeRunStatus(cancelResponse.Body.Bytes())
	if err != nil || cancelled.State != "cancel_requested" {
		t.Fatalf("cancel wire invalid: %#v %v", cancelled, err)
	}
}

func TestHTTPFailsClosedOnAuthenticationAndMediaType(t *testing.T) {
	service, _, _, _ := newTestService(t)
	handler, _ := NewHTTPHandler(service, testCallerProvider{failure: NewError(CategoryAuthentication, ReasonAuthenticationRequired)})
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("authentication contract missing: %d %#v", response.Code, response.Header())
	}
	handler, _ = NewHTTPHandler(service, testCallerProvider{})
	request = httptest.NewRequest(http.MethodPost, "/v1/agent-runs", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "text/plain")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 415 {
		t.Fatalf("media type accepted: %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/agent-runs", bytes.NewReader(bytes.Repeat([]byte("x"), maxRunRequestBytes+1)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-key-oversize")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "RUN_REQUEST_TOO_LARGE") {
		t.Fatalf("oversize body contract missing: status=%d body=%s", response.Code, response.Body.String())
	}
}
