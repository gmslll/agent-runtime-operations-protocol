package internaltest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

const (
	Session1 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	Session2 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	Session3 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5e"
	Session4 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5f"
	Session5 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b60"
	Session6 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b61"
	Session7 = "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b62"
	Lease1   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	Lease2   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	Lease3   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5e"
	Lease4   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5f"
	Lease5   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b60"
	Lease6   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b61"
	Lease7   = "lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b62"
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
	t.Log("registry matrix: registered initial instance")
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

	heartbeat := registry.KeepaliveCommand{Request: registry.KeepaliveRequest{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: Session1, LeaseID: Lease1, Generation: 1, HeartbeatSequence: 1, ReportedAt: now.Add(-time.Hour), Ready: true, AvailableSlots: 1}, EventID: EventID(2), Now: now.Add(time.Second), TTL: 30 * time.Second}
	aliveResult := Within(t, unit, func(ctx context.Context) (registry.KeepaliveResult, error) {
		return repository.Keepalive(ctx, heartbeat)
	})
	alive := aliveResult.Instance
	t.Log("registry matrix: keepalive committed")
	if aliveResult.Replay || alive.LeaseExpiresAt != heartbeat.Now.Add(heartbeat.TTL) || alive.LeaseExpiresAt == heartbeat.Request.ReportedAt.Add(heartbeat.TTL) || alive.RegistryRevision != 2 {
		t.Fatalf("server-time lease = %+v", alive)
	}
	replayedHeartbeat := Within(t, unit, func(ctx context.Context) (registry.KeepaliveResult, error) {
		return repository.Keepalive(ctx, heartbeat)
	})
	if !replayedHeartbeat.Replay || replayedHeartbeat.Instance.RegistryRevision != alive.RegistryRevision || !replayedHeartbeat.Instance.LeaseExpiresAt.Equal(alive.LeaseExpiresAt) {
		t.Fatalf("keepalive replay = %+v", replayedHeartbeat)
	}
	changedHeartbeat := heartbeat
	changedHeartbeat.Request.AvailableSlots = 0
	WithinError(t, unit, registry.ReasonHeartbeatStale, func(ctx context.Context) error {
		_, err := repository.Keepalive(ctx, changedHeartbeat)
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
	t.Log("registry matrix: compare-and-swap committed")
	if updated.Operator.Weight != 75 || updated.RegistryRevision != 3 {
		t.Fatalf("cas update = %+v", updated)
	}
	WithinError(t, unit, registry.ReasonResourceConflict, func(ctx context.Context) error {
		_, err := repository.CompareAndSwap(ctx, cas)
		return err
	})
	query := registry.DiscoveryQuery{TenantID: "tenant-a", AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", ProtocolVersion: "1.0"}
	snapshot, err := repository.Snapshot(context.Background(), query, now.Add(2*time.Second))
	t.Log("registry matrix: initial snapshot read")
	if err != nil || snapshot.Revision != 3 || len(snapshot.Instances) != 1 {
		t.Fatalf("eligible snapshot = %+v, %v", snapshot, err)
	}

	drain := registry.DrainCommand{Request: registry.DrainRequest{Fence: registry.Fence{TenantID: "tenant-a", InstanceID: "runtime-a", SessionID: Session1, LeaseID: Lease1, Generation: 1}, DeadlineAt: now.Add(time.Minute)}, EventID: EventID(4), Now: now.Add(3 * time.Second)}
	drained := Within(t, unit, func(ctx context.Context) (registry.Instance, error) { return repository.Drain(ctx, drain) })
	t.Log("registry matrix: drain committed")
	if !drained.Draining || drained.RegistryRevision != 4 {
		t.Fatalf("drain = %+v", drained)
	}
	snapshot, err = repository.Snapshot(context.Background(), query, now.Add(3*time.Second))
	if err != nil || len(snapshot.Instances) != 0 {
		t.Fatalf("draining snapshot = %+v, %v", snapshot, err)
	}

	register2 := registry.RegisterCommand{Request: Request(Session2, "c", "4"), LeaseID: Lease2, EventID: EventID(5), Now: now.Add(4 * time.Second), TTL: 30 * time.Second, Keepalive: 10 * time.Second}
	second := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register2) })
	t.Log("registry matrix: drained instance re-registered")
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
	removedResult := Within(t, unit, func(ctx context.Context) (registry.DeregisterResult, error) {
		return repository.Deregister(ctx, deregister)
	})
	removed := removedResult.Instance
	if removedResult.Replay || removed.Status != registry.StatusDeregistered || removed.Draining || removed.RegistryRevision != 6 {
		t.Fatalf("deregister = %+v", removed)
	}
	replayedDeregister := Within(t, unit, func(ctx context.Context) (registry.DeregisterResult, error) {
		return repository.Deregister(ctx, deregister)
	})
	if !replayedDeregister.Replay || replayedDeregister.Instance.RegistryRevision != removed.RegistryRevision {
		t.Fatalf("deregister replay = %+v", replayedDeregister)
	}
	register3 := registry.RegisterCommand{Request: Request(Session3, "d", "5"), LeaseID: Lease3, EventID: EventID(7), Now: now.Add(6 * time.Second), TTL: 30 * time.Second, Keepalive: 10 * time.Second}
	third := Within(t, unit, func(ctx context.Context) (registry.Registration, error) { return repository.Register(ctx, register3) })
	t.Log("registry matrix: deregistered instance re-registered")
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
	windowRepository, ok := repository.(interface {
		EventWindow(context.Context, string, uint64, uint64) (registry.EventWindow, error)
	})
	if !ok {
		t.Fatal("durable repository does not expose the P16 atomic event window")
	}
	window, err := windowRepository.EventWindow(context.Background(), "tenant-a", 4, 100)
	if err != nil || window.Revision != 8 || window.CompactionWatermark != 0 || len(window.Events) != 4 || window.Events[0].Revision != 5 || window.Events[3].Revision != 8 {
		t.Fatalf("event window = %+v, %v", window, err)
	}
	watermark, err := repository.CompactionWatermark(context.Background(), "tenant-a")
	if err != nil || watermark != 0 {
		t.Fatalf("watermark = %d, %v", watermark, err)
	}

	// Expiry rotates lease liveness but must not clear an operator drain latch.
	drainRequest := RequestFor("runtime-drain-expire", Session4, "6", "6")
	drainCreated := Within(t, unit, func(ctx context.Context) (registry.Registration, error) {
		return repository.Register(ctx, registry.RegisterCommand{Request: drainRequest, LeaseID: Lease4, EventID: EventID(9), Now: now.Add(10 * time.Second), TTL: 2 * time.Second, Keepalive: time.Second})
	})
	t.Log("registry matrix: expiry-drain instance registered")
	drainFence := registry.Fence{TenantID: "tenant-a", InstanceID: drainRequest.InstanceID, SessionID: Session4, LeaseID: Lease4, Generation: drainCreated.Instance.Generation}
	Within(t, unit, func(ctx context.Context) (registry.Instance, error) {
		return repository.Drain(ctx, registry.DrainCommand{Request: registry.DrainRequest{Fence: drainFence, DeadlineAt: now.Add(time.Minute)}, EventID: EventID(10), Now: now.Add(11 * time.Second)})
	})
	Within(t, unit, func(ctx context.Context) ([]registry.Instance, error) {
		return repository.Expire(ctx, registry.ExpireCommand{TenantID: "tenant-a", EventIDs: []string{EventID(11)}, Now: now.Add(12 * time.Second), Limit: 1})
	})
	reregister := RequestFor("runtime-drain-expire", Session5, "7", "7")
	drainRotated := Within(t, unit, func(ctx context.Context) (registry.Registration, error) {
		return repository.Register(ctx, registry.RegisterCommand{Request: reregister, LeaseID: Lease5, EventID: EventID(12), Now: now.Add(13 * time.Second), TTL: 30 * time.Second, Keepalive: 10 * time.Second})
	})
	t.Log("registry matrix: expired drained instance re-registered")
	if !drainRotated.Instance.Draining || drainRotated.Instance.DrainDeadlineAt == nil {
		t.Fatalf("expiry cleared drain latch: %+v", drainRotated)
	}

	// Canonical fixed-width timestamps must keep SQL range ordering identical
	// to time.Time ordering across fractional-second values.
	dueRequest := RequestFor("runtime-fraction-due", Session6, "8", "8")
	due := Within(t, unit, func(ctx context.Context) (registry.Registration, error) {
		return repository.Register(ctx, registry.RegisterCommand{Request: dueRequest, LeaseID: Lease6, EventID: EventID(13), Now: now.Add(20*time.Second + 90*time.Millisecond), TTL: 10 * time.Second, Keepalive: time.Second})
	})
	liveRequest := RequestFor("runtime-fraction-live", Session7, "9", "9")
	live := Within(t, unit, func(ctx context.Context) (registry.Registration, error) {
		return repository.Register(ctx, registry.RegisterCommand{Request: liveRequest, LeaseID: Lease7, EventID: EventID(14), Now: now.Add(20*time.Second + 120*time.Millisecond), TTL: 10 * time.Second, Keepalive: time.Second})
	})
	t.Log("registry matrix: fractional leases registered")
	cutoff := now.Add(30*time.Second + 100*time.Millisecond)
	fractionExpired := Within(t, unit, func(ctx context.Context) ([]registry.Instance, error) {
		return repository.Expire(ctx, registry.ExpireCommand{TenantID: "tenant-a", EventIDs: []string{EventID(15), EventID(16)}, Now: cutoff, Limit: 2})
	})
	if len(fractionExpired) != 1 || fractionExpired[0].InstanceID != due.Instance.InstanceID || !cutoff.Before(live.Instance.LeaseExpiresAt) {
		t.Fatalf("fractional lease ordering = expired %+v due %+v live %+v", fractionExpired, due, live)
	}
	fractionSnapshot, err := repository.Snapshot(context.Background(), query, cutoff)
	if err != nil || fractionSnapshot.CompactionWatermark > fractionSnapshot.Revision {
		t.Fatalf("fractional snapshot = %+v, %v", fractionSnapshot, err)
	}
	foundLive := false
	for _, instance := range fractionSnapshot.Instances {
		if instance.RegistryRevision > fractionSnapshot.Revision {
			t.Fatalf("snapshot revision %d precedes instance revision %d", fractionSnapshot.Revision, instance.RegistryRevision)
		}
		foundLive = foundLive || instance.InstanceID == live.Instance.InstanceID
	}
	if !foundLive {
		t.Fatalf("live fractional lease omitted: %+v", fractionSnapshot)
	}

	// Every snapshot must describe one committed database position even while
	// registrations advance the global revision on another connection.
	writerResult := make(chan error, 1)
	go func() {
		for index := 0; index < 32; index++ {
			number := 100 + index
			request := RequestFor(fmt.Sprintf("runtime-concurrent-%02d", index), SessionID(number), fmt.Sprintf("%064x", number), fmt.Sprintf("%064x", 1000+number))
			err := unit.Within(context.Background(), func(ctx context.Context) error {
				_, registerErr := repository.Register(ctx, registry.RegisterCommand{Request: request, LeaseID: LeaseID(number), EventID: EventID(number), Now: now.Add(time.Duration(40+index) * time.Second), TTL: 5 * time.Minute, Keepalive: time.Minute})
				return registerErr
			})
			if err != nil {
				writerResult <- err
				return
			}
		}
		writerResult <- nil
	}()
	for index := 0; index < 64; index++ {
		consistent, snapshotErr := repository.Snapshot(context.Background(), query, now.Add(100*time.Second))
		if snapshotErr != nil {
			t.Fatalf("concurrent snapshot %d: %v", index, snapshotErr)
		}
		if consistent.CompactionWatermark > consistent.Revision {
			t.Fatalf("concurrent snapshot watermark=%d revision=%d", consistent.CompactionWatermark, consistent.Revision)
		}
		for _, instance := range consistent.Instances {
			if instance.RegistryRevision > consistent.Revision {
				t.Fatalf("torn snapshot revision=%d contains instance revision=%d", consistent.Revision, instance.RegistryRevision)
			}
		}
	}
	if err := <-writerResult; err != nil {
		t.Fatalf("concurrent registry writer: %v", err)
	}
}

func Request(session, key, request string) registry.RegisterRequest {
	return RequestFor("runtime-a", session, key, request)
}

func RequestFor(instanceID, session, key, request string) registry.RegisterRequest {
	return registry.RegisterRequest{
		TenantID: "tenant-a", InstanceID: instanceID, SessionID: session, ServiceID: "image-runtime", Environment: "production",
		Endpoint:             registry.Endpoint{BaseURL: "https://runtime.internal.example", HealthPath: "/v1/health/ready"},
		Bindings:             []registry.Binding{{AgentID: "image.generate", AgentVersion: "1.0.0", SkillIDs: []string{"default"}, ManifestDigest: "sha256:" + digest("a")}},
		Runtime:              registry.RuntimeState{Healthy: true, Ready: true, Capacity: registry.Capacity{MaxConcurrency: 2, MaxQueueDepth: 4, AvailableSlots: 2}, RuntimeVersion: "2026.09.26", ProtocolVersions: []string{"1.0"}, TransportProfiles: []string{"direct"}, Capabilities: registry.Capabilities{Streaming: true, StreamResume: true, Cancellation: true, StatusQuery: true, EventOutbox: "durable"}, Labels: map[string]string{"region": "cn-east"}},
		IdempotencyKeyDigest: digest(key), IdempotencyRequestDigest: digest(request),
	}
}

func EventID(number int) string {
	return fmt.Sprintf("evt_018f6b6e-8a2e-7c3a-8b2a-%012x", number)
}

func SessionID(number int) string {
	return fmt.Sprintf("ses_018f6b6e-8a2e-7c3a-8b2a-%012x", number)
}

func LeaseID(number int) string {
	return fmt.Sprintf("lease_018f6b6e-8a2e-7c3a-8b2a-%012x", number)
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
