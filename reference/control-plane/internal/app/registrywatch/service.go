package registrywatch

import (
	"context"
	"errors"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type Service struct{ dependencies Dependencies }

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Reader == nil || dependencies.Authorizer == nil || dependencies.Notifier == nil || dependencies.Coordinator == nil {
		return nil, ErrDependencyUnavailable
	}
	if dependencies.PollInterval == 0 {
		dependencies.PollInterval = DefaultPoll
	}
	if dependencies.MaxEvents == 0 {
		dependencies.MaxEvents = MaxEvents
	}
	if dependencies.PollInterval < time.Millisecond || dependencies.PollInterval > MaxWait || dependencies.MaxEvents == 0 || dependencies.MaxEvents > MaxEvents {
		return nil, ErrDependencyUnavailable
	}
	return &Service{dependencies: dependencies}, nil
}

func (service *Service) Name() string { return "registry-recovery-service" }

func (service *Service) Check(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrDependencyUnavailable
	}
	if err := service.dependencies.Coordinator.Check(ctx); err != nil {
		return ErrDependencyUnavailable
	}
	return nil
}

func (service *Service) Compact(ctx context.Context, nodeID string, watermark uint64) (CompactionResult, error) {
	if nodeID == "" || len(nodeID) > 128 || watermark > registry.MaxSafeInteger {
		return CompactionResult{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	leadership, err := service.dependencies.Coordinator.Acquire(ctx, nodeID)
	if err != nil {
		return CompactionResult{}, ErrDependencyUnavailable
	}
	defer leadership.Close()
	result, err := leadership.Compact(ctx, watermark)
	if err != nil {
		if registry.HasReason(err, registry.ReasonInvalidRequest) {
			return CompactionResult{}, err
		}
		return CompactionResult{}, ErrDependencyUnavailable
	}
	return result, nil
}

func (service *Service) Watch(ctx context.Context, caller registryapi.Caller, input WatchInput) (Changes, error) {
	if !caller.Valid() || input.AfterRevision > registry.MaxSafeInteger || input.Wait < time.Second || input.Wait > MaxWait {
		return Changes{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	if err := service.dependencies.Authorizer.Authorize(ctx, registryapi.AuthorizationRequest{Caller: caller, Operation: registryapi.OperationWatch}); err != nil {
		return Changes{}, ErrForbidden
	}
	first, err := service.read(ctx, caller.TenantID, input.AfterRevision)
	if err != nil || ready(first, input.AfterRevision) {
		return first, err
	}

	subscription := service.dependencies.Notifier.Subscribe(caller.TenantID)
	defer subscription.Close()
	second, err := service.read(ctx, caller.TenantID, input.AfterRevision)
	if err != nil || ready(second, input.AfterRevision) {
		return second, err
	}

	timer := time.NewTimer(input.Wait)
	defer timer.Stop()
	ticker := time.NewTicker(service.dependencies.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Changes{}, ctx.Err()
		case <-subscription.C():
		case <-ticker.C:
		case <-timer.C:
			return service.read(ctx, caller.TenantID, input.AfterRevision)
		}
		result, readErr := service.read(ctx, caller.TenantID, input.AfterRevision)
		if readErr != nil || ready(result, input.AfterRevision) {
			return result, readErr
		}
	}
}

func (service *Service) read(ctx context.Context, tenantID string, afterRevision uint64) (Changes, error) {
	window, err := service.dependencies.Reader.EventWindow(ctx, tenantID, afterRevision, service.dependencies.MaxEvents)
	if err != nil {
		if registry.HasReason(err, registry.ReasonInvalidRequest) {
			return Changes{}, registry.NewError(registry.ReasonInvalidRequest)
		}
		return Changes{}, ErrDependencyUnavailable
	}
	if window.Revision > registry.MaxSafeInteger || window.CompactionWatermark > window.Revision || afterRevision > window.Revision {
		return Changes{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	if afterRevision < window.CompactionWatermark {
		return Changes{}, ErrCompacted
	}
	previous := afterRevision
	for _, event := range window.Events {
		if event.Validate() != nil || event.TenantID != tenantID || event.Revision <= previous || event.Revision > window.Revision {
			return Changes{}, ErrDependencyUnavailable
		}
		previous = event.Revision
	}
	return Changes{Revision: window.Revision, CompactionWatermark: window.CompactionWatermark, Events: append([]registry.Event(nil), window.Events...)}, nil
}

func ready(changes Changes, afterRevision uint64) bool {
	return len(changes.Events) != 0 || changes.Revision > afterRevision
}

func NormalizeError(err error) error {
	switch {
	case err == nil, errors.Is(err, ErrCompacted), errors.Is(err, ErrForbidden), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), registry.HasReason(err, registry.ReasonInvalidRequest):
		return err
	default:
		return ErrDependencyUnavailable
	}
}
