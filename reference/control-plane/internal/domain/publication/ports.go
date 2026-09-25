package publication

import (
	"context"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

// Repository is the immutable Publication persistence port. Create and every
// future mutation must receive the transaction-bearing context supplied by
// UnitOfWork. Create and read hydration must call Record.ValidateWithDigest
// with the service's pinned ManifestDigester. Reads may use either that
// transaction or the durable store.
type Repository interface {
	Create(context.Context, *Record) error
	Get(context.Context, string, string, string) (Record, error)
	GetByIdempotencyDigest(context.Context, string, string) (Record, error)
}

type AuthorizationRequest struct {
	Caller    Caller
	Operation Operation
	AgentID   string
}

func (request AuthorizationRequest) Validate() error {
	if err := request.Caller.Validate(); err != nil {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	if !request.Operation.valid() || !agentIDPattern.MatchString(request.AgentID) {
		return NewError(CategoryAuthorization, ReasonPublicationForbidden)
	}
	return nil
}

// Authorizer is mandatory and fail-closed. Publish and Get construct their
// respective fixed Operation value; callers cannot select an operation.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

// BundleValidator must be deterministic and offline. Implementations may read
// only the supplied archive and locally pinned validation resources; network,
// DNS, filesystem escape, environment fallback, and endpoint probing are not
// part of this port.
type BundleValidator interface {
	ValidateBundle(context.Context, []byte) (ValidatedBundle, error)
}

// ManifestDigester is the single pinned RFC 8785 JCS digest implementation
// used at validator, service, and storage boundaries.
type ManifestDigester interface {
	DigestManifest(context.Context, []byte) (string, error)
}

// RequestFingerprinter digests the complete validated semantic request. The
// returned value is persisted as IdempotencyRequestDigest.
type RequestFingerprinter interface {
	DigestRequest(context.Context, RequestFingerprint) (string, error)
}

// PublicationService freezes the P12 use-case boundary without exporting a
// second public DTO. Root CLI code must call the public HTTP/SDK contract and
// cannot import this nested-module internal package.
type PublicationService interface {
	Publish(context.Context, PublishRequest) (PublishResult, error)
	Get(context.Context, GetRequest) (GetResult, error)
}

// Dependencies deliberately reuse the P08/P09 seams. Fault checkpoints use
// the fixed platform checkpoint set; durable observations and publication
// insertion therefore participate in the same transaction-bearing context.
type Dependencies struct {
	Clock            platformports.Clock
	IDs              platformports.IDSource
	Faults           platformports.FaultHook
	UoW              platformports.UnitOfWork
	Observability    observability.ObservationWriter
	Authorizer       Authorizer
	Repository       Repository
	Validator        BundleValidator
	ManifestDigester ManifestDigester
	Fingerprinter    RequestFingerprinter
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Faults == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Authorizer == nil || dependencies.Repository == nil || dependencies.Validator == nil || dependencies.ManifestDigester == nil || dependencies.Fingerprinter == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
