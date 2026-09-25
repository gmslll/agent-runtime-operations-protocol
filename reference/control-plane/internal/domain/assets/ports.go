package assets

import (
	"context"
	"net"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

// RunGrantRequest is the only authorization input. Operation is fixed by the
// service method; callers cannot widen it.
type RunGrantRequest struct {
	Binding Binding
}

func (request RunGrantRequest) Validate() error {
	if err := request.Binding.Validate(); err != nil {
		return NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	return nil
}

// RunGrantAuthorizer is mandatory. A nil authorizer, a denied decision, and
// any unexpected error all fail closed.
type RunGrantAuthorizer interface {
	Authorize(context.Context, RunGrantRequest) error
}

// AssetRepository stores quarantine and ready objects. Put is idempotent for
// an identical binding and state, and conflicts when the same asset id is
// reused with a different binding.
type AssetRepository interface {
	Put(context.Context, Asset) error
	Get(context.Context, string, string) (Asset, error)
}

// GrantRepository stores opaque-digest grants. Consume is the only use
// increment and must enforce nbf, exp, revocation, and max uses atomically.
type GrantRepository interface {
	Save(context.Context, Grant) error
	GetGrant(context.Context, string, string) (Grant, error)
	FindByOpaqueDigest(context.Context, string, string) (Grant, error)
	FindByIdempotency(context.Context, string, string) (Grant, error)
	Consume(context.Context, string, string, time.Time) (Grant, error)
	Revoke(context.Context, string, string, time.Time) error
}

// NameResolver resolves a hostname at the moment of connect or redirect.
// Implementations must not cache across calls.
type NameResolver interface {
	LookupIP(context.Context, string) ([]net.IP, error)
}

// TokenIssuer deterministically derives an opaque bearer value from a durable
// grant. Implementations must use deployment secret material, be stable across
// restarts while that key version is active, and never persist plaintext.
type TokenIssuer interface {
	KeyID() string
	IssueToken(context.Context, Grant) (OpaqueToken, error)
}

// Dependencies fail closed when the authorizer or any other required port is
// missing.
type Dependencies struct {
	Clock      platformports.Clock
	IDs        platformports.IDSource
	Faults     platformports.FaultHook
	UoW        platformports.UnitOfWork
	Audit      observability.ObservationWriter
	Authorizer RunGrantAuthorizer
	Assets     AssetRepository
	Grants     GrantRepository
	Resolver   NameResolver
	Tokens     TokenIssuer
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Faults == nil || dependencies.UoW == nil || dependencies.Audit == nil || dependencies.Authorizer == nil || dependencies.Assets == nil || dependencies.Grants == nil || dependencies.Resolver == nil || dependencies.Tokens == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
