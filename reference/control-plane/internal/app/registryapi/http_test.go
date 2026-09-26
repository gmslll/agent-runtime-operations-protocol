package registryapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

var testNow = time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)

type testClock struct{}

func (testClock) Now() time.Time { return testNow }

type testUoW struct{ calls int }

func (uow *testUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	uow.calls++
	return callback(ctx)
}

type testAuthorizer struct{}

func (testAuthorizer) Authorize(_ context.Context, request AuthorizationRequest) error {
	want := map[Operation]string{OperationRegister: "registry:register", OperationKeepalive: "registry:write", OperationOperate: "registry:operate", OperationDrain: "registry:write", OperationDeregister: "registry:write", OperationDiscover: "registry:discover"}[request.Operation]
	if want == "" || !request.Caller.HasScope(want) {
		return ErrForbidden
	}
	return nil
}

type testCore struct {
	instance    registry.Instance
	registerErr error
	snapshotErr error
}

func (core *testCore) Register(_ context.Context, request registry.RegisterRequest) (registry.Registration, error) {
	if core.registerErr != nil {
		return registry.Registration{}, core.registerErr
	}
	core.instance.TenantID, core.instance.InstanceID, core.instance.SessionID = request.TenantID, request.InstanceID, request.SessionID
	return registry.Registration{Instance: core.instance, LeaseTTLSeconds: 30, KeepaliveIntervalSeconds: 10}, nil
}
func (core *testCore) Keepalive(_ context.Context, request registry.KeepaliveRequest) (registry.Instance, error) {
	core.instance.HeartbeatSequence = request.HeartbeatSequence
	core.instance.Runtime.Ready, core.instance.Runtime.Capacity.ActiveRuns, core.instance.Runtime.Capacity.AvailableSlots, core.instance.Runtime.Capacity.QueueDepth = request.Ready, request.ActiveRuns, request.AvailableSlots, request.QueueDepth
	core.instance.LeaseExpiresAt = testNow.Add(30 * time.Second)
	return core.instance, nil
}
func (core *testCore) CompareAndSwap(_ context.Context, request registry.CASRequest) (registry.Instance, error) {
	core.instance.Operator = registry.OperatorState{Enabled: request.Enabled, Weight: request.Weight, Priority: request.Priority, MaintenanceReason: request.MaintenanceReason}
	core.instance.ResourceVersion++
	return core.instance, nil
}
func (core *testCore) Drain(_ context.Context, request registry.DrainRequest) (registry.Instance, error) {
	core.instance.Draining = true
	core.instance.DrainDeadlineAt = &request.DeadlineAt
	return core.instance, nil
}
func (core *testCore) Deregister(_ context.Context, _ registry.Fence) (registry.Instance, error) {
	core.instance.Status = registry.StatusDeregistered
	return core.instance, nil
}
func (core *testCore) Snapshot(_ context.Context, _ registry.DiscoveryQuery) (registry.Snapshot, error) {
	if core.snapshotErr != nil {
		return registry.Snapshot{}, core.snapshotErr
	}
	return registry.Snapshot{Revision: 2, Instances: []registry.Instance{core.instance}}, nil
}

func TestHTTPRegistrationDiscoveryAndWatchBoundary(t *testing.T) {
	core := &testCore{instance: validInstance()}
	uow := &testUoW{}
	service, err := New(Dependencies{Core: core, UoW: uow, Clock: testClock{}, Authorizer: testAuthorizer{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	caller := Caller{TenantID: "reference-dev", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:register", "registry:write", "registry:operate", "registry:discover"}}
	handler, err := NewHTTPHandler(service, func(*http.Request) (Caller, platform.RequestMetadata, bool) {
		return caller, platform.RequestMetadata{}, true
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"schema_version":1,"session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","service_id":"image-runtime","environment":"production","endpoint":{"base_url":"https://runtime.internal.example","health_path":"/v1/health/ready"},"bindings":[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"runtime":{"healthy":true,"ready":true,"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":1,"available_slots":1,"queue_depth":0},"runtime_version":"2026.09.26","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"labels":{"region":"cn-east"}}}`
	request := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "register-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || uow.calls != 1 {
		t.Fatalf("register status=%d uow=%d body=%s", response.Code, uow.calls, response.Body.String())
	}
	var registered map[string]any
	if json.Unmarshal(response.Body.Bytes(), &registered) != nil || registered["schema_version"] != float64(1) {
		t.Fatalf("invalid register response %s", response.Body.String())
	}

	discover := httptest.NewRequest(http.MethodGet, "/v1/discovery/agents/image.generate/instances?version=1.0.0&skill_id=default&protocol_version=1.0", nil)
	discoveryResponse := httptest.NewRecorder()
	handler.ServeHTTP(discoveryResponse, discover)
	if discoveryResponse.Code != http.StatusOK || !strings.Contains(discoveryResponse.Body.String(), `"revision":2`) {
		t.Fatalf("discover status=%d body=%s", discoveryResponse.Code, discoveryResponse.Body.String())
	}

	watch := httptest.NewRequest(http.MethodGet, "/v1/discovery/changes?after_revision=0&wait_seconds=1", nil)
	watchResponse := httptest.NewRecorder()
	handler.ServeHTTP(watchResponse, watch)
	if watchResponse.Code != http.StatusNotFound {
		t.Fatalf("P15 registered P16 Watch route: %d", watchResponse.Code)
	}
}

func TestHTTPRejectsUnknownFieldsAndMissingScope(t *testing.T) {
	core := &testCore{instance: validInstance()}
	service, _ := New(Dependencies{Core: core, UoW: &testUoW{}, Clock: testClock{}, Authorizer: testAuthorizer{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	caller := Caller{TenantID: "reference-dev", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:discover"}}
	handler, _ := NewHTTPHandler(service, func(*http.Request) (Caller, platform.RequestMetadata, bool) {
		return caller, platform.RequestMetadata{}, true
	})
	request := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(`{"schema_version":1,"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "register-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d", response.Code)
	}

	valid := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(validRegisterBody()))
	valid.Header.Set("Content-Type", "application/json")
	valid.Header.Set("Idempotency-Key", "register-0001")
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, valid)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("missing scope status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
}

func TestHTTPRejectsOversizeTrailingAndDuplicateHeaders(t *testing.T) {
	core := &testCore{instance: validInstance()}
	service, _ := New(Dependencies{Core: core, UoW: &testUoW{}, Clock: testClock{}, Authorizer: testAuthorizer{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	caller := Caller{TenantID: "reference-dev", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:register"}}
	handler, _ := NewHTTPHandler(service, func(*http.Request) (Caller, platform.RequestMetadata, bool) {
		return caller, platform.RequestMetadata{}, true
	})

	for name, body := range map[string]string{
		"oversize": strings.Repeat(" ", maxJSONBody+1),
		"trailing": validRegisterBody() + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "register-0001")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	request := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(validRegisterBody()))
	request.Header["Content-Type"] = []string{"application/json", "application/json"}
	request.Header.Set("Idempotency-Key", "register-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate content type status=%d", response.Code)
	}
}

func TestHTTPFencingAndDependencyErrorsAreTypedAndRedacted(t *testing.T) {
	secret := "postgres://user:password@example.invalid/db"
	core := &testCore{instance: validInstance(), registerErr: fmt.Errorf("%s: %w", secret, registry.NewError(registry.ReasonGenerationFenced))}
	service, _ := New(Dependencies{Core: core, UoW: &testUoW{}, Clock: testClock{}, Authorizer: testAuthorizer{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	caller := Caller{TenantID: "reference-dev", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:register", "registry:discover"}}
	handler, _ := NewHTTPHandler(service, func(*http.Request) (Caller, platform.RequestMetadata, bool) {
		return caller, platform.RequestMetadata{}, true
	})
	request := httptest.NewRequest(http.MethodPut, "/v1/registry/instances/runtime-a", strings.NewReader(validRegisterBody()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "register-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"INSTANCE_GENERATION_FENCED"`) || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("fencing response status=%d body=%s", response.Code, response.Body.String())
	}

	core.snapshotErr = fmt.Errorf("%s: database unavailable", secret)
	discover := httptest.NewRequest(http.MethodGet, "/v1/discovery/agents/image.generate/instances?version=1.0.0&skill_id=default&protocol_version=1.0", nil)
	dependency := httptest.NewRecorder()
	handler.ServeHTTP(dependency, discover)
	if dependency.Code != http.StatusServiceUnavailable || dependency.Header().Get("Retry-After") != "1" || !strings.Contains(dependency.Body.String(), `"retry_after_seconds":1`) || strings.Contains(dependency.Body.String(), secret) {
		t.Fatalf("dependency response status=%d header=%v body=%s", dependency.Code, dependency.Header(), dependency.Body.String())
	}
}

func validRegisterBody() string {
	return `{"schema_version":1,"session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","service_id":"image-runtime","environment":"production","endpoint":{"base_url":"https://runtime.internal.example","health_path":"/v1/health/ready"},"bindings":[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"runtime":{"healthy":true,"ready":true,"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":1,"available_slots":1,"queue_depth":0},"runtime_version":"2026.09.26","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"labels":{"region":"cn-east"}}}`
}

func validInstance() registry.Instance {
	return registry.Instance{TenantID: "reference-dev", InstanceID: "runtime-a", SessionID: "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", ServiceID: "image-runtime", Environment: "production", Generation: 1, ResourceVersion: 1, RegistryRevision: 2, LeaseID: "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", LeaseExpiresAt: testNow.Add(30 * time.Second), Endpoint: registry.Endpoint{BaseURL: "https://runtime.internal.example", HealthPath: "/v1/health/ready"}, Bindings: []registry.Binding{{AgentID: "image.generate", AgentVersion: "1.0.0", SkillIDs: []string{"default"}, ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, Runtime: registry.RuntimeState{Healthy: true, Ready: true, Capacity: registry.Capacity{MaxConcurrency: 2, MaxQueueDepth: 4, ActiveRuns: 1, AvailableSlots: 1}, RuntimeVersion: "2026.09.26", ProtocolVersions: []string{"1.0"}, TransportProfiles: []string{"direct"}, Capabilities: registry.Capabilities{Streaming: true, StreamResume: true, Cancellation: true, StatusQuery: true, EventOutbox: "durable"}, Labels: map[string]string{"region": "cn-east"}}, Operator: registry.OperatorState{Enabled: true, Weight: 100, Priority: 10}, Status: registry.StatusRegistered, CreatedAt: testNow, UpdatedAt: testNow}
}
