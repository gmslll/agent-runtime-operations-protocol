package registry

import (
	"context"
	"reflect"
	"time"
)

type Service struct{ dependencies Dependencies }

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.LeaseTTL == 0 {
		dependencies.LeaseTTL = DefaultLeaseTTL
	}
	if dependencies.KeepaliveInterval == 0 {
		dependencies.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if err := dependencies.Validate(); err != nil {
		return nil, err
	}
	now := dependencies.Clock.Now()
	if !utc(now) {
		return nil, NewError(ReasonDependencyUnavailable)
	}
	return &Service{dependencies: dependencies}, nil
}

func (service *Service) Register(ctx context.Context, request RegisterRequest) (Registration, error) {
	if err := request.Validate(); err != nil {
		return Registration{}, err
	}
	now, err := service.now()
	if err != nil {
		return Registration{}, err
	}
	leaseID, err := service.dependencies.IDs.NewLeaseID(ctx)
	if err != nil || !validPrefixedUUID(leaseID, "lease_") {
		return Registration{}, NewError(ReasonDependencyUnavailable)
	}
	eventID, err := service.eventID(ctx)
	if err != nil {
		return Registration{}, err
	}
	result, err := service.dependencies.Repository.Register(ctx, RegisterCommand{Request: request, LeaseID: leaseID, EventID: eventID, Now: now, TTL: service.dependencies.LeaseTTL, Keepalive: service.dependencies.KeepaliveInterval})
	if err != nil {
		return Registration{}, normalize(err)
	}
	if err := result.Validate(); err != nil {
		return Registration{}, NewError(ReasonDependencyUnavailable)
	}
	instance := result.Instance
	if instance.TenantID != request.TenantID || instance.InstanceID != request.InstanceID || instance.SessionID != request.SessionID || instance.ServiceID != request.ServiceID || instance.Environment != request.Environment || !reflect.DeepEqual(instance.Endpoint, request.Endpoint) || !reflect.DeepEqual(instance.Bindings, request.Bindings) || !reflect.DeepEqual(instance.Runtime, request.Runtime) || instance.Status != StatusRegistered || result.LeaseTTLSeconds != uint64(service.dependencies.LeaseTTL/time.Second) || result.KeepaliveIntervalSeconds != uint64(service.dependencies.KeepaliveInterval/time.Second) {
		return Registration{}, NewError(ReasonDependencyUnavailable)
	}
	if !result.Replay && (instance.LeaseID != leaseID || !instance.LeaseExpiresAt.Equal(now.Add(service.dependencies.LeaseTTL))) {
		return Registration{}, NewError(ReasonDependencyUnavailable)
	}
	return result, nil
}

func (service *Service) Keepalive(ctx context.Context, request KeepaliveRequest) (Instance, error) {
	if err := request.Validate(); err != nil {
		return Instance{}, err
	}
	now, err := service.now()
	if err != nil {
		return Instance{}, err
	}
	eventID, err := service.eventID(ctx)
	if err != nil {
		return Instance{}, err
	}
	instance, err := service.dependencies.Repository.Keepalive(ctx, KeepaliveCommand{Request: request, EventID: eventID, Now: now, TTL: service.dependencies.LeaseTTL})
	instance, err = service.checked(instance, err)
	if err != nil {
		return Instance{}, err
	}
	if !matchesFence(instance, request.Fence()) || instance.HeartbeatSequence != request.HeartbeatSequence || instance.Runtime.Healthy != request.Healthy || instance.Runtime.Ready != request.Ready || instance.Runtime.Capacity.ActiveRuns != request.ActiveRuns || instance.Runtime.Capacity.AvailableSlots != request.AvailableSlots || instance.Runtime.Capacity.QueueDepth != request.QueueDepth || !instance.LeaseExpiresAt.Equal(now.Add(service.dependencies.LeaseTTL)) || instance.Status != StatusRegistered {
		return Instance{}, NewError(ReasonDependencyUnavailable)
	}
	return instance, nil
}

func (service *Service) CompareAndSwap(ctx context.Context, request CASRequest) (Instance, error) {
	if err := request.Validate(); err != nil {
		return Instance{}, err
	}
	now, err := service.now()
	if err != nil {
		return Instance{}, err
	}
	eventID, err := service.eventID(ctx)
	if err != nil {
		return Instance{}, err
	}
	instance, err := service.dependencies.Repository.CompareAndSwap(ctx, CASCommand{Request: request, EventID: eventID, Now: now})
	instance, err = service.checked(instance, err)
	if err != nil {
		return Instance{}, err
	}
	want := OperatorState{Enabled: request.Enabled, Weight: request.Weight, Priority: request.Priority, MaintenanceReason: request.MaintenanceReason}
	if instance.TenantID != request.TenantID || instance.InstanceID != request.InstanceID || instance.ResourceVersion != request.ExpectedResourceVersion+1 || !reflect.DeepEqual(instance.Operator, want) {
		return Instance{}, NewError(ReasonDependencyUnavailable)
	}
	return instance, nil
}

func (service *Service) Drain(ctx context.Context, request DrainRequest) (Instance, error) {
	if err := request.Validate(); err != nil {
		return Instance{}, err
	}
	now, err := service.now()
	if err != nil || !request.DeadlineAt.After(now) {
		return Instance{}, NewError(ReasonInvalidRequest)
	}
	eventID, err := service.eventID(ctx)
	if err != nil {
		return Instance{}, err
	}
	instance, err := service.dependencies.Repository.Drain(ctx, DrainCommand{Request: request, EventID: eventID, Now: now})
	instance, err = service.checked(instance, err)
	if err != nil {
		return Instance{}, err
	}
	if !matchesFence(instance, request.Fence) || !instance.Draining || instance.DrainDeadlineAt == nil || !instance.DrainDeadlineAt.Equal(request.DeadlineAt) || instance.Status != StatusRegistered {
		return Instance{}, NewError(ReasonDependencyUnavailable)
	}
	return instance, nil
}

func (service *Service) Deregister(ctx context.Context, fence Fence) (Instance, error) {
	if err := fence.Validate(); err != nil {
		return Instance{}, err
	}
	now, err := service.now()
	if err != nil {
		return Instance{}, err
	}
	eventID, err := service.eventID(ctx)
	if err != nil {
		return Instance{}, err
	}
	instance, err := service.dependencies.Repository.Deregister(ctx, DeregisterCommand{Fence: fence, EventID: eventID, Now: now})
	instance, err = service.checked(instance, err)
	if err != nil {
		return Instance{}, err
	}
	if !matchesFence(instance, fence) || instance.Status != StatusDeregistered || instance.Draining || instance.DrainDeadlineAt != nil || !instance.LeaseExpiresAt.Equal(now) {
		return Instance{}, NewError(ReasonDependencyUnavailable)
	}
	return instance, nil
}

func (service *Service) Expire(ctx context.Context, tenantID string, limit uint64) ([]Instance, error) {
	if !tenantPattern.MatchString(tenantID) || len(tenantID) > 128 || limit == 0 || limit > 1000 {
		return nil, NewError(ReasonInvalidRequest)
	}
	now, err := service.now()
	if err != nil {
		return nil, err
	}
	eventIDs := make([]string, limit)
	for index := range eventIDs {
		eventIDs[index], err = service.eventID(ctx)
		if err != nil {
			return nil, err
		}
	}
	instances, err := service.dependencies.Repository.Expire(ctx, ExpireCommand{TenantID: tenantID, EventIDs: eventIDs, Now: now, Limit: limit})
	if err != nil {
		return nil, normalize(err)
	}
	for _, instance := range instances {
		if instance.Validate() != nil || instance.TenantID != tenantID || instance.Status != StatusExpired || !instance.LeaseExpiresAt.Equal(now) {
			return nil, NewError(ReasonDependencyUnavailable)
		}
	}
	return instances, nil
}

func (service *Service) Snapshot(ctx context.Context, query DiscoveryQuery) (Snapshot, error) {
	if err := query.Validate(); err != nil {
		return Snapshot{}, err
	}
	now, err := service.now()
	if err != nil {
		return Snapshot{}, err
	}
	snapshot, err := service.dependencies.Repository.Snapshot(ctx, query, now)
	if err != nil {
		return Snapshot{}, normalize(err)
	}
	if snapshot.Revision > MaxSafeInteger {
		return Snapshot{}, NewError(ReasonDependencyUnavailable)
	}
	for _, instance := range snapshot.Instances {
		if instance.Validate() != nil || !instance.DiscoverableAt(now, query) {
			return Snapshot{}, NewError(ReasonDependencyUnavailable)
		}
	}
	return snapshot, nil
}

func (service *Service) now() (time.Time, error) {
	now := service.dependencies.Clock.Now()
	if !utc(now) {
		return time.Time{}, NewError(ReasonDependencyUnavailable)
	}
	return now, nil
}

func (service *Service) eventID(ctx context.Context) (string, error) {
	value, err := service.dependencies.IDs.NewEventID(ctx)
	if err != nil || !validPrefixedUUID(value, "evt_") {
		return "", NewError(ReasonDependencyUnavailable)
	}
	return value, nil
}

func (service *Service) checked(instance Instance, err error) (Instance, error) {
	if err != nil {
		return Instance{}, normalize(err)
	}
	if err := instance.Validate(); err != nil {
		return Instance{}, NewError(ReasonDependencyUnavailable)
	}
	return instance, nil
}

func matchesFence(instance Instance, fence Fence) bool {
	return instance.TenantID == fence.TenantID && instance.InstanceID == fence.InstanceID && instance.SessionID == fence.SessionID && instance.LeaseID == fence.LeaseID && instance.Generation == fence.Generation
}

func normalize(err error) error {
	if err == nil {
		return nil
	}
	for _, reason := range []ErrorReason{ReasonInvalidRequest, ReasonNotFound, ReasonSessionReused, ReasonGenerationFenced, ReasonLeaseExpired, ReasonHeartbeatStale, ReasonResourceConflict, ReasonIdempotencyConflict, ReasonRevisionOverflow, ReasonGenerationOverflow, ReasonDependencyUnavailable} {
		if HasReason(err, reason) {
			return Error{Reason: reason}
		}
	}
	return NewError(ReasonDependencyUnavailable)
}
