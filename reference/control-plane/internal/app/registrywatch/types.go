// Package registrywatch implements the P16 long-poll and recovery semantics
// over the append-only P14 registry ledger. Public wire types remain owned by
// the P15 OpenAPI contract.
package registrywatch

import (
	"context"
	"errors"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

const (
	MaxEvents     = uint64(10000)
	MaxWait       = 30 * time.Second
	DefaultPoll   = 250 * time.Millisecond
	SchemaVersion = 1
)

var (
	ErrCompacted             = errors.New("registry watch revision compacted")
	ErrForbidden             = errors.New("registry watch forbidden")
	ErrDependencyUnavailable = errors.New("registry watch dependency unavailable")
)

type Reader interface {
	EventWindow(context.Context, string, uint64, uint64) (registry.EventWindow, error)
}

type Authorizer interface {
	Authorize(context.Context, registryapi.AuthorizationRequest) error
}

type Subscription interface {
	C() <-chan struct{}
	Close()
}

type Notifier interface {
	Subscribe(string) Subscription
	Notify(string, uint64)
}

type WatchInput struct {
	AfterRevision uint64
	Wait          time.Duration
	Metadata      platform.RequestMetadata
}

type Changes struct {
	Revision            uint64
	CompactionWatermark uint64
	Events              []registry.Event
}

type Dependencies struct {
	Reader       Reader
	Authorizer   Authorizer
	Notifier     Notifier
	Coordinator  Coordinator
	PollInterval time.Duration
	MaxEvents    uint64
}

type CompactionResult struct {
	Revision          uint64
	PreviousWatermark uint64
	Watermark         uint64
}

type Leadership interface {
	Compact(context.Context, uint64) (CompactionResult, error)
	Close() error
}

type Coordinator interface {
	Acquire(context.Context, string) (Leadership, error)
	Check(context.Context) error
}
