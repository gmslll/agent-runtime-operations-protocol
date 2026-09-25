package publication

import (
	"context"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

// Repository is the immutable Publication persistence port. Create and every
// future mutation must receive the transaction-bearing context supplied by
// UnitOfWork. Reads may use either that transaction or the durable store.
type Repository interface {
	Create(context.Context, *Record) error
	Get(context.Context, string, string) (Record, error)
	GetByIdempotencyDigest(context.Context, string) (Record, error)
}

// BundleValidator must be deterministic and offline. Implementations may read
// only the supplied archive and locally pinned validation resources; network,
// DNS, filesystem escape, environment fallback, and endpoint probing are not
// part of this port.
type BundleValidator interface {
	ValidateBundle(context.Context, []byte) (ValidatedBundle, error)
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
	Clock         platformports.Clock
	IDs           platformports.IDSource
	Faults        platformports.FaultHook
	UoW           platformports.UnitOfWork
	Observability observability.ObservationWriter
	Repository    Repository
	Validator     BundleValidator
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Faults == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Repository == nil || dependencies.Validator == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable, nil)
	}
	return nil
}
