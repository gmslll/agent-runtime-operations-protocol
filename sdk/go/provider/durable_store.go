// Package provider implements the provider-side AROP runtime contract.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when durable provider state does not exist.
var ErrNotFound = errors.New("provider state not found")

// ErrConflict is returned when an idempotency key is reused for different
// immutable input or a compare-and-set precondition no longer holds.
var ErrConflict = errors.New("provider state conflict")

// ErrEffectUncertain means an effect was claimed before a previous process
// stopped, but its external outcome was never durably recorded. The SDK never
// guesses or repeats such an effect automatically.
var ErrEffectUncertain = errors.New("effect outcome is uncertain")

// InboxState is the durable state of one run attempt at the provider.
type InboxState string

const (
	InboxAccepted        InboxState = "accepted"
	InboxRunning         InboxState = "running"
	InboxCancelRequested InboxState = "cancel_requested"
	InboxSucceeded       InboxState = "succeeded"
	InboxFailed          InboxState = "failed"
	InboxCancelled       InboxState = "cancelled"
	InboxTimedOut        InboxState = "timed_out"
)

// Terminal reports whether no further provider execution is permitted.
func (state InboxState) Terminal() bool {
	switch state {
	case InboxSucceeded, InboxFailed, InboxCancelled, InboxTimedOut:
		return true
	default:
		return false
	}
}

// InboxRecord is the durable, secret-free provider view of one authorized
// attempt. Token bytes are deliberately absent and must never be persisted.
type InboxRecord struct {
	RunID, AttemptID                       string
	RequestDigest, AuthorizationDigest     string
	RequestJSON, ResultJSON                json.RawMessage
	AgentID, AgentVersion, SkillID         string
	DeploymentID, InstanceID               string
	Generation, FencingToken, StateVersion uint64
	Traceparent, Tracestate                string
	State                                  InboxState
	DeadlineAt, CreatedAt, UpdatedAt       time.Time
	CancelRequestedAt                      *time.Time
}

// EffectState is the durable state machine for an external side effect.
type EffectState string

const (
	EffectStarted   EffectState = "started"
	EffectCompleted EffectState = "completed"
)

// EffectRecord is keyed by stable effect_id across attempts. Result contains
// only the caller-selected, non-secret replay value.
type EffectRecord struct {
	EffectID, RunID, AttemptID, RequestDigest string
	State                                     EffectState
	Result                                    json.RawMessage
	StartedAt, UpdatedAt                      time.Time
}

// OutboxRecord stores an event before delivery. Sequence is monotonically
// allocated per attempt by the durable adapter.
type OutboxRecord struct {
	RunID, AttemptID, EventID, EventType string
	Sequence                             uint64
	Envelope                             json.RawMessage
	CreatedAt                            time.Time
	DeliveredAt                          *time.Time
}

// DurableStore is the driver-free provider durability contract. Implementers
// must provide real commit/rollback isolation; an in-memory fallback is not a
// conforming production implementation.
type DurableStore interface {
	Within(context.Context, func(context.Context, Transaction) error) error
	GetInbox(context.Context, string, string) (InboxRecord, error)
	ListRecoverable(context.Context, time.Time, int) ([]InboxRecord, error)
	ListOutbox(context.Context, string, string, uint64, int) ([]OutboxRecord, error)
	Ready(context.Context) error
}

// Transaction contains all provider mutations that must commit atomically.
type Transaction interface {
	GetInbox(context.Context, string, string) (InboxRecord, error)
	CreateInbox(context.Context, InboxRecord) error
	UpdateInbox(context.Context, InboxRecord, uint64) error
	GetEffect(context.Context, string) (EffectRecord, error)
	CreateEffect(context.Context, EffectRecord) error
	CompleteEffect(context.Context, string, string, json.RawMessage, time.Time) error
	AppendOutbox(context.Context, OutboxRecord) (uint64, error)
	MarkOutboxDelivered(context.Context, string, string, uint64, time.Time) error
}
