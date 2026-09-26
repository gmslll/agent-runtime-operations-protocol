package registry

import (
	"context"
	"time"
)

type RegisterCommand struct {
	Request   RegisterRequest
	LeaseID   string
	EventID   string
	Now       time.Time
	TTL       time.Duration
	Keepalive time.Duration
}

type KeepaliveCommand struct {
	Request KeepaliveRequest
	EventID string
	Now     time.Time
	TTL     time.Duration
}

type CASCommand struct {
	Request CASRequest
	EventID string
	Now     time.Time
}

type DrainCommand struct {
	Request DrainRequest
	EventID string
	Now     time.Time
}

type DeregisterCommand struct {
	Fence   Fence
	EventID string
	Now     time.Time
}

type ExpireCommand struct {
	TenantID string
	EventIDs []string
	Now      time.Time
	Limit    uint64
}

type Snapshot struct {
	Revision  uint64
	Instances []Instance
}

type Repository interface {
	Register(context.Context, RegisterCommand) (Registration, error)
	Keepalive(context.Context, KeepaliveCommand) (Instance, error)
	CompareAndSwap(context.Context, CASCommand) (Instance, error)
	Drain(context.Context, DrainCommand) (Instance, error)
	Deregister(context.Context, DeregisterCommand) (Instance, error)
	Expire(context.Context, ExpireCommand) ([]Instance, error)
	Snapshot(context.Context, DiscoveryQuery, time.Time) (Snapshot, error)
	Events(context.Context, string, uint64, uint64) ([]Event, error)
	CompactionWatermark(context.Context, string) (uint64, error)
}

type Clock interface{ Now() time.Time }

type IDSource interface {
	NewLeaseID(context.Context) (string, error)
	NewEventID(context.Context) (string, error)
}

type Dependencies struct {
	Clock             Clock
	IDs               IDSource
	Repository        Repository
	LeaseTTL          time.Duration
	KeepaliveInterval time.Duration
}

func (dependencies Dependencies) Validate() error {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Repository == nil || validateLeasePolicy(dependencies.LeaseTTL, dependencies.KeepaliveInterval) != nil {
		return NewError(ReasonDependencyUnavailable)
	}
	return nil
}
