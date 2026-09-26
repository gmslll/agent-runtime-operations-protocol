package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registrywire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/registry"
)

const (
	testInstanceJSON = `{"schema_version":1,"instance_id":"runtime-a","session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","service_id":"image-runtime","environment":"production","generation":1,"resource_version":1,"registry_revision":1,"lease_id":"lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","lease_expires_at":"2026-09-26T08:00:30Z","endpoint":{"base_url":"https://runtime.internal.example","health_path":"/v1/health/ready"},"bindings":[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"runtime":{"healthy":true,"ready":true,"runtime_version":"2026.09.26","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":1,"available_slots":1,"queue_depth":0},"labels":{"region":"cn-east"}},"operator":{"enabled":true,"weight":100,"priority":10},"draining":false,"status":"registered"}`
	testLeaseJSON    = `{"schema_version":1,"lease_id":"lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","instance_id":"runtime-a","session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","generation":1,"heartbeat_sequence":2,"reported_at":"2026-09-26T07:59:59Z","server_time":"2026-09-26T08:00:00Z","lease_expires_at":"2026-09-26T08:00:30Z","registry_revision":2,"lease_ttl_seconds":30,"keepalive_interval_seconds":10}`
	testSnapshotJSON = `{"schema_version":1,"revision":2,"compaction_watermark":0,"server_time":"2026-09-26T08:00:00Z","instances":[` + testInstanceJSON + `]}`
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestKeepaliveUsesExactWireAndSingleRoundTrip(t *testing.T) {
	var calls atomic.Int32
	client := testClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.EscapedPath() != "/control/v1/registry/leases/lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c/keepalive" || request.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("unexpected request %s %s %#v", request.Method, request.URL.String(), request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"schema_version":1,"instance_id":"runtime-a","session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","generation":1,"heartbeat_sequence":2,"reported_at":"2026-09-26T07:59:59Z","ready":true,"active_runs":1,"available_slots":1,"queue_depth":0}`
		if string(body) != want {
			t.Fatalf("keepalive body=%s want=%s", body, want)
		}
		return jsonResponse(http.StatusOK, testLeaseJSON), nil
	}))
	lease, err := client.Keepalive(context.Background(), KeepaliveRequest{
		InstanceID: "runtime-a", SessionID: "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c",
		LeaseID: "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Generation: 1,
		HeartbeatSequence: 2, ReportedAt: time.Date(2026, 9, 26, 7, 59, 59, 0, time.UTC),
		Ready: true, ActiveRuns: 1, AvailableSlots: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || uint64(lease.HeartbeatSequence) != 2 {
		t.Fatalf("calls=%d lease=%#v", calls.Load(), lease)
	}
}

func TestDiscoverEncodesExactTenantSafeQuery(t *testing.T) {
	client := testClient(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.EscapedPath() != "/control/v1/discovery/agents/image.generate/instances" || request.URL.RawQuery != "protocol_version=1.0&skill_id=default&version=1.0.0" {
			t.Fatalf("unexpected discovery request %s", request.URL.String())
		}
		return jsonResponse(http.StatusOK, testSnapshotJSON), nil
	}))
	snapshot, err := client.Discover(context.Background(), DiscoveryQuery{AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if uint64(snapshot.Revision) != 2 || len(snapshot.Instances) != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestRemoteErrorsAndTransportErrorsAreRedacted(t *testing.T) {
	secret := "bearer-super-secret"
	client := testClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusConflict, `{"code":"INSTANCE_GENERATION_FENCED","category":"conflict","message":"`+secret+`","retryable":false,"details":{"dsn":"`+secret+`"}}`), nil
	}))
	_, err := client.Deregister(context.Background(), DeregisterRequest{InstanceID: "runtime-a", SessionID: "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", LeaseID: "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Generation: 1})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "INSTANCE_GENERATION_FENCED" || strings.Contains(fmt.Sprintf("%v", err), secret) {
		t.Fatalf("unredacted or untyped error: %#v", err)
	}

	client = testClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(secret)
	}))
	_, err = client.Discover(context.Background(), DiscoveryQuery{AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error leaked: %v", err)
	}
}

func TestClientRejectsPathInjectionBeforeCredentialOrNetwork(t *testing.T) {
	var credentials, network atomic.Int32
	base, _ := url.Parse("https://control.example.invalid/control")
	client := Client{BaseURL: base, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		network.Add(1)
		return nil, errors.New("must not run")
	})}, Credential: CredentialSourceFunc(func(context.Context) (string, error) {
		credentials.Add(1)
		return "test-token", nil
	})}
	_, err := client.Discover(context.Background(), DiscoveryQuery{AgentID: "../other", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"})
	if err == nil || credentials.Load() != 0 || network.Load() != 0 {
		t.Fatalf("injection reached side effect: err=%v credential=%d network=%d", err, credentials.Load(), network.Load())
	}
}

func TestLeaseClosesAfterFencing(t *testing.T) {
	instance, err := registrywire.DecodeRuntimeInstance([]byte(testInstanceJSON))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := testClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(http.StatusConflict, `{"code":"INSTANCE_GENERATION_FENCED","category":"conflict","message":"fenced","retryable":false}`), nil
	}))
	lease, err := NewLease(client, Registration{Instance: instance, LeaseTTLSeconds: 30, KeepaliveIntervalSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Keepalive(context.Background(), CapacityState{Ready: true}); err == nil {
		t.Fatal("fencing was accepted")
	}
	if _, err := lease.Keepalive(context.Background(), CapacityState{Ready: true}); err == nil || calls.Load() != 1 {
		t.Fatalf("closed lease contacted server: err=%v calls=%d", err, calls.Load())
	}
}

func testClient(transport http.RoundTripper) Client {
	base, _ := url.Parse("https://control.example.invalid/control")
	return Client{BaseURL: base, HTTPClient: &http.Client{Transport: transport, Timeout: time.Second}, Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "test-token", nil })}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
