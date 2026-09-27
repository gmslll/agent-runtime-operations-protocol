package worker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type CryptoTokenSource struct{}

func (CryptoTokenSource) NewLeaseToken(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("generate lease token")
	}
	allZero := true
	for _, item := range value {
		allZero = allZero && item == 0
	}
	if allZero {
		return "", errors.New("generated all-zero lease token")
	}
	return "wlt_" + base64.RawURLEncoding.EncodeToString(value), nil
}

type Service struct {
	deps                 Dependencies
	healthMu             sync.Mutex
	dependencyUnhealthy  bool
	observationUnhealthy bool
}

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Validate() != nil || !utc(dependencies.Clock.Now()) {
		return nil, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return &Service{deps: dependencies}, nil
}

func (service *Service) Claim(ctx context.Context, request ClaimRequest) (Claim, bool, error) {
	started := service.now()
	if request.LeaseSeconds == 0 {
		request.LeaseSeconds = 60
	}
	if request.Validate() != nil {
		return Claim{}, false, service.reject(ctx, request.Metadata, OperationClaim, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	if err := service.deps.Authorizer.Authorize(ctx, request.Caller, OperationClaim, request.WorkerID); err != nil {
		return Claim{}, false, service.reject(ctx, request.Metadata, OperationClaim, started, statusFor(err), normalizeAuthorization(err))
	}
	deadline := started.Add(time.Duration(request.WaitSeconds) * time.Second)
	for {
		claim, err := service.claimOnce(ctx, request, started)
		if err == nil {
			service.healthyState()
			return claim, true, nil
		}
		if typed, ok := AsError(err); !ok || typed.Reason != ReasonNoWork {
			return Claim{}, false, service.reject(ctx, request.Metadata, OperationClaim, started, statusFor(err), err)
		}
		now := service.now()
		if request.WaitSeconds == 0 || !now.Before(deadline) {
			if auditErr := service.audit(ctx, request.Metadata, OperationClaim, started, 204); auditErr != nil {
				service.markObservationUnhealthy()
				return Claim{}, false, NewError(CategoryDependency, ReasonDependencyUnavailable)
			}
			service.healthyState()
			return Claim{}, false, nil
		}
		remaining := deadline.Sub(now)
		pause := 100 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Claim{}, false, service.reject(context.Background(), request.Metadata, OperationClaim, started, 408, NewError(CategoryTimeout, ReasonNoWork))
		case <-timer.C:
		}
	}
}

func (service *Service) claimOnce(ctx context.Context, request ClaimRequest, started time.Time) (Claim, error) {
	claimID, err := service.deps.IDs.NewClaimID(ctx)
	if err != nil || !prefixedUUID("clm_", claimID) {
		return Claim{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	attemptID, err := service.deps.IDs.NewAttemptID(ctx)
	if err != nil || !prefixedUUID("att_", attemptID) {
		return Claim{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	tokenID, err := service.deps.IDs.NewTokenID(ctx)
	if err != nil || !prefixedUUID("tok_", tokenID) {
		return Claim{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	leaseToken, err := service.deps.Tokens.NewLeaseToken(ctx)
	if err != nil || !tokenPattern.MatchString(leaseToken) {
		return Claim{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	command := ClaimCommand{Request: request, ClaimID: claimID, LeaseToken: leaseToken, LeaseTokenDigest: TokenDigest(leaseToken), ReplacementAttemptID: attemptID, TokenID: tokenID, Now: service.now()}
	command.LeaseExpiresAt = command.Now.Add(time.Duration(request.LeaseSeconds) * time.Second)
	var claim Claim
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var claimErr error
		claim, claimErr = service.deps.Repository.Claim(txctx, command)
		if claimErr != nil {
			return claimErr
		}
		return service.observe(txctx, request.Metadata, OperationClaim, started, 200)
	})
	if err != nil {
		return Claim{}, err
	}
	if claim.LeaseToken != leaseToken || claim.LeaseTokenDigest != command.LeaseTokenDigest || claim.Validate() != nil {
		return Claim{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return claim, nil
}

func (service *Service) Renew(ctx context.Context, request RenewRequest) (Claim, error) {
	started := service.now()
	if request.Validate() != nil {
		return Claim{}, service.reject(ctx, request.Metadata, OperationRenew, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	if err := service.deps.Authorizer.Authorize(ctx, request.Caller, OperationRenew, request.WorkerID); err != nil {
		return Claim{}, service.reject(ctx, request.Metadata, OperationRenew, started, statusFor(err), normalizeAuthorization(err))
	}
	command := RenewCommand{Request: request, LeaseTokenDigest: TokenDigest(request.LeaseToken), Now: started, ExpiresAt: started.Add(time.Duration(request.LeaseSeconds) * time.Second)}
	var claim Claim
	err := service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var renewErr error
		claim, renewErr = service.deps.Repository.Renew(txctx, command)
		if renewErr != nil {
			return renewErr
		}
		return service.observe(txctx, request.Metadata, OperationRenew, started, 200)
	})
	if err != nil {
		return Claim{}, service.reject(ctx, request.Metadata, OperationRenew, started, statusFor(err), err)
	}
	claim.LeaseToken = request.LeaseToken
	if claim.Validate() != nil {
		return Claim{}, service.fail(ctx, request.Metadata, OperationRenew, started)
	}
	service.healthyState()
	return claim, nil
}

func (service *Service) Complete(ctx context.Context, request CompleteRequest) (bool, error) {
	started := service.now()
	if request.Validate() != nil {
		return false, service.reject(ctx, request.Metadata, OperationComplete, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	if err := service.deps.Authorizer.Authorize(ctx, request.Caller, OperationComplete, request.WorkerID); err != nil {
		return false, service.reject(ctx, request.Metadata, OperationComplete, started, statusFor(err), normalizeAuthorization(err))
	}
	eventID, err := service.deps.IDs.NewEventID(ctx)
	if err != nil || !prefixedUUID("evt_", eventID) {
		return false, service.fail(ctx, request.Metadata, OperationComplete, started)
	}
	outboxID, err := service.deps.IDs.NewOutboxID(ctx)
	if err != nil || !prefixedUUID("out_", outboxID) {
		return false, service.fail(ctx, request.Metadata, OperationComplete, started)
	}
	digest, err := RequestDigest(struct {
		WorkerID, ClaimID, CompletionID, AttemptID string
		FencingToken                               uint64
		Result                                     jsonRaw
		EffectIDs                                  []string
		CompletedAt                                string
	}{request.WorkerID, request.ClaimID, request.CompletionID, request.AttemptID, request.FencingToken, jsonRaw(request.Result), slices.Clone(request.EffectIDs), request.CompletedAt.Format(time.RFC3339Nano)})
	if err != nil {
		return false, service.fail(ctx, request.Metadata, OperationComplete, started)
	}
	command := CompleteCommand{Request: request, LeaseTokenDigest: TokenDigest(request.LeaseToken), KeyDigest: KeyDigest(request.IdempotencyKey), RequestDigest: digest, EventID: eventID, OutboxID: outboxID, Now: started}
	var replay bool
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var completeErr error
		replay, completeErr = service.deps.Repository.Complete(txctx, command)
		if completeErr != nil {
			return completeErr
		}
		return service.observe(txctx, request.Metadata, OperationComplete, started, 204)
	})
	if err != nil {
		return false, service.reject(ctx, request.Metadata, OperationComplete, started, statusFor(err), err)
	}
	service.healthyState()
	return replay, nil
}

type jsonRaw []byte

func (value jsonRaw) MarshalJSON() ([]byte, error) { return append([]byte(nil), value...), nil }

func (service *Service) Release(ctx context.Context, request ReleaseRequest) error {
	started := service.now()
	if request.Validate() != nil {
		return service.reject(ctx, request.Metadata, OperationRelease, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	if err := service.deps.Authorizer.Authorize(ctx, request.Caller, OperationRelease, request.WorkerID); err != nil {
		return service.reject(ctx, request.Metadata, OperationRelease, started, statusFor(err), normalizeAuthorization(err))
	}
	attemptID, err := service.deps.IDs.NewAttemptID(ctx)
	if err != nil || !prefixedUUID("att_", attemptID) {
		return service.fail(ctx, request.Metadata, OperationRelease, started)
	}
	tokenID, err := service.deps.IDs.NewTokenID(ctx)
	if err != nil || !prefixedUUID("tok_", tokenID) {
		return service.fail(ctx, request.Metadata, OperationRelease, started)
	}
	command := ReleaseCommand{Request: request, LeaseTokenDigest: TokenDigest(request.LeaseToken), ReplacementAttemptID: attemptID, TokenID: tokenID, Now: started}
	if err := service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		if releaseErr := service.deps.Repository.Release(txctx, command); releaseErr != nil {
			return releaseErr
		}
		return service.observe(txctx, request.Metadata, OperationRelease, started, 204)
	}); err != nil {
		return service.reject(ctx, request.Metadata, OperationRelease, started, statusFor(err), err)
	}
	service.healthyState()
	return nil
}

func (service *Service) Name() string { return "worker-service" }

func (service *Service) Check(ctx context.Context) error {
	if service.deps.Repository.Check(ctx) != nil {
		service.markDependencyUnhealthy()
		return errors.New("worker service unhealthy")
	}
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	if service.dependencyUnhealthy || service.observationUnhealthy {
		return errors.New("worker service unhealthy")
	}
	return nil
}

func (service *Service) now() time.Time { return service.deps.Clock.Now().UTC() }

func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int, failure error) error {
	if typed, ok := AsError(failure); ok && typed.Category == CategoryDependency {
		service.markDependencyUnhealthy()
	}
	if auditErr := service.audit(ctx, metadata, operation, started, status); auditErr != nil {
		service.markObservationUnhealthy()
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return failure
}

func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time) error {
	service.markDependencyUnhealthy()
	return service.reject(ctx, metadata, operation, started, 503, NewError(CategoryDependency, ReasonDependencyUnavailable))
}

func (service *Service) audit(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	return service.deps.UoW.Within(ctx, func(txctx context.Context) error { return service.observe(txctx, metadata, operation, started, status) })
}

func (service *Service) observe(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	id, err := service.deps.IDs.NewAuditID(ctx)
	if err != nil || !prefixedUUID("aud_", id) {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	ended := service.now()
	outcome, span := observability.OutcomeFailed, observability.SpanStatusError
	if status < 400 {
		outcome, span = observability.OutcomeSucceeded, observability.SpanStatusOK
	} else if status < 500 {
		outcome = observability.OutcomeRejected
	}
	return service.deps.Observability.AppendObservation(ctx,
		observability.AuditEntry{ID: id, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: string(operation), Outcome: outcome, HTTPStatus: status},
		observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: string(operation), StartedAt: started, EndedAt: ended, Status: span},
	)
}

func normalizeAuthorization(err error) error {
	if typed, ok := AsError(err); ok && (typed.Category == CategoryAuthentication || typed.Category == CategoryAuthorization) {
		return err
	}
	return NewError(CategoryAuthorization, ReasonAuthorization)
}

func statusFor(err error) int {
	if typed, ok := AsError(err); ok {
		switch typed.Category {
		case CategoryValidation:
			return 400
		case CategoryAuthentication:
			return 401
		case CategoryAuthorization:
			return 403
		case CategoryNotFound:
			return 404
		case CategoryConflict:
			return 409
		case CategoryCapacity:
			return 429
		case CategoryTimeout:
			return 408
		}
	}
	return 503
}

func (service *Service) healthyState() {
	service.healthMu.Lock()
	service.dependencyUnhealthy = false
	service.observationUnhealthy = false
	service.healthMu.Unlock()
}
func (service *Service) markDependencyUnhealthy() {
	service.healthMu.Lock()
	service.dependencyUnhealthy = true
	service.healthMu.Unlock()
}
func (service *Service) markObservationUnhealthy() {
	service.healthMu.Lock()
	service.observationUnhealthy = true
	service.healthMu.Unlock()
}
