package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type Service struct {
	deps      Dependencies
	healthMu  sync.Mutex
	unhealthy bool
}

func New(dependencies Dependencies) (*Service, error) {
	if err := dependencies.Validate(); err != nil {
		return nil, err
	}
	if !utc(dependencies.Clock.Now()) {
		return nil, errors.New("run clock must return UTC")
	}
	return &Service{deps: dependencies}, nil
}

func (service *Service) Create(ctx context.Context, request CreateRequest) (Run, error) {
	now := service.now()
	if err := request.Validate(now); err != nil {
		return Run{}, service.reject(ctx, request.Metadata, OperationCreate, now, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	snapshot, err := service.deps.Authorizer.Authorize(ctx, request.Caller, OperationCreate, request.Agent)
	if err != nil {
		return Run{}, service.reject(ctx, request.Metadata, OperationCreate, now, statusFor(err), normalizeAuthorization(err))
	}
	canonical, snapshotDigest, snapshotJSON, err := snapshot.Canonical()
	if err != nil || canonical.TenantID != request.Caller.TenantID || canonical.PrincipalID != request.Caller.PrincipalID {
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	labels, err := canonicalLabels(request.Labels)
	if err != nil {
		return Run{}, service.reject(ctx, request.Metadata, OperationCreate, now, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	request.Labels = labels
	requestDigest, err := createRequestDigest(request, canonical)
	if err != nil {
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	keyDigest := digestText(request.IdempotencyKey)
	if existing, stored, lookupErr := service.deps.Repository.GetByIdempotency(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
		if stored != requestDigest {
			return Run{}, service.reject(ctx, request.Metadata, OperationCreate, now, 409, NewError(CategoryConflict, ReasonIdempotencyConflict))
		}
		if err := service.audit(ctx, request.Metadata, OperationCreate, now, 201); err != nil {
			return Run{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		service.healthy()
		return existing, nil
	} else if !isNotFound(lookupErr) {
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	runID, err := service.deps.IDs.NewRunID(ctx)
	if err != nil || !prefixedUUID("run_", runID) {
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	outboxID, err := service.deps.IDs.NewOutboxID(ctx)
	if err != nil || !prefixedUUID("out_", outboxID) {
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	run := Run{TenantID: request.Caller.TenantID, RunID: runID, Agent: request.Agent, Input: append(json.RawMessage(nil), request.Input...), Labels: append(json.RawMessage(nil), labels...), ConversationRef: request.ConversationRef, Effects: request.Effects, State: StateQueued, StateVersion: 1, AuthorizationSnapshot: snapshotJSON, AuthorizationSnapshotDigest: snapshotDigest, Traceparent: request.Metadata.Traceparent(), Tracestate: request.Tracestate, DeadlineAt: request.DeadlineAt, CreatedAt: now, UpdatedAt: now}
	payload, _ := json.Marshal(map[string]any{"run_id": runID, "state": StateQueued, "state_version": 1})
	outbox := Outbox{OutboxID: outboxID, TenantID: run.TenantID, RunID: runID, Kind: "run-queued", StateVersion: 1, Payload: payload, CreatedAt: now}
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		if err := service.deps.Repository.Create(txctx, run, keyDigest, requestDigest, outbox); err != nil {
			return err
		}
		return service.observe(txctx, request.Metadata, OperationCreate, now, 201)
	})
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			existing, stored, lookupErr := service.deps.Repository.GetByIdempotency(ctx, request.Caller.TenantID, keyDigest)
			if lookupErr == nil && stored == requestDigest {
				if auditErr := service.audit(ctx, request.Metadata, OperationCreate, now, 201); auditErr != nil {
					return Run{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
				}
				service.healthy()
				return existing, nil
			}
			if lookupErr == nil {
				return Run{}, service.reject(ctx, request.Metadata, OperationCreate, now, 409, NewError(CategoryConflict, ReasonIdempotencyConflict))
			}
		}
		return Run{}, service.fail(ctx, request.Metadata, OperationCreate, now)
	}
	service.healthy()
	return run, nil
}

func (service *Service) Get(ctx context.Context, caller Caller, runID string, metadata platform.RequestMetadata) (Run, error) {
	started := service.now()
	if caller.Validate() != nil || !prefixedUUID("run_", runID) {
		return Run{}, service.reject(ctx, metadata, OperationRead, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	record, err := service.deps.Repository.Get(ctx, caller.TenantID, runID)
	if err != nil {
		if isNotFound(err) {
			return Run{}, service.reject(ctx, metadata, OperationRead, started, 404, err)
		}
		return Run{}, service.fail(ctx, metadata, OperationRead, started)
	}
	if _, err := service.deps.Authorizer.Authorize(ctx, caller, OperationRead, record.Agent); err != nil {
		return Run{}, service.reject(ctx, metadata, OperationRead, started, statusFor(err), normalizeAuthorization(err))
	}
	if err := service.audit(ctx, metadata, OperationRead, started, 200); err != nil {
		return Run{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	service.healthy()
	return record, nil
}

func (service *Service) Cancel(ctx context.Context, request CommandRequest) (Run, error) {
	started := service.now()
	if request.Caller.Validate() != nil || !prefixedUUID("run_", request.RunID) || !validIdempotencyKey(request.IdempotencyKey) || request.Command.Validate() != nil {
		return Run{}, service.reject(ctx, request.Metadata, OperationCommand, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	current, err := service.deps.Repository.Get(ctx, request.Caller.TenantID, request.RunID)
	if err != nil {
		if isNotFound(err) {
			return Run{}, service.reject(ctx, request.Metadata, OperationCommand, started, 404, err)
		}
		return Run{}, service.fail(ctx, request.Metadata, OperationCommand, started)
	}
	if _, err = service.deps.Authorizer.Authorize(ctx, request.Caller, OperationCommand, current.Agent); err != nil {
		return Run{}, service.reject(ctx, request.Metadata, OperationCommand, started, statusFor(err), normalizeAuthorization(err))
	}
	payload, _ := json.Marshal(map[string]any{"run_id": request.RunID, "command_id": request.Command.CommandID, "type": request.Command.Type})
	outboxFactory := func(factoryCtx context.Context, stateVersion uint64) (Outbox, error) {
		outboxID, idErr := service.deps.IDs.NewOutboxID(factoryCtx)
		if idErr != nil || !prefixedUUID("out_", outboxID) {
			return Outbox{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		return Outbox{OutboxID: outboxID, TenantID: request.Caller.TenantID, RunID: request.RunID, Kind: "run-command", StateVersion: stateVersion, Payload: payload, CreatedAt: started}, nil
	}
	commandBytes, _ := json.Marshal(request.Command)
	commandDigest := digestBytes(commandBytes)
	keyDigest := digestText(request.IdempotencyKey)
	var result Run
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var inner error
		result, inner = service.deps.Repository.Cancel(txctx, request.Caller.TenantID, request.RunID, request.Command, keyDigest, commandDigest, started, outboxFactory)
		if inner != nil {
			return inner
		}
		return service.observe(txctx, request.Metadata, OperationCommand, started, 200)
	})
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			return Run{}, service.reject(ctx, request.Metadata, OperationCommand, started, 409, err)
		}
		return Run{}, service.fail(ctx, request.Metadata, OperationCommand, started)
	}
	service.healthy()
	return result, nil
}

func (service *Service) Expire(ctx context.Context, caller Caller, runID string, expectedVersion uint64, metadata platform.RequestMetadata) (Run, error) {
	now := service.now()
	if caller.Validate() != nil || !prefixedUUID("run_", runID) || expectedVersion == 0 || expectedVersion > MaxSafeInteger {
		return Run{}, service.reject(ctx, metadata, OperationExpire, now, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	current, err := service.deps.Repository.Get(ctx, caller.TenantID, runID)
	if err != nil {
		if isNotFound(err) {
			return Run{}, service.reject(ctx, metadata, OperationExpire, now, 404, err)
		}
		return Run{}, service.fail(ctx, metadata, OperationExpire, now)
	}
	if _, err = service.deps.Authorizer.Authorize(ctx, caller, OperationExpire, current.Agent); err != nil {
		return Run{}, service.reject(ctx, metadata, OperationExpire, now, statusFor(err), normalizeAuthorization(err))
	}
	if now.Before(current.DeadlineAt) {
		return Run{}, service.reject(ctx, metadata, OperationExpire, now, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	if current.State.Terminal() {
		if err := service.audit(ctx, metadata, OperationExpire, now, 200); err != nil {
			return Run{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		service.healthy()
		return current, nil
	}
	outbox, err := service.timedOutOutbox(ctx, caller.TenantID, runID, expectedVersion, now)
	if err != nil {
		return Run{}, service.fail(ctx, metadata, OperationExpire, now)
	}
	var result Run
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var inner error
		result, inner = service.deps.Repository.Expire(txctx, caller.TenantID, runID, expectedVersion, now, outbox)
		if inner != nil {
			return inner
		}
		return service.observe(txctx, metadata, OperationExpire, now, 200)
	})
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			return Run{}, service.reject(ctx, metadata, OperationExpire, now, 409, err)
		}
		return Run{}, service.fail(ctx, metadata, OperationExpire, now)
	}
	service.healthy()
	return result, nil
}

// ExpireOverdue is the control plane's deadline sweeper: it moves up to limit
// non-terminal runs whose deadline has passed to timed_out. It acts as the
// system itself, so no caller authorization applies. Runs that changed state
// concurrently are skipped and picked up again by the next sweep.
func (service *Service) ExpireOverdue(ctx context.Context, limit int) (int, error) {
	now := service.now()
	overdue, err := service.deps.Repository.ListOverdue(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, item := range overdue {
		outbox, err := service.timedOutOutbox(ctx, item.TenantID, item.RunID, item.StateVersion, now)
		if err != nil {
			return expired, err
		}
		var result Run
		err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
			var inner error
			result, inner = service.deps.Repository.Expire(txctx, item.TenantID, item.RunID, item.StateVersion, now, outbox)
			return inner
		})
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			continue
		}
		if err != nil {
			return expired, err
		}
		if result.State == StateTimedOut && result.StateVersion == item.StateVersion+1 {
			expired++
		}
	}
	return expired, nil
}

func (service *Service) timedOutOutbox(ctx context.Context, tenantID, runID string, expectedVersion uint64, now time.Time) (Outbox, error) {
	outboxID, err := service.deps.IDs.NewOutboxID(ctx)
	if err != nil {
		return Outbox{}, err
	}
	payload, _ := json.Marshal(map[string]any{"run_id": runID, "state": StateTimedOut})
	return Outbox{OutboxID: outboxID, TenantID: tenantID, RunID: runID, Kind: "run-timed-out", StateVersion: expectedVersion + 1, Payload: payload, CreatedAt: now}, nil
}

func (service *Service) ReserveEffect(ctx context.Context, caller Caller, runID, effectID, semanticDigest string, metadata platform.RequestMetadata) (bool, error) {
	started := service.now()
	if caller.Validate() != nil || !prefixedUUID("run_", runID) || !effectPattern.MatchString(effectID) || !digestPattern.MatchString(semanticDigest) {
		return false, service.reject(ctx, metadata, OperationEffect, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	record, err := service.deps.Repository.Get(ctx, caller.TenantID, runID)
	if err != nil {
		if isNotFound(err) {
			return false, service.reject(ctx, metadata, OperationEffect, started, 404, err)
		}
		return false, service.fail(ctx, metadata, OperationEffect, started)
	}
	if record.Effects.Level != EffectWrite && record.Effects.Level != EffectIrreversible || record.Effects.EffectID != effectID {
		return false, service.reject(ctx, metadata, OperationEffect, started, 409, NewError(CategoryConflict, ReasonEffectConflict))
	}
	if _, err = service.deps.Authorizer.Authorize(ctx, caller, OperationEffect, record.Agent); err != nil {
		return false, service.reject(ctx, metadata, OperationEffect, started, statusFor(err), normalizeAuthorization(err))
	}
	var replay bool
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var inner error
		replay, inner = service.deps.Repository.ReserveEffect(txctx, EffectReservation{TenantID: caller.TenantID, RunID: runID, EffectID: effectID, SemanticDigest: semanticDigest, CreatedAt: service.now()})
		if inner != nil {
			return inner
		}
		return service.observe(txctx, metadata, OperationEffect, started, 200)
	})
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			return false, service.reject(ctx, metadata, OperationEffect, started, 409, err)
		}
		return false, service.fail(ctx, metadata, OperationEffect, started)
	}
	service.healthy()
	return replay, nil
}

func createRequestDigest(request CreateRequest, snapshot AuthorizationSnapshot) (string, error) {
	value := struct {
		Agent           AgentBinding          `json:"agent"`
		Input           json.RawMessage       `json:"input"`
		Labels          json.RawMessage       `json:"labels"`
		ConversationRef string                `json:"conversation_ref,omitempty"`
		DeadlineAt      string                `json:"deadline_at"`
		Effects         EffectIntent          `json:"effects"`
		Authorization   AuthorizationSnapshot `json:"authorization"`
	}{request.Agent, request.Input, request.Labels, request.ConversationRef, request.DeadlineAt.Format(time.RFC3339Nano), request.Effects, snapshot}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}
func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int, failure error) error {
	if service.audit(ctx, metadata, operation, started, status) != nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return failure
}
func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time) error {
	service.unhealthyState()
	_ = service.audit(ctx, metadata, operation, started, 503)
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}
func (service *Service) audit(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	return service.deps.UoW.Within(ctx, func(txctx context.Context) error { return service.observe(txctx, metadata, operation, started, status) })
}
func (service *Service) observe(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	auditID, err := service.deps.IDs.NewOutboxID(ctx)
	if err != nil {
		return err
	}
	auditID = "aud_" + auditID[len("out_"):]
	ended := service.now()
	outcome, span := observability.OutcomeSucceeded, observability.SpanStatusOK
	if status >= 500 {
		outcome, span = observability.OutcomeFailed, observability.SpanStatusError
	} else if status >= 400 {
		outcome, span = observability.OutcomeRejected, observability.SpanStatusError
	}
	return service.deps.Observability.AppendObservation(ctx, observability.AuditEntry{ID: auditID, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: string(operation), Outcome: outcome, HTTPStatus: status}, observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: string(operation), StartedAt: started, EndedAt: ended, Status: span})
}
func normalizeAuthorization(err error) error {
	if typed, ok := AsError(err); ok && (typed.Category == CategoryAuthentication || typed.Category == CategoryAuthorization || typed.Category == CategoryDependency) {
		return err
	}
	return NewError(CategoryAuthorization, ReasonRunForbidden)
}
func statusFor(err error) int {
	if typed, ok := AsError(err); ok {
		if typed.Category == CategoryAuthentication {
			return 401
		}
		if typed.Category == CategoryAuthorization {
			return 403
		}
		if typed.Category == CategoryDependency {
			return 503
		}
	}
	return 403
}
func isNotFound(err error) bool {
	typed, ok := AsError(err)
	return ok && typed.Category == CategoryNotFound
}
func (service *Service) now() time.Time { return service.deps.Clock.Now().UTC() }
func (service *Service) Name() string   { return "run-lifecycle-service" }
func (service *Service) Check(context.Context) error {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	if service.unhealthy {
		return errors.New("run durable dependency unavailable")
	}
	return nil
}
func (service *Service) unhealthyState() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.unhealthy = true
}
func (service *Service) healthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.unhealthy = false
}

var _ platformports.ReadinessCheck = (*Service)(nil)
