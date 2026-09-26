package dispatch

import (
	"context"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type IDSource interface {
	NewAttemptID(context.Context) (string, error)
	NewDeploymentID(context.Context) (string, error)
	NewTokenID(context.Context) (string, error)
	NewAuditID(context.Context) (string, error)
}

type RunReader interface {
	DispatchableRun(context.Context, string, string) (RunView, error)
}

type CandidateSource interface {
	Candidates(context.Context, RunView) ([]Candidate, error)
}

type Authorizer interface {
	Authorize(context.Context, Caller, Operation, run.AgentBinding) error
}

type ReserveCommand struct {
	Run           RunView
	Candidate     Candidate
	AttemptID     string
	DeploymentID  string
	TokenID       string
	SigningKey    KeyMetadata
	SigningKeys   []KeyMetadata
	KeyDigest     string
	RequestDigest string
	Now           time.Time
	LeaseExpires  time.Time
	TicketExpires time.Time
}

type Repository interface {
	GetByIdempotency(context.Context, string, string) (Attempt, string, error)
	Reserve(context.Context, ReserveCommand) (Attempt, bool, error)
	SyncKeys(context.Context, []KeyMetadata, time.Time) error
	Keys(context.Context, time.Time) ([]KeyMetadata, error)
}

// Signer is the only component permitted to access ES256 private material.
// It exposes public metadata and raw JWS signatures, never private scalars.
type Signer interface {
	ActiveKey(context.Context, time.Time) (KeyMetadata, error)
	VerificationKeys(context.Context, time.Time) ([]KeyMetadata, error)
	Sign(context.Context, string, []byte) ([]byte, error)
}

type Dependencies struct {
	Clock         platformports.Clock
	IDs           IDSource
	UoW           platformports.UnitOfWork
	Observability observability.ObservationWriter
	Runs          RunReader
	Candidates    CandidateSource
	Authorizer    Authorizer
	Repository    Repository
	Signer        Signer
	Issuer        string
	TicketTTL     time.Duration
	AttemptLease  time.Duration
	MaxTokenTTL   time.Duration
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Runs == nil || dependencies.Candidates == nil || dependencies.Authorizer == nil || dependencies.Repository == nil || dependencies.Signer == nil || dependencies.TicketTTL < time.Minute || dependencies.TicketTTL > 5*time.Minute || dependencies.AttemptLease < dependencies.TicketTTL || dependencies.AttemptLease > 15*time.Minute || dependencies.MaxTokenTTL < dependencies.TicketTTL || dependencies.MaxTokenTTL > 5*time.Minute {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	parsed, err := urlParseIssuer(dependencies.Issuer)
	if err != nil || parsed == "" {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
