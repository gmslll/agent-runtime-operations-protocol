package registry

import (
	"context"
	"testing"
	"time"
)

const (
	testSession = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	testLease   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	testEvent   = "evt_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
)

func TestServiceUsesServerClockAndDefaultLeasePolicy(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: fixedIDs{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	request := validRegisterRequest()
	repository.register = func(command RegisterCommand) (Registration, error) {
		if command.Now != now || command.TTL != DefaultLeaseTTL || command.Keepalive != DefaultKeepaliveInterval || command.LeaseID != testLease || command.EventID != testEvent {
			t.Fatalf("register command = %+v", command)
		}
		return Registration{Instance: validInstance(now), LeaseTTLSeconds: 30, KeepaliveIntervalSeconds: 10}, nil
	}
	registered, err := service.Register(context.Background(), request)
	if err != nil || registered.Instance.RegistryRevision != 1 {
		t.Fatalf("register = %+v, %v", registered, err)
	}
}

func TestKeepaliveNeverTrustsReportedAtForServerTime(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: fixedIDs{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	reported := now.Add(-24 * time.Hour)
	repository.keepalive = func(command KeepaliveCommand) (KeepaliveResult, error) {
		if command.Now != now || command.Request.ReportedAt != reported {
			t.Fatalf("keepalive used caller time: %+v", command)
		}
		instance := validInstance(now)
		instance.HeartbeatSequence = 2
		instance.RegistryRevision = 2
		instance.ResourceVersion = 2
		instance.Runtime.Capacity.ActiveRuns = command.Request.ActiveRuns
		instance.Runtime.Capacity.AvailableSlots = command.Request.AvailableSlots
		instance.Runtime.Capacity.QueueDepth = command.Request.QueueDepth
		instance.Runtime.Ready = command.Request.Ready
		return KeepaliveResult{Instance: instance}, nil
	}
	_, err = service.Keepalive(context.Background(), KeepaliveRequest{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: testSession, LeaseID: testLease, Generation: 1, HeartbeatSequence: 2, ReportedAt: reported, Ready: true, AvailableSlots: 1})
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceAcceptsOnlyLiveExactKeepaliveReplay(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: fixedIDs{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	request := KeepaliveRequest{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: testSession, LeaseID: testLease, Generation: 1, HeartbeatSequence: 2, ReportedAt: now.Add(-time.Hour), Ready: true, AvailableSlots: 1}
	repository.keepalive = func(command KeepaliveCommand) (KeepaliveResult, error) {
		instance := validInstance(now)
		instance.HeartbeatSequence = command.Request.HeartbeatSequence
		instance.Runtime.Ready = command.Request.Ready
		instance.Runtime.Capacity.ActiveRuns = command.Request.ActiveRuns
		instance.Runtime.Capacity.AvailableSlots = command.Request.AvailableSlots
		instance.Runtime.Capacity.QueueDepth = command.Request.QueueDepth
		return KeepaliveResult{Instance: instance, Replay: true}, nil
	}
	if _, err := service.Keepalive(context.Background(), request); err != nil {
		t.Fatalf("live keepalive replay = %v", err)
	}
	repository.keepalive = func(command KeepaliveCommand) (KeepaliveResult, error) {
		instance := validInstance(now)
		instance.HeartbeatSequence = command.Request.HeartbeatSequence
		instance.Runtime.Ready = command.Request.Ready
		instance.Runtime.Capacity.ActiveRuns = command.Request.ActiveRuns
		instance.Runtime.Capacity.AvailableSlots = command.Request.AvailableSlots
		instance.Runtime.Capacity.QueueDepth = command.Request.QueueDepth
		instance.LeaseExpiresAt = now
		return KeepaliveResult{Instance: instance, Replay: true}, nil
	}
	if _, err := service.Keepalive(context.Background(), request); !HasReason(err, ReasonDependencyUnavailable) {
		t.Fatalf("expired keepalive replay = %v", err)
	}
}

func TestServiceAcceptsExactDeregisterReplay(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: fixedIDs{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	fence := Fence{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: testSession, LeaseID: testLease, Generation: 1}
	repository.deregister = func(command DeregisterCommand) (DeregisterResult, error) {
		instance := validInstance(now.Add(-time.Minute))
		instance.Status = StatusDeregistered
		instance.Draining = false
		instance.DrainDeadlineAt = nil
		instance.LeaseExpiresAt = now.Add(-time.Second)
		return DeregisterResult{Instance: instance, Replay: true}, nil
	}
	if _, err := service.Deregister(context.Background(), fence); err != nil {
		t.Fatalf("deregister replay = %v", err)
	}
}

func TestDiscoveryEligibilityIsExactConjunction(t *testing.T) {
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	query := DiscoveryQuery{TenantID: "tenant-a", AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"}
	base := validInstance(now)
	if !base.DiscoverableAt(now, query) {
		t.Fatal("eligible instance was excluded")
	}
	cases := map[string]func(*Instance){
		"lease":    func(value *Instance) { value.LeaseExpiresAt = now },
		"healthy":  func(value *Instance) { value.Runtime.Healthy = false },
		"ready":    func(value *Instance) { value.Runtime.Ready = false },
		"enabled":  func(value *Instance) { value.Operator.Enabled = false },
		"draining": func(value *Instance) { value.Draining = true },
		"capacity": func(value *Instance) { value.Runtime.Capacity.AvailableSlots = 0 },
		"protocol": func(value *Instance) { value.Runtime.ProtocolVersions = []string{"2.0"} },
		"binding":  func(value *Instance) { value.Bindings[0].AgentVersion = "2.0.0" },
		"status":   func(value *Instance) { value.Status = StatusExpired },
		"tenant":   func(value *Instance) { value.TenantID = "tenant-b" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Bindings = cloneBindings(base.Bindings)
			candidate.Runtime.ProtocolVersions = append([]string(nil), base.Runtime.ProtocolVersions...)
			mutate(&candidate)
			if candidate.DiscoverableAt(now, query) {
				t.Fatal("ineligible instance was discoverable")
			}
		})
	}
}

func TestDrainClassifiesClockFailureAsDependencyUnavailable(t *testing.T) {
	repository := &fakeRepository{}
	clock := &sequenceClock{values: []time.Time{time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC), {}}}
	service, err := New(Dependencies{Clock: clock, IDs: fixedIDs{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	request := DrainRequest{Fence: Fence{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: testSession, LeaseID: testLease, Generation: 1}, DeadlineAt: time.Date(2026, 9, 26, 8, 1, 0, 0, time.UTC)}
	if _, err := service.Drain(context.Background(), request); !HasReason(err, ReasonDependencyUnavailable) {
		t.Fatalf("Drain clock failure = %v", err)
	}
}

func TestValidationRejectsUnsafeIntegersAndNonCanonicalBindings(t *testing.T) {
	request := validRegisterRequest()
	request.Runtime.Capacity.MaxConcurrency = MaxSafeInteger + 1
	if request.Validate() == nil {
		t.Fatal("unsafe integer accepted")
	}
	request = validRegisterRequest()
	request.Bindings = append(request.Bindings, request.Bindings[0])
	if request.Validate() == nil {
		t.Fatal("duplicate binding accepted")
	}
}

func validRegisterRequest() RegisterRequest {
	return RegisterRequest{
		TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: testSession, ServiceID: "image-runtime", Environment: "production",
		Endpoint:             Endpoint{BaseURL: "https://runtime.internal.example", HealthPath: "/v1/health/ready"},
		Bindings:             []Binding{{AgentID: "image.generate", AgentVersion: "1.0.0", SkillIDs: []string{"default"}, ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Runtime:              RuntimeState{Healthy: true, Ready: true, Capacity: Capacity{MaxConcurrency: 2, MaxQueueDepth: 4, AvailableSlots: 2}, RuntimeVersion: "2026.09.26", ProtocolVersions: []string{"1.0"}, TransportProfiles: []string{"direct"}, Capabilities: Capabilities{Streaming: true, StreamResume: true, Cancellation: true, StatusQuery: true, EventOutbox: "durable"}, Labels: map[string]string{"region": "cn-east"}},
		IdempotencyKeyDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", IdempotencyRequestDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
}

func validInstance(now time.Time) Instance {
	request := validRegisterRequest()
	return Instance{
		TenantID: request.TenantID, InstanceID: request.InstanceID, SessionID: request.SessionID, ServiceID: request.ServiceID, Environment: request.Environment,
		Generation: 1, ResourceVersion: 1, RegistryRevision: 1, LeaseID: testLease, LeaseExpiresAt: now.Add(DefaultLeaseTTL), Endpoint: request.Endpoint,
		Bindings: cloneBindings(request.Bindings), Runtime: request.Runtime, Operator: OperatorState{Enabled: true, Weight: 100}, Status: StatusRegistered, CreatedAt: now, UpdatedAt: now,
	}
}

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type sequenceClock struct {
	values []time.Time
	index  int
}

func (clock *sequenceClock) Now() time.Time {
	if clock.index >= len(clock.values) {
		return time.Time{}
	}
	value := clock.values[clock.index]
	clock.index++
	return value
}

type fixedIDs struct{}

func (fixedIDs) NewLeaseID(context.Context) (string, error) { return testLease, nil }
func (fixedIDs) NewEventID(context.Context) (string, error) { return testEvent, nil }

type fakeRepository struct {
	register   func(RegisterCommand) (Registration, error)
	keepalive  func(KeepaliveCommand) (KeepaliveResult, error)
	deregister func(DeregisterCommand) (DeregisterResult, error)
}

func (repository *fakeRepository) Register(_ context.Context, command RegisterCommand) (Registration, error) {
	return repository.register(command)
}
func (repository *fakeRepository) Keepalive(_ context.Context, command KeepaliveCommand) (KeepaliveResult, error) {
	return repository.keepalive(command)
}
func (*fakeRepository) CompareAndSwap(context.Context, CASCommand) (Instance, error) {
	return Instance{}, NewError(ReasonDependencyUnavailable)
}
func (*fakeRepository) Drain(context.Context, DrainCommand) (Instance, error) {
	return Instance{}, NewError(ReasonDependencyUnavailable)
}
func (repository *fakeRepository) Deregister(_ context.Context, command DeregisterCommand) (DeregisterResult, error) {
	if repository.deregister == nil {
		return DeregisterResult{}, NewError(ReasonDependencyUnavailable)
	}
	return repository.deregister(command)
}
func (*fakeRepository) Expire(context.Context, ExpireCommand) ([]Instance, error) {
	return nil, NewError(ReasonDependencyUnavailable)
}
func (*fakeRepository) Snapshot(context.Context, DiscoveryQuery, time.Time) (Snapshot, error) {
	return Snapshot{}, NewError(ReasonDependencyUnavailable)
}
func (*fakeRepository) Events(context.Context, string, uint64, uint64) ([]Event, error) {
	return nil, NewError(ReasonDependencyUnavailable)
}
func (*fakeRepository) CompactionWatermark(context.Context, string) (uint64, error) {
	return 0, NewError(ReasonDependencyUnavailable)
}
