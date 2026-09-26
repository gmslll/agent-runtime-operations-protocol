package run

import (
	"context"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type IDSource interface {
	NewRunID(context.Context) (string, error)
	NewOutboxID(context.Context) (string, error)
}
type Authorizer interface {
	Authorize(context.Context, Caller, Operation, AgentBinding) (AuthorizationSnapshot, error)
}
type Repository interface {
	Create(context.Context, Run, string, string, Outbox) error
	Get(context.Context, string, string) (Run, error)
	GetByIdempotency(context.Context, string, string) (Run, string, error)
	Cancel(context.Context, string, string, Command, string, string, time.Time, Outbox) (Run, error)
	Expire(context.Context, string, string, uint64, time.Time, Outbox) (Run, error)
	ReserveEffect(context.Context, EffectReservation) (bool, error)
}
type Dependencies struct {
	Clock         platformports.Clock
	IDs           IDSource
	UoW           platformports.UnitOfWork
	Observability observability.ObservationWriter
	Authorizer    Authorizer
	Repository    Repository
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Authorizer == nil || dependencies.Repository == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
