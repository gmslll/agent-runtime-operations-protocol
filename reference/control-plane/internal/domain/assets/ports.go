package assets

import (
	"context"
	"net"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
)

// RunGrantRequest is the only authorization input. Operation is fixed by the
// service method; callers cannot widen it.
type RunGrantRequest struct {
	Caller    Caller
	RunID     string
	AssetID   string
	Operation Operation
}

func (request RunGrantRequest) Validate() error {
	if err := request.Caller.Validate(); err != nil {
		return err
	}
	if !runPattern.MatchString(request.RunID) || !assetPattern.MatchString(request.AssetID) || !request.Operation.valid() {
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
	FindByOpaqueDigest(context.Context, string, string) (Grant, error)
	FindByIdempotency(context.Context, string, string) (Grant, error)
	Consume(context.Context, string, string, time.Time) (Grant, error)
	Revoke(context.Context, string, string) error
}

// NameResolver resolves a hostname at the moment of connect or redirect.
// Implementations must not cache across calls.
type NameResolver interface {
	LookupIP(context.Context, string) ([]net.IP, error)
}

// Dependencies fail closed when the authorizer or any other required port is
// missing.
type Dependencies struct {
	Clock      platformports.Clock
	Authorizer RunGrantAuthorizer
	Assets     AssetRepository
	Grants     GrantRepository
	Resolver   NameResolver
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.Authorizer == nil || dependencies.Assets == nil || dependencies.Grants == nil || dependencies.Resolver == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
