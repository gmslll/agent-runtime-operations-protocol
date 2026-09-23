// Package ports contains the deterministic platform seams used by the P08
// application foundation. It intentionally contains no HTTP or adapter code.
package ports

import (
	"context"
	"fmt"
	"time"
)

type Clock interface {
	Now() time.Time
}

type IDKind string

const (
	IDRequest IDKind = "request"
	IDAudit   IDKind = "audit"
	IDTrace   IDKind = "trace"
	IDSpan    IDKind = "span"
)

func (kind IDKind) Validate() error {
	switch kind {
	case IDRequest, IDAudit, IDTrace, IDSpan:
		return nil
	default:
		return fmt.Errorf("unknown ID kind %q", kind)
	}
}

type IDSource interface {
	NewID(context.Context, IDKind) (string, error)
}

type Checkpoint string

const (
	CheckpointRequestAccepted Checkpoint = "platform.request.accepted"
	CheckpointBeforeUseCase   Checkpoint = "platform.before-use-case"
	CheckpointAfterUseCase    Checkpoint = "platform.after-use-case"
)

func (checkpoint Checkpoint) Validate() error {
	switch checkpoint {
	case CheckpointRequestAccepted, CheckpointBeforeUseCase, CheckpointAfterUseCase:
		return nil
	default:
		return fmt.Errorf("unknown fault checkpoint %q", checkpoint)
	}
}

type FaultHook interface {
	Check(context.Context, Checkpoint) error
}

type UnitOfWork interface {
	Within(context.Context, func(context.Context) error) error
}

type ReadinessCheck interface {
	Name() string
	Check(context.Context) error
}
