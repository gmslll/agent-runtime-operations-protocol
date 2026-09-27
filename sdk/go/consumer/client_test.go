package consumer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dispatchwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

var consumerNow = time.Date(2026, 9, 27, 7, 0, 0, 0, time.UTC)

func TestInvokeRedispatchesExpiredTicketAndUsesSelectedEndpoint(t *testing.T) {
	var dispatches, deliveries atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, ":dispatch"):
			dispatch := dispatches.Add(1)
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer control-token-000000" || request.Header.Get("Idempotency-Key") != fmt.Sprintf("dispatch-key-%03d", dispatch) {
				t.Fatalf("invalid dispatch request: %s %#v", request.URL, request.Header)
			}
			expires := consumerNow.Add(-time.Second)
			attempt := "att_01999999-9999-7999-8999-999999999991"
			if dispatch == 2 {
				expires = consumerNow.Add(2 * time.Minute)
				attempt = "att_01999999-9999-7999-8999-999999999992"
			}
			writeTicket(response, server.URL, "direct", attempt, expires)
		case request.URL.Path == "/v1/runs":
			deliveries.Add(1)
			if request.Header.Get("Authorization") != "Bearer "+testRunToken() || request.Header.Get("Idempotency-Key") != "att_01999999-9999-7999-8999-999999999992" {
				t.Fatalf("invalid direct headers: %#v", request.Header)
			}
			writeStatus(response, http.StatusAccepted)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "direct")
	status, err := client.Invoke(context.Background(), testRunID(), testRunRequest(t))
	if err != nil || string(status.RunID) != testRunID() || dispatches.Load() != 2 || deliveries.Load() != 1 {
		t.Fatalf("invoke status=%#v err=%v dispatch=%d delivery=%d", status, err, dispatches.Load(), deliveries.Load())
	}
}

func TestProxyDeliveryKeepsRuntimeTokenOffAuthorizationHeader(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, ":dispatch"):
			writeTicket(response, server.URL, "proxy", testAttemptID(), consumerNow.Add(time.Minute))
		case strings.HasSuffix(request.URL.Path, ":deliver"):
			if request.Header.Get("Authorization") != "Bearer control-token-000000" || request.Header.Get("X-AROP-Run-Token") != testRunToken() || request.Header.Get("X-AROP-Runtime-Endpoint") != "" {
				t.Fatalf("proxy credentials were not separated: %#v", request.Header)
			}
			writeStatus(response, http.StatusAccepted)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "proxy")
	status, err := client.Invoke(context.Background(), testRunID(), testRunRequest(t))
	if err != nil || string(status.RunID) != testRunID() {
		t.Fatalf("proxy invoke failed: %#v %v", status, err)
	}
}

func TestNetworkPolicyAndRedirectFailClosedWithoutCredentialLeak(t *testing.T) {
	var leaked atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("Idempotency-Key") != "" {
			leaked.Add(1)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	var source *httptest.Server
	source = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, ":dispatch") {
			writeTicket(response, source.URL, "direct", testAttemptID(), consumerNow.Add(time.Minute))
			return
		}
		response.Header().Set("Location", target.URL+"/stolen")
		response.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := testClient(t, source, &countingResolver{}, allowAll{}, "direct")
	_, err := client.Invoke(context.Background(), testRunID(), testRunRequest(t))
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("cross-origin redirect accepted or leaked credentials: err=%v leaked=%d", err, leaked.Load())
	}

	resolver := &countingResolver{}
	blockedClient := testClient(t, source, resolver, denyAll{}, "direct")
	if _, err = blockedClient.Dispatch(context.Background(), testRunID(), "dispatch-key-001"); !errorsIs(err, ErrUnsafeEndpoint) {
		t.Fatalf("private address policy did not fail closed: %v", err)
	}
	if resolver.calls.Load() == 0 {
		t.Fatal("resolver was not consulted")
	}
}

func TestSameOriginRedirectIsRejectedWithoutCredentialReplay(t *testing.T) {
	var redirected atomic.Int64
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, ":dispatch"):
			writeTicket(response, server.URL, "direct", testAttemptID(), consumerNow.Add(time.Minute))
		case request.URL.Path == "/v1/runs":
			response.Header().Set("Location", server.URL+"/v1/redirected")
			response.WriteHeader(http.StatusTemporaryRedirect)
		case request.URL.Path == "/v1/redirected":
			if request.Header.Get("Authorization") != "" || request.Header.Get("Idempotency-Key") != "" {
				redirected.Add(1)
			}
			writeStatus(response, http.StatusAccepted)
		}
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "direct")
	if _, err := client.Invoke(context.Background(), testRunID(), testRunRequest(t)); err == nil || redirected.Load() != 0 {
		t.Fatalf("same-origin redirect accepted or replayed credentials: err=%v replay=%d", err, redirected.Load())
	}
}

func TestDispatchRejectsDuplicateOrMalformedResponseHeaders(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Add("Content-Type", "application/json")
		response.Header().Add("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "direct")
	if _, err := client.Dispatch(context.Background(), testRunID(), "dispatch-key-001"); err == nil {
		t.Fatal("duplicate Content-Type was accepted")
	}
}

func testClient(t *testing.T, server *httptest.Server, resolver Resolver, policy AddressPolicy, mode string) *Client {
	t.Helper()
	httpClient := server.Client()
	transport := httpClient.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	httpClient.Transport = transport
	client, err := New(Config{
		ControlPlaneURL: server.URL, Tokens: staticToken("control-token-000000"), Idempotency: &sequenceKeys{}, Resolver: resolver,
		AddressPolicy: policy, HTTPClient: httpClient, Clock: func() time.Time { return consumerNow }, RequestTimeout: 3 * time.Second, DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type staticToken string

func (token staticToken) Token(context.Context) (string, error) { return string(token), nil }

type sequenceKeys struct{ next atomic.Int64 }

func (keys *sequenceKeys) NewKey(context.Context) (string, error) {
	return fmt.Sprintf("dispatch-key-%03d", keys.next.Add(1)), nil
}

type countingResolver struct{ calls atomic.Int64 }

func (resolver *countingResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	resolver.calls.Add(1)
	return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
}

type allowAll struct{}

func (allowAll) Allow(netip.Addr) bool { return true }

type denyAll struct{}

func (denyAll) Allow(netip.Addr) bool { return false }

func writeTicket(response http.ResponseWriter, endpoint, mode, attempt string, expires time.Time) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Location", "/v1/agent-runs/"+testRunID()+"/attempts/"+attempt)
	response.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"schema_version": 1, "run_id": testRunID(), "attempt_id": attempt, "fencing_token": 1,
		"agent":     map[string]any{"id": "test.agent", "version": "1.0.0", "skill_id": "default", "manifest_digest": "sha256:" + strings.Repeat("a", 64)},
		"delivery":  map[string]any{"mode": mode, "deployment_id": "dep_01999999-9999-7999-8999-999999999997", "instance_id": "runtime-a", "generation": 1, "audience": endpoint + "/deployments/dep_01999999-9999-7999-8999-999999999997", "endpoint": endpoint + "/v1/runs", "expires_at": expires.Format(time.RFC3339Nano)},
		"run_token": testRunToken(), "trace": map[string]any{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	})
}

func writeStatus(response http.ResponseWriter, status int) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{
		"schema_version": 1, "run_id": testRunID(), "state": "running", "state_version": 2,
		"agent":                         map[string]any{"id": "test.agent", "version": "1.0.0", "skill_id": "default", "manifest_digest": "sha256:" + strings.Repeat("a", 64)},
		"authorization_snapshot_digest": "sha256:" + strings.Repeat("b", 64), "created_at": consumerNow.Format(time.RFC3339Nano), "updated_at": consumerNow.Format(time.RFC3339Nano), "deadline_at": consumerNow.Add(time.Hour).Format(time.RFC3339Nano),
		"trace": map[string]any{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	})
}

func testRunRequest(t *testing.T) runwire.RunRequest {
	t.Helper()
	value, err := runwire.DecodeRunRequest([]byte(`{"schema_version":1,"agent":{"id":"test.agent","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"text","text":"hello"}],"deadline_at":"2026-09-27T08:00:00Z","effects":{"level":"none"},"trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testRunToken() string {
	return strings.Repeat("a", 40) + "." + strings.Repeat("b", 40) + "." + strings.Repeat("c", 40)
}
func testRunID() string     { return "run_01999999-9999-7999-8999-999999999999" }
func testAttemptID() string { return "att_01999999-9999-7999-8999-999999999998" }

func errorsIs(err, target error) bool {
	return err != nil && (err == target || strings.Contains(err.Error(), target.Error()))
}

var _ dispatchwire.DispatchTicket
