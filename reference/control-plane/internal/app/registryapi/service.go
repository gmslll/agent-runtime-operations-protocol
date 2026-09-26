package registryapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type Core interface {
	Register(context.Context, registry.RegisterRequest) (registry.Registration, error)
	Keepalive(context.Context, registry.KeepaliveRequest) (registry.Instance, error)
	CompareAndSwap(context.Context, registry.CASRequest) (registry.Instance, error)
	Drain(context.Context, registry.DrainRequest) (registry.Instance, error)
	Deregister(context.Context, registry.Fence) (registry.Instance, error)
	Snapshot(context.Context, registry.DiscoveryQuery) (registry.Snapshot, error)
}

type Dependencies struct {
	Core              Core
	UoW               platformports.UnitOfWork
	Clock             platformports.Clock
	Authorizer        Authorizer
	LeaseTTL          time.Duration
	KeepaliveInterval time.Duration
	RevisionNotifier  RevisionNotifier
}

// RevisionNotifier is an optional after-commit latency hint for P16 Watch.
// Durable ledger polling remains authoritative, so notification has no error
// channel and can never roll back a committed mutation.
type RevisionNotifier interface {
	Notify(string, uint64)
}

type Service struct{ dependencies Dependencies }

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Core == nil || dependencies.UoW == nil || dependencies.Clock == nil || dependencies.Authorizer == nil || dependencies.LeaseTTL <= 0 || dependencies.LeaseTTL > registry.MaxLeaseTTL || dependencies.KeepaliveInterval <= 0 || dependencies.KeepaliveInterval >= dependencies.LeaseTTL {
		return nil, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return &Service{dependencies: dependencies}, nil
}

func (service *Service) Register(ctx context.Context, caller Caller, input RegisterInput) (registry.Registration, error) {
	if err := service.authorize(ctx, caller, OperationRegister, input.InstanceID, ""); err != nil {
		return registry.Registration{}, err
	}
	keyDigest := digest([]byte(input.IdempotencyKey))
	requestDigest, err := semanticDigest(struct {
		TenantID, PrincipalID, CredentialID, InstanceID, SessionID, ServiceID, Environment string
		Endpoint                                                                           registry.Endpoint
		Bindings                                                                           []registry.Binding
		Runtime                                                                            registry.RuntimeState
	}{caller.TenantID, caller.PrincipalID, caller.CredentialID, input.InstanceID, input.SessionID, input.ServiceID, input.Environment, input.Endpoint, input.Bindings, input.Runtime})
	if err != nil {
		return registry.Registration{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	request := registry.RegisterRequest{
		TenantID: caller.TenantID, InstanceID: input.InstanceID, SessionID: input.SessionID,
		ServiceID: input.ServiceID, Environment: input.Environment, Endpoint: input.Endpoint,
		Bindings: input.Bindings, Runtime: input.Runtime, IdempotencyKeyDigest: keyDigest,
		IdempotencyRequestDigest: requestDigest,
	}
	var result registry.Registration
	err = service.dependencies.UoW.Within(ctx, func(transactionContext context.Context) error {
		var callErr error
		result, callErr = service.dependencies.Core.Register(transactionContext, request)
		return callErr
	})
	if err == nil && !result.Replay {
		service.notify(caller.TenantID, result.Instance.RegistryRevision)
	}
	return result, normalize(err)
}

func (service *Service) Keepalive(ctx context.Context, caller Caller, input KeepaliveInput) (registry.Instance, error) {
	if err := service.authorize(ctx, caller, OperationKeepalive, input.InstanceID, ""); err != nil {
		return registry.Instance{}, err
	}
	request := registry.KeepaliveRequest{TenantID: caller.TenantID, InstanceID: input.InstanceID, SessionID: input.SessionID, LeaseID: input.LeaseID, Generation: input.Generation, HeartbeatSequence: input.HeartbeatSequence, ReportedAt: input.ReportedAt, Ready: input.Ready, ActiveRuns: input.ActiveRuns, AvailableSlots: input.AvailableSlots, QueueDepth: input.QueueDepth}
	return service.mutate(ctx, caller.TenantID, func(transactionContext context.Context) (registry.Instance, error) {
		return service.dependencies.Core.Keepalive(transactionContext, request)
	})
}

func (service *Service) Operate(ctx context.Context, caller Caller, input OperateInput) (registry.Instance, error) {
	if err := service.authorize(ctx, caller, OperationOperate, input.InstanceID, ""); err != nil {
		return registry.Instance{}, err
	}
	request := registry.CASRequest{TenantID: caller.TenantID, InstanceID: input.InstanceID, ExpectedResourceVersion: input.ExpectedResourceVersion, Enabled: input.Enabled, Weight: input.Weight, Priority: input.Priority, MaintenanceReason: input.MaintenanceReason}
	return service.mutate(ctx, caller.TenantID, func(transactionContext context.Context) (registry.Instance, error) {
		return service.dependencies.Core.CompareAndSwap(transactionContext, request)
	})
}

func (service *Service) Drain(ctx context.Context, caller Caller, input DrainInput) (registry.Instance, error) {
	if err := service.authorize(ctx, caller, OperationDrain, input.InstanceID, ""); err != nil {
		return registry.Instance{}, err
	}
	request := registry.DrainRequest{Fence: registry.Fence{TenantID: caller.TenantID, InstanceID: input.InstanceID, SessionID: input.SessionID, LeaseID: input.LeaseID, Generation: input.Generation}, DeadlineAt: input.DeadlineAt}
	return service.mutate(ctx, caller.TenantID, func(transactionContext context.Context) (registry.Instance, error) {
		return service.dependencies.Core.Drain(transactionContext, request)
	})
}

func (service *Service) Deregister(ctx context.Context, caller Caller, input DeregisterInput) (registry.Instance, error) {
	if err := service.authorize(ctx, caller, OperationDeregister, input.InstanceID, ""); err != nil {
		return registry.Instance{}, err
	}
	fence := registry.Fence{TenantID: caller.TenantID, InstanceID: input.InstanceID, SessionID: input.SessionID, LeaseID: input.LeaseID, Generation: input.Generation}
	return service.mutate(ctx, caller.TenantID, func(transactionContext context.Context) (registry.Instance, error) {
		return service.dependencies.Core.Deregister(transactionContext, fence)
	})
}

func (service *Service) Discover(ctx context.Context, caller Caller, input DiscoverInput) (Snapshot, error) {
	if err := service.authorize(ctx, caller, OperationDiscover, "", input.AgentID); err != nil {
		return Snapshot{}, err
	}
	serverTime := service.dependencies.Clock.Now()
	if serverTime.IsZero() || serverTime.Location() != time.UTC {
		return Snapshot{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	result, err := service.dependencies.Core.Snapshot(ctx, registry.DiscoveryQuery{TenantID: caller.TenantID, AgentID: input.AgentID, AgentVersion: input.AgentVersion, SkillID: input.SkillID, ProtocolVersion: input.ProtocolVersion})
	if err != nil {
		return Snapshot{}, normalize(err)
	}
	return Snapshot{Revision: result.Revision, CompactionWatermark: result.CompactionWatermark, ServerTime: serverTime, Instances: result.Instances}, nil
}

func (service *Service) LeaseTTL() time.Duration { return service.dependencies.LeaseTTL }
func (service *Service) KeepaliveInterval() time.Duration {
	return service.dependencies.KeepaliveInterval
}

func (service *Service) Name() string { return "registry-api-service" }

func (service *Service) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	now := service.dependencies.Clock.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return nil
}

func (service *Service) authorize(ctx context.Context, caller Caller, operation Operation, instanceID, agentID string) error {
	if err := caller.Validate(); err != nil {
		return ErrForbidden
	}
	if err := service.dependencies.Authorizer.Authorize(ctx, AuthorizationRequest{Caller: caller, Operation: operation, InstanceID: instanceID, AgentID: agentID}); err != nil {
		return ErrForbidden
	}
	return nil
}

func (service *Service) mutate(ctx context.Context, tenantID string, callback func(context.Context) (registry.Instance, error)) (registry.Instance, error) {
	var result registry.Instance
	err := service.dependencies.UoW.Within(ctx, func(transactionContext context.Context) error {
		var callErr error
		result, callErr = callback(transactionContext)
		return callErr
	})
	if err == nil {
		service.notify(tenantID, result.RegistryRevision)
	}
	return result, normalize(err)
}

func (service *Service) notify(tenantID string, revision uint64) {
	if service.dependencies.RevisionNotifier != nil && revision > 0 {
		service.dependencies.RevisionNotifier.Notify(tenantID, revision)
	}
}

func semanticDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digest(encoded), nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func normalize(err error) error {
	if err == nil || errors.Is(err, ErrForbidden) {
		return err
	}
	for _, reason := range []registry.ErrorReason{registry.ReasonInvalidRequest, registry.ReasonNotFound, registry.ReasonSessionReused, registry.ReasonGenerationFenced, registry.ReasonLeaseExpired, registry.ReasonHeartbeatStale, registry.ReasonResourceConflict, registry.ReasonIdempotencyConflict, registry.ReasonRevisionOverflow, registry.ReasonResourceOverflow, registry.ReasonGenerationOverflow, registry.ReasonDependencyUnavailable} {
		if registry.HasReason(err, reason) {
			return registry.NewError(reason)
		}
	}
	return registry.NewError(registry.ReasonDependencyUnavailable)
}
