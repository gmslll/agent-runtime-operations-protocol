package registry

import (
	"context"
	"errors"
	"sync"
	"time"

	registrywire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/registry"
)

type CapacityState struct {
	Ready          bool
	ActiveRuns     uint64
	AvailableSlots uint64
	QueueDepth     uint64
}

type Lease struct {
	client                         Client
	mu                             sync.Mutex
	instanceID, sessionID, leaseID string
	generation, sequence           uint64
	interval                       time.Duration
	closed                         bool
}

func NewLease(client Client, registration Registration) (*Lease, error) {
	if registration.KeepaliveIntervalSeconds == 0 || registration.LeaseTTLSeconds <= registration.KeepaliveIntervalSeconds {
		return nil, errors.New("invalid registry lease policy")
	}
	return &Lease{client: client, instanceID: string(registration.Instance.InstanceID), sessionID: string(registration.Instance.SessionID), leaseID: string(registration.Instance.LeaseID), generation: uint64(registration.Instance.Generation), sequence: 0, interval: time.Duration(registration.KeepaliveIntervalSeconds) * time.Second}, nil
}

func (lease *Lease) Keepalive(ctx context.Context, state CapacityState) (registrywire.RegistryLease, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return registrywire.RegistryLease{}, errors.New("registry lease is closed")
	}
	if lease.sequence >= 9007199254740991 {
		return registrywire.RegistryLease{}, errors.New("registry heartbeat sequence exhausted")
	}
	lease.sequence++
	result, err := lease.client.Keepalive(ctx, KeepaliveRequest{InstanceID: lease.instanceID, SessionID: lease.sessionID, LeaseID: lease.leaseID, Generation: lease.generation, HeartbeatSequence: lease.sequence, ReportedAt: time.Now().UTC(), Ready: state.Ready, ActiveRuns: state.ActiveRuns, AvailableSlots: state.AvailableSlots, QueueDepth: state.QueueDepth})
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) && (remote.Code == "INSTANCE_GENERATION_FENCED" || remote.Code == "REGISTRY_LEASE_EXPIRED" || remote.Code == "REGISTRY_INSTANCE_NOT_FOUND") {
			lease.closed = true
		}
		return registrywire.RegistryLease{}, err
	}
	return result, nil
}

func (lease *Lease) Run(ctx context.Context, state func(context.Context) (CapacityState, error)) error {
	if state == nil {
		return errors.New("registry keepalive state provider is required")
	}
	ticker := time.NewTicker(lease.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			current, err := state(ctx)
			if err != nil {
				return err
			}
			if _, err := lease.Keepalive(ctx, current); err != nil {
				return err
			}
		}
	}
}

func (lease *Lease) Drain(ctx context.Context, deadline time.Time) (registrywire.RuntimeInstance, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return registrywire.RuntimeInstance{}, errors.New("registry lease is closed")
	}
	return lease.client.Drain(ctx, DrainRequest{InstanceID: lease.instanceID, SessionID: lease.sessionID, LeaseID: lease.leaseID, Generation: lease.generation, DeadlineAt: deadline})
}

func (lease *Lease) Deregister(ctx context.Context) (registrywire.RuntimeInstance, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return registrywire.RuntimeInstance{}, errors.New("registry lease is closed")
	}
	result, err := lease.client.Deregister(ctx, DeregisterRequest{InstanceID: lease.instanceID, SessionID: lease.sessionID, LeaseID: lease.leaseID, Generation: lease.generation})
	if err == nil {
		lease.closed = true
	}
	return result, err
}
