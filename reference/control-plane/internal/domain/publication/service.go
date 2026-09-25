package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

// Service implements the P12 publication use case. It is intentionally
// transport-neutral; HTTP and CLI adapters consume this interface later.
type Service struct {
	deps           Dependencies
	mu             sync.Mutex
	healthMu       sync.Mutex
	auditUnhealthy error
}

func New(dependencies Dependencies) (*Service, error) {
	if err := dependencies.Validate(); err != nil {
		return nil, err
	}
	now := dependencies.Clock.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return nil, errors.New("publication clock must return non-zero UTC time")
	}
	return &Service{deps: dependencies}, nil
}

func (service *Service) Publish(ctx context.Context, request PublishRequest) (PublishResult, error) {
	started := service.now()
	request.Bundle = slices.Clone(request.Bundle)
	if err := request.Validate(); err != nil {
		status, failure := 400, NewError(CategoryValidation, ReasonInvalidRequest)
		if len(request.Bundle) > MaxBundleBytes {
			status, failure = 413, NewError(CategoryCapacity, ReasonBundleTooLarge)
		} else if typed, ok := AsError(err); ok && typed.Category == CategoryAuthentication {
			status, failure = 401, err
		}
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, status, failure)
	}
	if err := service.authorize(ctx, request.Caller, OperationPublish, request.AgentID); err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryDependency {
			return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, err)
		}
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, statusFor(err, 403), err)
	}
	if err := service.deps.Faults.Check(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, err)
	}
	bundle, err := service.deps.Validator.ValidateBundle(ctx, request.Bundle)
	clear(request.Bundle)
	request.Bundle = nil
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryDependency {
			return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, err)
		}
		status := statusFor(err, 400)
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, status, normalizeValidationFailure(err))
	}
	if bundle.AgentID != request.AgentID {
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 400, NewError(CategoryValidation, ReasonAgentIDMismatch))
	}
	if err := bundle.ValidateWithDigest(ctx, service.deps.ManifestDigester); err != nil {
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 400, NewError(CategoryValidation, ReasonBundleInvalid))
	}

	keyDigest := sha256Hex(request.IdempotencyKey)
	fingerprint := NewPublishFingerprint(request, bundle)
	requestDigest, err := service.deps.Fingerprinter.DigestRequest(ctx, fingerprint)
	if err != nil || !digestHexPattern.MatchString(requestDigest) {
		return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, errors.Join(err, errors.New("invalid request fingerprint digest")))
	}

	service.mu.Lock()
	defer service.mu.Unlock()
	if existing, lookupErr := service.deps.Repository.GetByIdempotencyDigest(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
		return service.replay(ctx, request.Metadata, started, existing, requestDigest)
	} else if !isNotFound(lookupErr) {
		return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, lookupErr)
	}
	if existing, lookupErr := service.deps.Repository.Get(ctx, request.Caller.TenantID, request.AgentID, bundle.Version); lookupErr == nil {
		if existing.BundleSemanticDigest == bundle.BundleSemanticDigest && existing.ManifestDigest == bundle.ManifestDigest {
			return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 409, NewError(CategoryConflict, ReasonImmutableConflict))
		}
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 409, NewError(CategoryConflict, ReasonImmutableConflict))
	} else if !isNotFound(lookupErr) {
		return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, lookupErr)
	}
	record, err := NewRecord(ctx, service.deps.ManifestDigester, request.Caller, bundle, keyDigest, requestDigest, service.now())
	if err != nil {
		return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 400, NewError(CategoryValidation, ReasonBundleInvalid))
	}
	err = service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		if err := service.deps.Repository.Create(transactionContext, &record); err != nil {
			return err
		}
		if err := service.deps.Faults.Check(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := service.appendObservation(transactionContext, request.Metadata, OperationPublish, started, 201); err != nil {
			return err
		}
		return service.deps.Faults.Check(transactionContext, platformports.CheckpointAfterUseCase)
	})
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
			if existing, lookupErr := service.deps.Repository.GetByIdempotencyDigest(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
				return service.replay(ctx, request.Metadata, started, existing, requestDigest)
			}
			return PublishResult{}, service.reject(ctx, request.Metadata, OperationPublish, started, 409, err)
		}
		return PublishResult{}, service.fail(ctx, request.Metadata, OperationPublish, started, err)
	}
	service.markAuditHealthy()
	return resultFor(record), nil
}

func (service *Service) Get(ctx context.Context, request GetRequest) (GetResult, error) {
	started := service.now()
	if err := request.Validate(); err != nil {
		status, failure := 400, NewError(CategoryValidation, ReasonInvalidRequest)
		if typed, ok := AsError(err); ok && typed.Category == CategoryAuthentication {
			status, failure = 401, err
		}
		return GetResult{}, service.rejectGet(ctx, request.Metadata, started, status, failure)
	}
	if err := service.authorize(ctx, request.Caller, OperationGet, request.AgentID); err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryDependency {
			return GetResult{}, service.failGet(ctx, request.Metadata, started, err)
		}
		return GetResult{}, service.rejectGet(ctx, request.Metadata, started, statusFor(err, 403), err)
	}
	record, err := service.deps.Repository.Get(ctx, request.Caller.TenantID, request.AgentID, request.Version)
	if err != nil {
		if isNotFound(err) {
			return GetResult{}, service.rejectGet(ctx, request.Metadata, started, 404, NewError(CategoryNotFound, ReasonNotFound))
		}
		return GetResult{}, service.failGet(ctx, request.Metadata, started, err)
	}
	if err := record.ValidateWithDigest(ctx, service.deps.ManifestDigester); err != nil {
		return GetResult{}, service.failGet(ctx, request.Metadata, started, err)
	}
	result := record.GetResult()
	if err := result.Validate(); err != nil {
		return GetResult{}, service.failGet(ctx, request.Metadata, started, err)
	}
	if err := service.audit(ctx, request.Metadata, OperationGet, started, 200); err != nil {
		return GetResult{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return result, nil
}

func (service *Service) replay(ctx context.Context, metadata platform.RequestMetadata, started time.Time, record Record, requestDigest string) (PublishResult, error) {
	if err := record.ValidateWithDigest(ctx, service.deps.ManifestDigester); err != nil {
		return PublishResult{}, service.fail(ctx, metadata, OperationPublish, started, err)
	}
	if record.IdempotencyRequestDigest != requestDigest {
		return PublishResult{}, service.reject(ctx, metadata, OperationPublish, started, 409, NewError(CategoryConflict, ReasonIdempotencyConflict))
	}
	if err := service.audit(ctx, metadata, OperationPublish, started, 200); err != nil {
		return PublishResult{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return resultFor(record), nil
}

func resultFor(record Record) PublishResult {
	return PublishResult{AgentID: record.AgentID, Version: record.Version, ManifestDigest: record.ManifestDigest, Location: "/v1/agent-definitions/" + record.AgentID + "/versions/" + record.Version, ETag: `"` + record.ManifestDigest + `"`}
}

func (service *Service) authorize(ctx context.Context, caller Caller, operation Operation, agentID string) error {
	if err := service.deps.Authorizer.Authorize(ctx, AuthorizationRequest{Caller: caller, Operation: operation, AgentID: agentID}); err != nil {
		if typed, ok := AsError(err); ok && (typed.Category == CategoryAuthentication || typed.Category == CategoryAuthorization || typed.Category == CategoryDependency) {
			return err
		}
		return NewError(CategoryAuthorization, ReasonPublicationForbidden)
	}
	return nil
}

func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int, failure error) error {
	if err := service.audit(ctx, metadata, operation, started, status); err != nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return failure
}
func (service *Service) rejectGet(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int, failure error) error {
	return service.reject(ctx, metadata, OperationGet, started, status, failure)
}

func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, cause error) error {
	if err := service.audit(ctx, metadata, operation, started, 503); err != nil {
		cause = errors.Join(cause, err)
	}
	service.markAuditUnhealthy(cause)
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}
func (service *Service) failGet(ctx context.Context, metadata platform.RequestMetadata, started time.Time, cause error) error {
	return service.fail(ctx, metadata, OperationGet, started, cause)
}

func (service *Service) audit(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	err := service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		return service.appendObservation(transactionContext, metadata, operation, started, status)
	})
	if err != nil {
		service.markAuditUnhealthy(err)
	} else {
		service.markAuditHealthy()
	}
	return err
}

func (service *Service) appendObservation(ctx context.Context, metadata platform.RequestMetadata, operation Operation, started time.Time, status int) error {
	auditID, err := service.deps.IDs.NewID(ctx, platformports.IDAudit)
	if err != nil {
		return err
	}
	ended := service.now()
	outcome, spanStatus := observability.OutcomeSucceeded, observability.SpanStatusOK
	if status >= 500 {
		outcome, spanStatus = observability.OutcomeFailed, observability.SpanStatusError
	} else if status >= 400 {
		outcome, spanStatus = observability.OutcomeRejected, observability.SpanStatusError
	}
	return service.deps.Observability.AppendObservation(ctx,
		observability.AuditEntry{ID: auditID, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: string(operation), Outcome: outcome, HTTPStatus: status},
		observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: string(operation), StartedAt: started, EndedAt: ended, Status: spanStatus})
}

func (service *Service) now() time.Time { return service.deps.Clock.Now().UTC() }
func (service *Service) Name() string   { return "publication-service" }
func (service *Service) Check(context.Context) error {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	return service.auditUnhealthy
}
func (service *Service) markAuditUnhealthy(cause error) {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.auditUnhealthy = fmt.Errorf("publication audit unavailable: %w", cause)
}
func (service *Service) markAuditHealthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.auditUnhealthy = nil
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func isNotFound(err error) bool {
	typed, ok := AsError(err)
	return ok && typed.Category == CategoryNotFound && typed.Reason == ReasonNotFound
}
func statusFor(err error, fallback int) int {
	if typed, ok := AsError(err); ok {
		switch typed.Category {
		case CategoryAuthentication:
			return 401
		case CategoryAuthorization:
			return 403
		case CategoryNotFound:
			return 404
		case CategoryConflict:
			return 409
		case CategoryCapacity:
			return 413
		case CategoryDependency:
			return 503
		}
	}
	return fallback
}
func normalizeValidationFailure(err error) error {
	if typed, ok := AsError(err); ok {
		return typed
	}
	return NewError(CategoryValidation, ReasonBundleInvalid)
}

var _ PublicationService = (*Service)(nil)
var _ platformports.ReadinessCheck = (*Service)(nil)
