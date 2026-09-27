package streaming

import (
	"context"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

type Reader interface {
	Binding(context.Context, string, string) (run.AgentBinding, error)
	Read(context.Context, string, string, uint64, int) (Page, error)
}

type Waiter interface {
	Wait(context.Context, time.Duration) error
}

type Sink interface {
	Start() error
	Event(Record) error
	Heartbeat() error
}

type Dependencies struct {
	Reader         Reader
	Authorizer     run.Authorizer
	Waiter         Waiter
	PollInterval   time.Duration
	HeartbeatEvery int
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Reader == nil || dependencies.Authorizer == nil || dependencies.Waiter == nil || dependencies.PollInterval < 10*time.Millisecond || dependencies.PollInterval > 5*time.Second || dependencies.HeartbeatEvery < 1 || dependencies.HeartbeatEvery > 120 {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}

type TimerWaiter struct{}

func (TimerWaiter) Wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
