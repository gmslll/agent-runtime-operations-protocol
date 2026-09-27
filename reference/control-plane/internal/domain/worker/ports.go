package worker

import (
	"context"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type IDSource interface {
	NewClaimID(context.Context) (string, error)
	NewAttemptID(context.Context) (string, error)
	NewTokenID(context.Context) (string, error)
	NewEventID(context.Context) (string, error)
	NewOutboxID(context.Context) (string, error)
	NewAuditID(context.Context) (string, error)
}

type TokenSource interface {
	NewLeaseToken(context.Context) (string, error)
}

type Authorizer interface {
	Authorize(context.Context, Caller, Operation, string) error
}

type ClaimCommand struct {
	Request                               ClaimRequest
	ClaimID, LeaseToken, LeaseTokenDigest string
	ReplacementAttemptID, TokenID         string
	Now, LeaseExpiresAt                   time.Time
}

type RenewCommand struct {
	Request          RenewRequest
	LeaseTokenDigest string
	Now, ExpiresAt   time.Time
}

type CompleteCommand struct {
	Request                          CompleteRequest
	LeaseTokenDigest, KeyDigest      string
	RequestDigest, EventID, OutboxID string
	Now                              time.Time
}

type ReleaseCommand struct {
	Request                       ReleaseRequest
	LeaseTokenDigest              string
	ReplacementAttemptID, TokenID string
	Now                           time.Time
}

type Repository interface {
	Claim(context.Context, ClaimCommand) (Claim, error)
	Renew(context.Context, RenewCommand) (Claim, error)
	Complete(context.Context, CompleteCommand) (bool, error)
	Release(context.Context, ReleaseCommand) error
	Check(context.Context) error
}

type Dependencies struct {
	Clock         platformports.Clock
	IDs           IDSource
	Tokens        TokenSource
	UoW           platformports.UnitOfWork
	Observability observability.ObservationWriter
	Authorizer    Authorizer
	Repository    Repository
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Tokens == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Authorizer == nil || dependencies.Repository == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
