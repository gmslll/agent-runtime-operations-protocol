package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	domainrun "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

var deliveryNow = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func TestServiceBindsTicketRunRequestAndProviderResponse(t *testing.T) {
	validator := fakeValidator{attempt: validAttempt(), view: validView()}
	forwarder := &fakeForwarder{response: validStatus()}
	service, err := New(validator, forwarder, func() time.Time { return deliveryNow })
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Caller: validCaller(), RunID: validator.attempt.RunID, AttemptID: validator.attempt.AttemptID, RunToken: strings.Repeat("token", 24), Body: validRequest()}
	status, err := service.Deliver(context.Background(), request)
	if err != nil || string(status.RunID) != request.RunID || forwarder.calls.Load() != 1 || forwarder.attempt.Endpoint != validator.attempt.Endpoint || forwarder.token != request.RunToken {
		t.Fatalf("valid proxy delivery failed: %#v %v", status, err)
	}

	mutated := request
	mutated.Body = bytes.Replace(request.Body, []byte(`"manifest_digest":"sha256:aaaaaaaa`), []byte(`"manifest_digest":"sha256:baaaaaaa`), 1)
	if _, err = service.Deliver(context.Background(), mutated); err == nil || forwarder.calls.Load() != 1 {
		t.Fatalf("mutated request forwarded: err=%v calls=%d", err, forwarder.calls.Load())
	}
	expiredValidator := validator
	expiredValidator.attempt.TicketExpiresAt = deliveryNow
	expired, _ := New(expiredValidator, forwarder, func() time.Time { return deliveryNow })
	if _, err = expired.Deliver(context.Background(), request); err == nil {
		t.Fatal("expired ticket forwarded")
	}
}

func TestHTTPHandlerAuthenticatesAndNeverUsesCallerEndpoint(t *testing.T) {
	validator := fakeValidator{attempt: validAttempt(), view: validView()}
	forwarder := &fakeForwarder{response: validStatus()}
	service, _ := New(validator, forwarder, func() time.Time { return deliveryNow })
	handler, err := NewHTTPHandler(service, fakeCallers{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+validator.attempt.RunID+":deliver", bytes.NewReader(validRequest()))
	request.Header.Set("Authorization", "Bearer control-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", validator.attempt.AttemptID)
	request.Header.Set("X-AROP-Run-Token", strings.Repeat("token", 24))
	request.Header.Set("X-AROP-Runtime-Endpoint", "https://attacker.example/v1/runs")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || forwarder.attempt.Endpoint != validator.attempt.Endpoint {
		t.Fatalf("proxy handler failed closed incorrectly: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+validator.attempt.RunID+":deliver", bytes.NewReader(validRequest()))
	request.Header["Content-Type"] = []string{"application/json", "application/json"}
	request.Header.Set("Idempotency-Key", validator.attempt.AttemptID)
	request.Header.Set("X-AROP-Run-Token", strings.Repeat("token", 24))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate content type accepted: %d", response.Code)
	}
}

func TestHTTPForwarderRechecksAddressAndRejectsCrossOriginRedirect(t *testing.T) {
	var leaked atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Location", target.URL+"/steal")
		response.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := source.Client()
	forwarder, err := NewHTTPForwarder(client, loopbackResolver{}, allowLoopback{})
	if err != nil {
		t.Fatal(err)
	}
	attempt := validAttempt()
	attempt.Endpoint = source.URL + "/v1/runs"
	if _, err = forwarder.Deliver(context.Background(), attempt, validRequest(), strings.Repeat("token", 24)); err == nil || leaked.Load() != 0 {
		t.Fatalf("cross-origin redirect accepted or leaked: %v %d", err, leaked.Load())
	}
	blocked, _ := NewHTTPForwarder(client, loopbackResolver{}, denyAddresses{})
	if _, err = blocked.Deliver(context.Background(), attempt, validRequest(), strings.Repeat("token", 24)); err == nil {
		t.Fatal("blocked address was dialed")
	}
}

func TestHTTPForwarderRejectsSameOriginRedirectWithoutCredentialReplay(t *testing.T) {
	var replayed atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/runs" {
			response.Header().Set("Location", server.URL+"/redirected")
			response.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Idempotency-Key") != "" {
			replayed.Add(1)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusAccepted)
		_, _ = response.Write(validStatus())
	}))
	defer server.Close()
	forwarder, err := NewHTTPForwarder(server.Client(), loopbackResolver{}, allowLoopback{})
	if err != nil {
		t.Fatal(err)
	}
	attempt := validAttempt()
	attempt.Endpoint = server.URL + "/v1/runs"
	if _, err = forwarder.Deliver(context.Background(), attempt, validRequest(), strings.Repeat("token", 24)); err == nil || replayed.Load() != 0 {
		t.Fatalf("same-origin redirect accepted or replayed credentials: %v %d", err, replayed.Load())
	}
}

type fakeValidator struct {
	attempt dispatch.Attempt
	view    dispatch.RunView
	err     error
}

func (validator fakeValidator) ValidateProxyDelivery(_ context.Context, caller dispatch.Caller, runID, attemptID, token string) (dispatch.Attempt, dispatch.RunView, error) {
	if validator.err != nil {
		return dispatch.Attempt{}, dispatch.RunView{}, validator.err
	}
	if caller.TenantID != validator.attempt.TenantID || runID != validator.attempt.RunID || attemptID != validator.attempt.AttemptID || len(token) < 96 {
		return dispatch.Attempt{}, dispatch.RunView{}, errors.New("ticket mismatch")
	}
	return validator.attempt, validator.view, nil
}

type fakeForwarder struct {
	response []byte
	attempt  dispatch.Attempt
	token    string
	calls    atomic.Int64
}

func (forwarder *fakeForwarder) Deliver(_ context.Context, attempt dispatch.Attempt, _ []byte, token string) ([]byte, error) {
	forwarder.calls.Add(1)
	forwarder.attempt, forwarder.token = attempt, token
	return append([]byte(nil), forwarder.response...), nil
}

type fakeCallers struct{}

func (fakeCallers) Authenticate(request *http.Request) (dispatch.Caller, error) {
	if request.Header.Get("Authorization") == "" {
		return dispatch.Caller{}, errors.New("missing auth")
	}
	return validCaller(), nil
}

type loopbackResolver struct{}

func (loopbackResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
}

type allowLoopback struct{}

func (allowLoopback) Allow(netip.Addr) bool { return true }

type denyAddresses struct{}

func (denyAddresses) Allow(netip.Addr) bool { return false }

func validCaller() dispatch.Caller {
	return dispatch.Caller{TenantID: "acme", PrincipalID: "prn_01999999-9999-7999-8999-999999999995", CredentialID: "cred_01999999-9999-7999-8999-999999999994", Scopes: []string{"run:deliver"}}
}

func validView() dispatch.RunView {
	return dispatch.RunView{
		TenantID: "acme", RunID: "run_01999999-9999-7999-8999-999999999999",
		Agent: domainrun.AgentBinding{ID: "test.agent", Version: "1.0.0", SkillID: "default", ManifestDigest: "sha256:" + strings.Repeat("a", 64)},
		State: domainrun.StateDispatching, StateVersion: 2, AuthorizationSnapshotHash: "sha256:" + strings.Repeat("b", 64),
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", DeadlineAt: deliveryNow.Add(time.Hour),
	}
}

func validAttempt() dispatch.Attempt {
	return dispatch.Attempt{
		TenantID: "acme", RunID: validView().RunID, AttemptID: "att_01999999-9999-7999-8999-999999999998", AttemptNumber: 1, FencingToken: 1,
		TokenID: "tok_01999999-9999-7999-8999-999999999993", DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a", SessionID: "ses_01999999-9999-7999-8999-999999999992", ServiceID: "runtime.service",
		Generation: 1, RegistryResourceVersion: 1, State: dispatch.StateIssued, TransportProfile: "proxy", Endpoint: "https://runtime.example.invalid/v1/runs", Audience: "https://control.example.invalid/deployments/dep_01999999-9999-7999-8999-999999999997", SigningKeyID: "dispatch-key-01",
		LeaseExpiresAt: deliveryNow.Add(10 * time.Minute), TicketExpiresAt: deliveryNow.Add(4 * time.Minute), CreatedAt: deliveryNow.Add(-time.Minute), Traceparent: validView().Traceparent,
		IdempotencyKeyHash: strings.Repeat("c", 64), IdempotencyRequestHash: "sha256:" + strings.Repeat("d", 64),
	}
}

func validRequest() []byte {
	return []byte(`{"schema_version":1,"agent":{"id":"test.agent","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"text","text":"hello"}],"deadline_at":"2026-09-27T11:00:00Z","effects":{"level":"none"},"trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}`)
}

func validStatus() []byte {
	value := map[string]any{
		"schema_version": 1, "run_id": validView().RunID, "state": "running", "state_version": 3,
		"agent":                         map[string]any{"id": "test.agent", "version": "1.0.0", "skill_id": "default", "manifest_digest": "sha256:" + strings.Repeat("a", 64)},
		"authorization_snapshot_digest": "sha256:" + strings.Repeat("b", 64), "created_at": deliveryNow.Format(time.RFC3339Nano), "updated_at": deliveryNow.Format(time.RFC3339Nano), "deadline_at": deliveryNow.Add(time.Hour).Format(time.RFC3339Nano),
		"trace": map[string]any{"traceparent": validView().Traceparent},
	}
	encoded, _ := json.Marshal(value)
	return encoded
}
