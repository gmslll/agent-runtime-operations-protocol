package internaltest

import (
	"context"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

const (
	Session1 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	Session2 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	Session3 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5e"
	Lease1   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	Lease2   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	Lease3   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5e"
)

type UnitOfWork interface {
	Within(context.Context, func(context.Context) error) error
}

func RunRepositoryMatrix(t *testing.T, unit UnitOfWork, repository registry.Repository) {
	t.Helper()
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	register1 := registry.RegisterCommand{Request: Request(Session1, "a", "1"), LeaseID: Lease1, EventID: EventID(1), Now: now, TTL: 30 * time.Second, Keepalive: 10 * time.Second}
	if _, err := repository.Register(context.Background(), register1); !registry.HasReason(err, registry.ReasonDependencyUnavailable) {
		t.Fatalf("write outside transaction = %v", err)
	}
	created := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register1) })
	if created.Instance.Generation != 1 || created.Instance.RegistryRevision != 1 || created.Replay {
		t.Fatalf("first registration = %+v", created)
	}
	replayed := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register1) })
	if !replayed.Replay || replayed.Instance.RegistryRevision != 1 {
		t.Fatalf("registration replay = %+v", replayed)
	}
	conflict := register1
	conflict.Request.IdempotencyRequestDigest = digest("2")
	WithinError(t, unit, registry.ReasonIdempotencyConflict, func(ctx context.Context) error {
		_, err := repository.Register(ctx, conflict)
		return err
	})
	reuse := register1
	reuse.Request.IdempotencyKeyDigest = digest("b")
	reuse.Request.IdempotencyRequestDigest = digest("3")
	WithinError(t, unit, registry.ReasonSessionReused, func(ctx context.Context) error {
		_, err := repository.Register(ctx, reuse)
		return err
	})

	heartbeat := registry.KeepaliveCommand{Request: registry.KeepaliveRequest{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: Session1, LeaseID: Lease1, Generation: 1, HeartbeatSequence: 1, ReportedAt: now.Add(-time.Hour), Healthy: true, Ready: true, AvailableSlots: 1}, EventID: EventID(2), Now: now.Add(time.Second), TTL: 30 * time.Second}
	alive := Within(t, unit, func(ctx context.Context) (registry.Instance, error) { return repository.Keepalive(ctx, heartbeat) })
	if alive.LeaseExpiresAt != heartbeat.Now.Add(heartbeat.TTL) || alive.LeaseExpiresAt == heartbeat.Request.ReportedAt.Add(heartbeat.TTL) || alive.RegistryRevision != 2 {
		t.Fatalf("server-time lease = %+v", alive)
	}
	WithinError(t, unit, registry.ReasonHeartbeatStale, func(ctx context.Context) error {
		_, err := repository.Keepalive(ctx, heartbeat)
		return err
	})
	staleFence := heartbeat
	staleFence.Request.SessionID = Session2
	staleFence.Request.HeartbeatSequence = 2
	WithinError(t, unit, registry.ReasonGenerationFenced, func(ctx context.Context) error {
		_, err := repository.Keepalive(ctx, staleFence)
		return err
	})

	cas := registry.CASCommand{Request: registry.CASRequest{TenantID: "tenant-a", InstanceID: "runtime-a", ExpectedResourceVersion: alive.ResourceVersion, Enabled: true, Weight: 75, Priority: 2}, EventID: EventID(3), Now: now.Add(2 * time.Second)}
	updated := Within(t, unit, func(ctx context.Context) (registry.Instance, error) { return repository.CompareAndSwap(ctx, cas) })
	if updated.Operator.Weight != 75 || updated.RegistryRevision != 3 {
		t.Fatalf("cas update = %+v", updated)
	}
	WithinError(t, unit, registry.ReasonResourceConflict, func(ctx context.Context) error {
		_, err := repository.CompareAndSwap(ctx, cas)
		return err
	})
	query := registry.DiscoveryQuery{TenantID: "tenant-a", AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"}
	snapshot, err := repository.Snapshot(context.Background(), query, now.Add(2*time.Second))
	if err != nil || snapshot.Revision != 3 || len(snapshot.Instances) != 1 {
		t.Fatalf("eligible snapshot = %+v, %v", snapshot, err)
	}

	drain := registry.DrainCommand{Request: registry.DrainRequest{Fence: registry.Fence{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: Session1, LeaseID: Lease1, Generation: 1}, DeadlineAt: now.Add(time.Minute)}, EventID: EventID(4), Now: now.Add(3 * time.Second)}
	drained := Within(t, unit, func(ctx context.Context) (registry.Instance, error) { return repository.Drain(ctx, drain) })
	if !drained.Draining || drained.RegistryRevision != 4 {
		t.Fatalf("drain = %+v", drained)
	}
	snapshot, err = repository.Snapshot(context.Background(), query, now.Add(3*time.Second))
	if err != nil || len(snapshot.Instances) != 0 {
		t.Fatalf("draining snapshot = %+v, %v", snapshot, err)
	}

	register2 := registry.RegisterCommand{Request: Request(Session2, "c", "4"), LeaseID: Lease2, EventID: EventID(5), Now: now.Add(4 * time.Second), TTL: 30 * time.Second, Keepalive: 10 * time.Second}
	second := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register2) })
	if second.Instance.Generation != 2 || !second.Instance.Draining || second.Instance.RegistryRevision != 5 {
		t.Fatalf("drain was not latched = %+v", second)
	}
	old := heartbeat
	old.Request.HeartbeatSequence = 2
	old.EventID = EventID(9)
	old.Now = now.Add(5 * time.Second)
	WithinError(t, unit, registry.ReasonGenerationFenced, func(ctx context.Context) error {
		_, err := repository.Keepalive(ctx, old)
		return err
	})

	deregister := registry.DeregisterCommand{Fence: registry.Fence{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: Session2, LeaseID: Lease2, Generation: 2}, EventID: EventID(6), Now: now.Add(5 * time.Second)}
	removed := Within(t, unit, func(ctx context.Context) (registry.Instance, error) { return repository.Deregister(ctx, deregister) })
	if removed.Status != registry.StatusDeregistered || removed.Draining || removed.RegistryRevision != 6 {
		t.Fatalf("deregister = %+v", removed)
	}
	register3 := registry.RegisterCommand{Request: Request(Session3, "d", "5"), LeaseID: Lease3, EventID: EventID(7), Now: now.Add(6 * time.Second), TTL: 30 * time.Second, Keepalive: 10 * time.Second}
	third := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register3) })
	if third.Instance.Generation != 3 || third.Instance.Draining || third.Instance.RegistryRevision != 7 {
		t.Fatalf("post-deregister registration = %+v", third)
	}

	expired := Within(t, unit, func(ctx context.Context) ([]registry.Instance, error) {
		return repository.Expire(ctx, registry.ExpireCommand{TenantID: "tenant-a", EventIDs: []string{EventID(8)}, Now: third.Instance.LeaseExpiresAt, Limit: 1})
	})
	if len(expired) != 1 || expired[0].Status != registry.StatusExpired || expired[0].RegistryRevision != 8 {
		t.Fatalf("expiry = %+v", expired)
	}
	events, err := repository.Events(context.Background(), "tenant-a", 0, 100)
	if err != nil || len(events) != 8 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	for index, event := range events {
		if event.Revision != uint64(index+1) {
			t.Fatalf("event revisions are not contiguous: %+v", events)
		}
	}
	watermark, err := repository.CompactionWatermark(context.Background(), "tenant-a")
	if err != nil || watermark != 0 {
		t.Fatalf("watermark = %d, %v", watermark, err)
	}
}

func Request(session, key, request string) registry.RegisterRequest {
	return registry.RegisterRequest{
		TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: session, ServiceID: "image-runtime", Environment: "production",
		Endpoint:             registry.Endpoint{BaseURL: "https://runtime.internal.example", HealthPath: "/v1/health/ready"},
		Bindings:             []registry.Binding{{AgentID: "image.generate", AgentVersion: "1.0.0", SkillIDs: []string{"default"}, ManifestDigest: "sha256:" + digest("a")}},
		Runtime:              registry.RuntimeState{Healthy: true, Ready: true, Capacity: registry.Capacity{MaxConcurrency: 2, MaxQueueDepth: 4, AvailableSlots: 2}, RuntimeVersion: "2026.09.26", ProtocolVersions: []string{"1.0"}, TransportProfiles: []string{"direct"}, Capabilities: registry.Capabilities{Streaming: true, StreamResume: true, Cancellation: true, StatusQuery: true, EventOutbox: "durable"}, Labels: map[string]string{"region": "cn-east"}},
		IdempotencyKeyDigest: digest(key), IdempotencyRequestDigest: digest(request),
	}
}

func EventID(number int) string {
	return "evt_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5" + string(rune('0'+number))
}

func digest(character string) string {
	result := ""
	for len(result) < 64 {
		result += character
	}
	return result[:64]
}

func Within[T any](t *testing.T, unit UnitOfWork, callback func(context.Context) (T, error)) T {
	t.Helper()
	var result T
	err := unit.Within(context.Background(), func(ctx context.Context) error {
		var callErr error
		result, callErr = callback(ctx)
		return callErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func WithinError(t *testing.T, unit UnitOfWork, reason registry.ErrorReason, callback func(context.Context) error) {
	t.Helper()
	err := unit.Within(context.Background(), callback)
	if !registry.HasReason(err, reason) {
		t.Fatalf("error=%v want=%s", err, reason)
	}
}
