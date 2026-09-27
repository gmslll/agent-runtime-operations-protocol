package streaming

import (
	"context"
	"errors"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

type Service struct{ deps Dependencies }

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Validate() != nil {
		return nil, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return &Service{deps: dependencies}, nil
}

func (service *Service) Name() string { return "streaming-service" }

func (service *Service) Check(ctx context.Context) error {
	if checker, ok := service.deps.Reader.(interface{ Check(context.Context) error }); ok {
		if err := checker.Check(ctx); err != nil {
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
	}
	return nil
}

func (service *Service) Stream(ctx context.Context, request Request, sink Sink) error {
	if sink == nil || request.Validate() != nil {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	binding, err := service.deps.Reader.Binding(ctx, request.Caller.TenantID, request.RunID)
	if err != nil {
		return normalize(err)
	}
	if _, err = service.deps.Authorizer.Authorize(ctx, request.Caller, "run.read", binding); err != nil {
		return normalizeAuthorization(err)
	}
	page, err := service.deps.Reader.Read(ctx, request.Caller.TenantID, request.RunID, request.After, MaxReplayEvents)
	if err != nil {
		return normalize(err)
	}
	if err = validateCursor(request.RunID, request.After, page); err != nil {
		return err
	}
	if err = sink.Start(); err != nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	after := request.After
	idle := 0
	for {
		if page.Validate(request.RunID, after) != nil {
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		for _, record := range page.Records {
			if err = sink.Event(record); err != nil {
				return NewError(CategoryDependency, ReasonDependencyUnavailable)
			}
			after = record.Sequence
			idle = 0
		}
		if page.Terminal && after == page.Latest {
			return nil
		}
		if len(page.Records) == MaxReplayEvents {
			page, err = service.deps.Reader.Read(ctx, request.Caller.TenantID, request.RunID, after, MaxReplayEvents)
			if err != nil {
				return normalize(err)
			}
			continue
		}
		if err = service.deps.Waiter.Wait(ctx, service.deps.PollInterval); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		idle++
		if idle%service.deps.HeartbeatEvery == 0 {
			if err = sink.Heartbeat(); err != nil {
				return NewError(CategoryDependency, ReasonDependencyUnavailable)
			}
		}
		page, err = service.deps.Reader.Read(ctx, request.Caller.TenantID, request.RunID, after, MaxReplayEvents)
		if err != nil {
			return normalize(err)
		}
		if err = validateCursor(request.RunID, after, page); err != nil {
			return err
		}
	}
}

func validateCursor(runID string, after uint64, page Page) error {
	if page.Latest < after {
		return NewError(CategoryCursor, ReasonCursorAhead)
	}
	if after == MaxSafeInteger {
		if len(page.Records) != 0 || page.Latest != after {
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		return nil
	}
	if after > 0 && page.FirstAvailable > after+1 {
		return CursorExpired(runID, page.Latest)
	}
	return nil
}

func normalize(err error) error {
	if typed, ok := AsError(err); ok {
		return typed
	}
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}

func normalizeAuthorization(err error) error {
	if typed, ok := AsError(err); ok {
		return typed
	}
	if typed, ok := run.AsError(err); ok {
		switch typed.Category {
		case run.CategoryAuthentication:
			return NewError(CategoryAuthentication, ReasonAuthentication)
		case run.CategoryDependency:
			return NewError(CategoryDependency, ReasonDependencyUnavailable)
		case run.CategoryAuthorization:
			return NewError(CategoryAuthorization, ReasonAuthorization)
		}
	}
	return NewError(CategoryAuthorization, ReasonAuthorization)
}
