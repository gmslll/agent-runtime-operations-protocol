package assets

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

// Service is the asset broker use case: short opaque grants, quarantine
// upload, ready-only download, and per-hop network checks.
type Service struct {
	deps                Dependencies
	healthMu            sync.Mutex
	auditUnhealthy      error
	dependencyUnhealthy error
}

func New(dependencies Dependencies) (*Service, error) {
	if err := dependencies.Validate(); err != nil {
		return nil, err
	}
	now := dependencies.Clock.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return nil, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return &Service{deps: dependencies}, nil
}

func (service *Service) IssueGrant(ctx context.Context, request IssueRequest) (IssuedGrant, error) {
	started := service.now()
	if err := request.Validate(); err != nil {
		return IssuedGrant{}, err
	}
	now := service.now()
	if request.NotBefore.Location() != time.UTC || request.ExpiresAt.Location() != time.UTC || !request.ExpiresAt.After(request.NotBefore) || request.ExpiresAt.Sub(request.NotBefore) > MaxGrantTTL {
		return IssuedGrant{}, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if !now.Before(request.ExpiresAt) {
		return IssuedGrant{}, NewError(CategoryAuthorization, ReasonGrantExpired)
	}
	if err := service.authorize(ctx, request.Binding()); err != nil {
		return IssuedGrant{}, err
	}
	if request.Operation == OperationDownload {
		asset, err := service.deps.Assets.Get(ctx, request.Caller.TenantID, request.AssetID)
		if err != nil {
			return IssuedGrant{}, notFoundOrDependency(err)
		}
		if asset.State != StateReady {
			return IssuedGrant{}, NewError(CategoryAuthorization, ReasonNotReady)
		}
		if !downloadMatches(asset.Binding, request.Binding()) {
			return IssuedGrant{}, NewError(CategoryValidation, ReasonBindingMismatch)
		}
	}
	keyDigest := digestIdempotency(request.IdempotencyKey)
	if existing, err := service.deps.Grants.FindByIdempotency(ctx, request.Caller.TenantID, keyDigest); err == nil {
		if !sameGrantRequest(existing, request) {
			return IssuedGrant{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
		}
		token, err := service.deps.Tokens.IssueToken(ctx, existing)
		if err != nil || token.Digest != existing.OpaqueDigest {
			return IssuedGrant{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		return IssuedGrant{Grant: existing, Token: token.Value, Replay: true}, nil
	} else if !isNotFound(err) {
		return IssuedGrant{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	grantID, err := newGrantID(now)
	if err != nil {
		return IssuedGrant{}, err
	}
	grant := Grant{
		GrantID:              grantID,
		Binding:              request.Binding(),
		NotBefore:            request.NotBefore.UTC(),
		ExpiresAt:            request.ExpiresAt.UTC(),
		MaxUses:              request.MaxUses,
		Audience:             AssetTokenAudience,
		TokenKeyID:           service.deps.Tokens.KeyID(),
		IdempotencyKeyDigest: keyDigest,
	}
	token, err := service.deps.Tokens.IssueToken(ctx, grant)
	if err != nil {
		return IssuedGrant{}, err
	}
	grant.OpaqueDigest = token.Digest
	if err := grant.Validate(); err != nil {
		return IssuedGrant{}, err
	}
	err = service.mutate(ctx, request.Metadata, string(request.Operation), started, 201, func(transactionContext context.Context) error {
		if request.Operation == OperationUpload {
			asset := Asset{Binding: request.Binding(), State: StateQuarantine, CreatedAt: now}
			if err := asset.Validate(); err != nil {
				return err
			}
			if err := service.deps.Assets.Put(transactionContext, asset); err != nil {
				return err
			}
		}
		return service.deps.Grants.Save(transactionContext, grant)
	})
	if err != nil {
		if existing, lookupErr := service.deps.Grants.FindByIdempotency(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
			if !sameGrantRequest(existing, request) {
				return IssuedGrant{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
			}
			replayToken, issueErr := service.deps.Tokens.IssueToken(ctx, existing)
			if issueErr != nil || replayToken.Digest != existing.OpaqueDigest {
				return IssuedGrant{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
			}
			return IssuedGrant{Grant: existing, Token: replayToken.Value, Replay: true}, nil
		}
		return IssuedGrant{}, err
	}
	return IssuedGrant{Grant: grant, Token: token.Value}, nil
}

// ReceiveUpload accepts bytes metadata only. The object stays in quarantine
// until PromoteReady confirms the same digest.
func (service *Service) ReceiveUpload(ctx context.Context, receipt UploadReceipt) (Asset, error) {
	started := service.now()
	if err := receipt.Caller.Validate(); err != nil {
		return Asset{}, err
	}
	if !idempotencyPattern.MatchString(receipt.IdempotencyKey) || receipt.GrantToken == "" {
		return Asset{}, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	grant, err := service.loadUsable(ctx, receipt.Caller, receipt.GrantToken, OperationUpload)
	if err != nil {
		return Asset{}, err
	}
	if grant.Binding.MediaType != receipt.MediaType || grant.Binding.SizeBytes != receipt.SizeBytes || grant.Binding.Digest != receipt.Digest {
		return Asset{}, NewError(CategoryValidation, ReasonBindingMismatch)
	}
	var asset Asset
	err = service.mutate(ctx, receipt.Metadata, string(OperationUpload), started, 200, func(transactionContext context.Context) error {
		consumed, consumeErr := service.deps.Grants.Consume(transactionContext, receipt.Caller.TenantID, grant.GrantID, service.now())
		if consumeErr != nil {
			return consumeErr
		}
		if !consumed.Binding.SameIdentity(grant.Binding) {
			return NewError(CategoryConflict, ReasonIdempotencyConflict)
		}
		asset, consumeErr = service.deps.Assets.Get(transactionContext, receipt.Caller.TenantID, grant.Binding.AssetID)
		if consumeErr != nil {
			return consumeErr
		}
		if !asset.Binding.SameIdentity(grant.Binding) || (asset.State != StateQuarantine && asset.State != StateReady) {
			return NewError(CategoryConflict, ReasonIdempotencyConflict)
		}
		return nil
	})
	return asset, err
}

// PromoteReady moves a quarantined upload to ready only when the digest matches.
func (service *Service) PromoteReady(ctx context.Context, request PromoteRequest) (Asset, error) {
	started := service.now()
	if err := request.Caller.Validate(); err != nil {
		return Asset{}, err
	}
	if !assetPattern.MatchString(request.AssetID) || validateDigest(request.Digest) != nil {
		return Asset{}, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	asset, err := service.deps.Assets.Get(ctx, request.Caller.TenantID, request.AssetID)
	if err != nil {
		return Asset{}, notFoundOrDependency(err)
	}
	if !sameCaller(request.Caller, asset.Binding) {
		return Asset{}, NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	if err := service.authorize(ctx, asset.Binding); err != nil {
		return Asset{}, err
	}
	if asset.Binding.Digest != request.Digest {
		return Asset{}, NewError(CategoryValidation, ReasonDigestMismatch)
	}
	if asset.State == StateReady {
		return asset, nil
	}
	if asset.State != StateQuarantine {
		return Asset{}, NewError(CategoryAuthorization, ReasonNotReady)
	}
	asset.State = StateReady
	asset.ReadyAt = service.now()
	if err := asset.Validate(); err != nil {
		return Asset{}, err
	}
	if err := service.mutate(ctx, request.Metadata, "asset.promote", started, 200, func(transactionContext context.Context) error {
		return service.deps.Assets.Put(transactionContext, asset)
	}); err != nil {
		return Asset{}, err
	}
	return asset, nil
}

func (service *Service) RevokeGrant(ctx context.Context, request RevokeRequest) error {
	started := service.now()
	if err := request.Caller.Validate(); err != nil {
		return err
	}
	if !grantPattern.MatchString(request.GrantID) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	grant, err := service.deps.Grants.GetGrant(ctx, request.Caller.TenantID, request.GrantID)
	if err != nil {
		return notFoundOrDependency(err)
	}
	if !sameCaller(request.Caller, grant.Binding) {
		return NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	if err := service.authorize(ctx, grant.Binding); err != nil {
		return err
	}
	return service.mutate(ctx, request.Metadata, "asset.revoke", started, 200, func(transactionContext context.Context) error {
		return service.deps.Grants.Revoke(transactionContext, request.Caller.TenantID, request.GrantID, service.now())
	})
}

// Connect consumes one grant use and revalidates DNS and IP for the initial
// URL and every redirect hop.
func (service *Service) Connect(ctx context.Context, request ConnectRequest, redirects []string) ([]ResolvedEndpoint, error) {
	started := service.now()
	if err := request.Caller.Validate(); err != nil {
		return nil, err
	}
	if request.GrantToken == "" || request.URL == "" {
		return nil, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	grant, err := service.loadUsable(ctx, request.Caller, request.GrantToken, "")
	if err != nil {
		return nil, err
	}
	if grant.Binding.Operation == OperationDownload {
		asset, err := service.deps.Assets.Get(ctx, request.Caller.TenantID, grant.Binding.AssetID)
		if err != nil {
			return nil, notFoundOrDependency(err)
		}
		if asset.State != StateReady || asset.Binding.Digest != grant.Binding.Digest {
			return nil, NewError(CategoryAuthorization, ReasonNotReady)
		}
	}
	chain, err := EvaluateRedirects(ctx, service.deps.Resolver, request.URL, redirects)
	if err != nil {
		return nil, err
	}
	if err := service.mutate(ctx, request.Metadata, "asset.connect", started, 200, func(transactionContext context.Context) error {
		_, consumeErr := service.deps.Grants.Consume(transactionContext, request.Caller.TenantID, grant.GrantID, service.now())
		return consumeErr
	}); err != nil {
		return nil, err
	}
	return chain, nil
}

func (service *Service) authorize(ctx context.Context, binding Binding) error {
	if service == nil || service.deps.Authorizer == nil {
		return NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	decision := service.deps.Authorizer.Authorize(ctx, RunGrantRequest{Binding: binding})
	if decision != nil {
		if typed, ok := AsError(decision); ok && typed.Category == CategoryDependency {
			return decision
		}
		return NewError(CategoryAuthorization, ReasonGrantForbidden, decision)
	}
	return nil
}

func (service *Service) consume(ctx context.Context, caller Caller, token string, operation Operation) (Grant, error) {
	grant, err := service.loadUsable(ctx, caller, token, operation)
	if err != nil {
		return Grant{}, err
	}
	consumed, err := service.deps.Grants.Consume(ctx, caller.TenantID, grant.GrantID, service.now())
	if err != nil {
		return Grant{}, err
	}
	return consumed, nil
}

func (service *Service) loadUsable(ctx context.Context, caller Caller, token string, operation Operation) (Grant, error) {
	if !opaqueTokenPattern.MatchString(token) {
		return Grant{}, NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	grant, err := service.deps.Grants.FindByOpaqueDigest(ctx, caller.TenantID, digestOpaque(token))
	if err != nil {
		return Grant{}, notFoundOrDependency(err)
	}
	if !sameCaller(caller, grant.Binding) {
		return Grant{}, NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	if operation != "" && grant.Binding.Operation != operation {
		return Grant{}, NewError(CategoryValidation, ReasonBindingMismatch)
	}
	if err := service.authorize(ctx, grant.Binding); err != nil {
		return Grant{}, err
	}
	if err := grant.UsableAt(service.now()); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

func sameGrantRequest(grant Grant, request IssueRequest) bool {
	return grant.Binding.SameIdentity(request.Binding()) &&
		grant.NotBefore.Equal(request.NotBefore.UTC()) &&
		grant.ExpiresAt.Equal(request.ExpiresAt.UTC()) &&
		grant.MaxUses == request.MaxUses
}

func (service *Service) now() time.Time {
	return service.deps.Clock.Now().UTC()
}

func (service *Service) mutate(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, status int, callback func(context.Context) error) error {
	if callback == nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if err := service.deps.Faults.Check(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return service.fail(ctx, metadata, operation, started, err)
	}
	err := service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		if err := service.deps.Faults.Check(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := callback(transactionContext); err != nil {
			return err
		}
		if err := service.appendObservation(transactionContext, metadata, operation, started, status); err != nil {
			return err
		}
		return service.deps.Faults.Check(transactionContext, platformports.CheckpointAfterUseCase)
	})
	if err == nil {
		service.markHealthy()
		return nil
	}
	if typed, ok := AsError(err); ok && typed.Category != CategoryDependency {
		if auditErr := service.audit(ctx, metadata, operation, started, statusForAssetError(typed)); auditErr != nil {
			return service.fail(ctx, metadata, operation, started, auditErr)
		}
		return typed
	}
	return service.fail(ctx, metadata, operation, started, err)
}

func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, _ error) error {
	service.markDependencyUnhealthy()
	if err := service.audit(ctx, metadata, operation, started, 503); err != nil {
		service.markAuditUnhealthy()
	}
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}

func (service *Service) audit(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, status int) error {
	err := service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		return service.appendObservation(transactionContext, metadata, operation, started, status)
	})
	if err != nil {
		service.markAuditUnhealthy()
	} else {
		service.markAuditHealthy()
	}
	return err
}

func (service *Service) appendObservation(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, status int) error {
	auditID, err := service.deps.IDs.NewID(ctx, platformports.IDAudit)
	if err != nil {
		return observationFailure{}
	}
	ended := service.now()
	outcome, spanStatus := observability.OutcomeSucceeded, observability.SpanStatusOK
	if status >= 500 {
		outcome, spanStatus = observability.OutcomeFailed, observability.SpanStatusError
	} else if status >= 400 {
		outcome, spanStatus = observability.OutcomeRejected, observability.SpanStatusError
	}
	if err := service.deps.Audit.AppendObservation(ctx,
		observability.AuditEntry{ID: auditID, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: operation, Outcome: outcome, HTTPStatus: status},
		observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: operation, StartedAt: started, EndedAt: ended, Status: spanStatus}); err != nil {
		return observationFailure{}
	}
	return nil
}

type observationFailure struct{}

func (observationFailure) Error() string { return "asset observation unavailable" }

func statusForAssetError(failure Error) int {
	switch failure.Category {
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
	default:
		return 400
	}
}

func (service *Service) Name() string { return "asset-broker-service" }

func (service *Service) Check(context.Context) error {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	return errors.Join(service.auditUnhealthy, service.dependencyUnhealthy)
}

func (service *Service) markAuditUnhealthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.auditUnhealthy = errors.New("asset audit unavailable")
}

func (service *Service) markAuditHealthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.auditUnhealthy = nil
}

func (service *Service) markDependencyUnhealthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.dependencyUnhealthy = errors.New("asset dependency unavailable")
}

func (service *Service) markHealthy() {
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	service.auditUnhealthy = nil
	service.dependencyUnhealthy = nil
}

func isNotFound(err error) bool {
	typed, ok := AsError(err)
	return ok && typed.Reason == ReasonNotFound
}

func notFoundOrDependency(err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return err
	}
	if typed, ok := AsError(err); ok {
		return typed
	}
	return NewError(CategoryDependency, ReasonDependencyUnavailable, err)
}

var _ platformports.ReadinessCheck = (*Service)(nil)
