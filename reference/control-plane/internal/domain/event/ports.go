package event

import (
	"context"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type IDSource interface {
	NewAuditID(context.Context) (string, error)
}

type TokenSource interface {
	NewEventToken(context.Context) (string, error)
}

type SessionCommand struct {
	Request            SessionRequest
	Token, TokenDigest string
	Now, ExpiresAt     time.Time
	LeaseExpiresAt     time.Time
}

type AppendCommand struct {
	Request                                          BatchRequest
	TokenDigest, IdempotencyKeyDigest, RequestDigest string
	Now                                              time.Time
}

type Repository interface {
	CreateSession(context.Context, SessionCommand) (Session, error)
	Append(context.Context, AppendCommand) (Ack, error)
}

type Dependencies struct {
	Clock               platformports.Clock
	IDs                 IDSource
	Tokens              TokenSource
	UoW                 platformports.UnitOfWork
	Observability       observability.ObservationWriter
	Repository          Repository
	ControlPlaneBaseURL string
	SessionTTL          time.Duration
	AttemptLeaseTTL     time.Duration
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Tokens == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Repository == nil || dependencies.SessionTTL < time.Minute || dependencies.SessionTTL > 5*time.Minute || dependencies.AttemptLeaseTTL < dependencies.SessionTTL || dependencies.AttemptLeaseTTL > 10*time.Minute {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return validateBaseURL(dependencies.ControlPlaneBaseURL)
}
