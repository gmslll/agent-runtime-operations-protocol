package core

import (
	"errors"
	"fmt"
)

type RunState string

const (
	RunQueued          RunState = "queued"
	RunDispatching     RunState = "dispatching"
	RunRunning         RunState = "running"
	RunWaitingInput    RunState = "waiting_input"
	RunCancelRequested RunState = "cancel_requested"
	RunSucceeded       RunState = "succeeded"
	RunFailed          RunState = "failed"
	RunCancelled       RunState = "cancelled"
	RunTimedOut        RunState = "timed_out"
)

var allRunStates = []RunState{RunQueued, RunDispatching, RunRunning, RunWaitingInput, RunCancelRequested, RunSucceeded, RunFailed, RunCancelled, RunTimedOut}
var terminalRunStates = []RunState{RunSucceeded, RunFailed, RunCancelled, RunTimedOut}

type AttemptState string

const (
	AttemptCreated   AttemptState = "created"
	AttemptAssigned  AttemptState = "assigned"
	AttemptAccepted  AttemptState = "accepted"
	AttemptRunning   AttemptState = "running"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptCancelled AttemptState = "cancelled"
	AttemptExpired   AttemptState = "expired"
	AttemptFenced    AttemptState = "fenced"
)

var allAttemptStates = []AttemptState{AttemptCreated, AttemptAssigned, AttemptAccepted, AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptExpired, AttemptFenced}
var terminalAttemptStates = []AttemptState{AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptExpired, AttemptFenced}

type RegistryState string

const (
	RegistryUnregistered RegistryState = "unregistered"
	RegistryRegistered   RegistryState = "registered"
	RegistryDraining     RegistryState = "draining"
	RegistryExpired      RegistryState = "expired"
	RegistryDeregistered RegistryState = "deregistered"
)

var allRegistryStates = []RegistryState{RegistryUnregistered, RegistryRegistered, RegistryDraining, RegistryExpired, RegistryDeregistered}

var runTransitions = transitionSet[RunState](map[RunState][]RunState{
	RunQueued:          {RunDispatching, RunCancelRequested, RunSucceeded, RunFailed, RunCancelled, RunTimedOut},
	RunDispatching:     {RunRunning, RunCancelRequested, RunSucceeded, RunFailed, RunCancelled, RunTimedOut},
	RunRunning:         {RunWaitingInput, RunCancelRequested, RunSucceeded, RunFailed, RunCancelled, RunTimedOut},
	RunWaitingInput:    {RunRunning, RunCancelRequested, RunSucceeded, RunFailed, RunCancelled, RunTimedOut},
	RunCancelRequested: {RunSucceeded, RunFailed, RunCancelled, RunTimedOut},
})

var attemptTransitions = transitionSet[AttemptState](map[AttemptState][]AttemptState{
	AttemptCreated: {AttemptAssigned}, AttemptAssigned: {AttemptAccepted}, AttemptAccepted: {AttemptRunning},
	AttemptRunning: {AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptExpired, AttemptFenced},
})

var registryTransitions = transitionSet[RegistryState](map[RegistryState][]RegistryState{
	RegistryUnregistered: {RegistryRegistered},
	RegistryRegistered:   {RegistryDraining, RegistryExpired, RegistryDeregistered},
	RegistryDraining:     {RegistryExpired, RegistryDeregistered},
	RegistryExpired:      {RegistryRegistered},
	RegistryDeregistered: {RegistryRegistered},
})

func transitionSet[T ~string](source map[T][]T) map[T]map[T]bool {
	result := map[T]map[T]bool{}
	for from, destinations := range source {
		result[from] = map[T]bool{}
		for _, to := range destinations {
			result[from][to] = true
		}
	}
	return result
}

func ValidateRunTransition(from, to RunState) error {
	if !runTransitions[from][to] {
		return fmt.Errorf("invalid Run transition %s -> %s", from, to)
	}
	return nil
}
func ValidateAttemptTransition(from, to AttemptState) error {
	if !attemptTransitions[from][to] {
		return fmt.Errorf("invalid Attempt transition %s -> %s", from, to)
	}
	return nil
}
func ValidateRegistryTransition(from, to RegistryState) error {
	if !registryTransitions[from][to] {
		return fmt.Errorf("invalid Registry transition %s -> %s", from, to)
	}
	return nil
}

func RunStates() []RunState         { return append([]RunState(nil), allRunStates...) }
func RunTerminalStates() []RunState { return append([]RunState(nil), terminalRunStates...) }
func AttemptStates() []AttemptState { return append([]AttemptState(nil), allAttemptStates...) }
func AttemptTerminalStates() []AttemptState {
	return append([]AttemptState(nil), terminalAttemptStates...)
}
func RegistryStates() []RegistryState { return append([]RegistryState(nil), allRegistryStates...) }

type DiscoveryFacts struct {
	LeaseAlive, Healthy, Ready, Enabled, Draining, CapacityAvailable bool
	ProtocolCompatible, BindingMatches                               bool
}

func (facts DiscoveryFacts) Discoverable() bool {
	return facts.LeaseAlive && facts.Healthy && facts.Ready && facts.Enabled && !facts.Draining && facts.CapacityAvailable && facts.ProtocolCompatible && facts.BindingMatches
}

// RegistrySession is a pure generation/fencing state transition helper.
type RegistrySession struct {
	State      RegistryState
	SessionID  string
	Generation uint64
}

var (
	ErrInstanceGenerationFenced   = errors.New("INSTANCE_GENERATION_FENCED")
	ErrLeaseExpired               = errors.New("LEASE_EXPIRED")
	ErrRegistryGenerationOverflow = errors.New("REGISTRY_GENERATION_OVERFLOW")
	ErrRegistrySessionReused      = errors.New("REGISTRY_SESSION_REUSED")
)

func (session RegistrySession) Register(sessionID string) (RegistrySession, error) {
	switch session.State {
	case RegistryUnregistered, RegistryRegistered, RegistryDraining, RegistryExpired, RegistryDeregistered:
	default:
		return session, fmt.Errorf("register rejected in unknown registry state %s", session.State)
	}
	if err := ValidateResourceID(ResourceSession, sessionID); err != nil {
		return session, err
	}
	if session.Generation == ^uint64(0) {
		return session, ErrRegistryGenerationOverflow
	}
	if session.SessionID != "" && session.SessionID == sessionID {
		return session, ErrRegistrySessionReused
	}
	nextState := RegistryRegistered
	if session.State == RegistryDraining {
		// Registration rotates the runtime session and fencing generation, but
		// must not implicitly clear an Instance/Operator drain decision.
		nextState = RegistryDraining
	}
	return RegistrySession{State: nextState, SessionID: sessionID, Generation: session.Generation + 1}, nil
}

func (session RegistrySession) Keepalive(sessionID string, generation uint64) (RegistrySession, error) {
	if err := session.authenticate(sessionID, generation); err != nil {
		return session, err
	}
	if session.State == RegistryExpired {
		return session, ErrLeaseExpired
	}
	if session.State != RegistryRegistered && session.State != RegistryDraining {
		return session, fmt.Errorf("keepalive rejected in registry state %s", session.State)
	}
	return session, nil
}

func (session RegistrySession) Drain(sessionID string, generation uint64) (RegistrySession, error) {
	if err := session.authenticate(sessionID, generation); err != nil {
		return session, err
	}
	if session.State == RegistryDraining {
		return session, nil
	}
	if err := ValidateRegistryTransition(session.State, RegistryDraining); err != nil {
		return session, err
	}
	session.State = RegistryDraining
	return session, nil
}

func (session RegistrySession) Expire(sessionID string, generation uint64) (RegistrySession, error) {
	if err := session.authenticate(sessionID, generation); err != nil {
		return session, err
	}
	if session.State == RegistryExpired {
		return session, nil
	}
	if err := ValidateRegistryTransition(session.State, RegistryExpired); err != nil {
		return session, err
	}
	session.State = RegistryExpired
	return session, nil
}

func (session RegistrySession) Deregister(sessionID string, generation uint64) (RegistrySession, error) {
	if err := session.authenticate(sessionID, generation); err != nil {
		return session, err
	}
	if session.State == RegistryDeregistered {
		return session, nil
	}
	if err := ValidateRegistryTransition(session.State, RegistryDeregistered); err != nil {
		return session, err
	}
	session.State = RegistryDeregistered
	return session, nil
}

func (session RegistrySession) authenticate(sessionID string, generation uint64) error {
	if session.SessionID != sessionID || session.Generation != generation {
		return ErrInstanceGenerationFenced
	}
	return nil
}
