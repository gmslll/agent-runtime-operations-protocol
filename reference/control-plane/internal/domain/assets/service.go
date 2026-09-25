package assets

import (
	"context"
	"time"
)

// Service is the asset broker use case: short opaque grants, quarantine
// upload, ready-only download, and per-hop network checks.
type Service struct {
	deps Dependencies
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
	if err := service.authorize(ctx, request.Caller, request.RunID, request.AssetID, request.Operation); err != nil {
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
		if !existing.Binding.SameIdentity(request.Binding()) || existing.MaxUses != request.MaxUses {
			return IssuedGrant{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
		}
		return IssuedGrant{Grant: existing, Replay: true}, nil
	} else if !isNotFound(err) {
		return IssuedGrant{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	token, err := newOpaqueToken()
	if err != nil {
		return IssuedGrant{}, err
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
		IdempotencyKeyDigest: keyDigest,
		OpaqueDigest:         token.Digest,
	}
	if err := grant.Validate(); err != nil {
		return IssuedGrant{}, err
	}
	if err := service.deps.Grants.Save(ctx, grant); err != nil {
		if existing, lookupErr := service.deps.Grants.FindByIdempotency(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
			if !existing.Binding.SameIdentity(request.Binding()) {
				return IssuedGrant{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
			}
			return IssuedGrant{Grant: existing, Replay: true}, nil
		}
		return IssuedGrant{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	return IssuedGrant{Grant: grant, Token: token.Value}, nil
}

// ReceiveUpload accepts bytes metadata only. The object stays in quarantine
// until PromoteReady confirms the same digest.
func (service *Service) ReceiveUpload(ctx context.Context, receipt UploadReceipt) (Asset, error) {
	if err := receipt.Caller.Validate(); err != nil {
		return Asset{}, err
	}
	if !idempotencyPattern.MatchString(receipt.IdempotencyKey) || receipt.GrantToken == "" {
		return Asset{}, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	grant, err := service.consume(ctx, receipt.Caller, receipt.GrantToken, OperationUpload)
	if err != nil {
		return Asset{}, err
	}
	if grant.Binding.MediaType != receipt.MediaType || grant.Binding.SizeBytes != receipt.SizeBytes || grant.Binding.Digest != receipt.Digest {
		return Asset{}, NewError(CategoryValidation, ReasonBindingMismatch)
	}
	now := service.now()
	asset := Asset{Binding: grant.Binding, State: StateQuarantine, CreatedAt: now}
	if err := asset.Validate(); err != nil {
		return Asset{}, err
	}
	if existing, err := service.deps.Assets.Get(ctx, receipt.Caller.TenantID, grant.Binding.AssetID); err == nil {
		if existing.Binding.SameIdentity(grant.Binding) && (existing.State == StateQuarantine || existing.State == StateReady) {
			return existing, nil
		}
		return Asset{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
	} else if !isNotFound(err) {
		return Asset{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	if err := service.deps.Assets.Put(ctx, asset); err != nil {
		return Asset{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	return asset, nil
}

// PromoteReady moves a quarantined upload to ready only when the digest matches.
func (service *Service) PromoteReady(ctx context.Context, request PromoteRequest) (Asset, error) {
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
	if err := service.authorize(ctx, request.Caller, asset.Binding.RunID, asset.Binding.AssetID, OperationUpload); err != nil {
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
	if err := service.deps.Assets.Put(ctx, asset); err != nil {
		return Asset{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	return asset, nil
}

func (service *Service) RevokeGrant(ctx context.Context, request RevokeRequest) error {
	if err := request.Caller.Validate(); err != nil {
		return err
	}
	if !grantPattern.MatchString(request.GrantID) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if err := service.deps.Grants.Revoke(ctx, request.Caller.TenantID, request.GrantID); err != nil {
		return notFoundOrDependency(err)
	}
	return nil
}

// Connect consumes one grant use and revalidates DNS and IP for the initial
// URL and every redirect hop.
func (service *Service) Connect(ctx context.Context, request ConnectRequest, redirects []string) ([]ResolvedEndpoint, error) {
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
	if _, err := service.deps.Grants.Consume(ctx, request.Caller.TenantID, grant.GrantID, service.now()); err != nil {
		return nil, err
	}
	return chain, nil
}

func (service *Service) authorize(ctx context.Context, caller Caller, runID, assetID string, operation Operation) error {
	if service == nil || service.deps.Authorizer == nil {
		return NewError(CategoryAuthorization, ReasonGrantForbidden)
	}
	decision := service.deps.Authorizer.Authorize(ctx, RunGrantRequest{
		Caller:    caller,
		RunID:     runID,
		AssetID:   assetID,
		Operation: operation,
	})
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
	if len(token) != opaqueTokenBytes*2 {
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
	if err := service.authorize(ctx, caller, grant.Binding.RunID, grant.Binding.AssetID, grant.Binding.Operation); err != nil {
		return Grant{}, err
	}
	if err := grant.UsableAt(service.now()); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

func (service *Service) now() time.Time {
	return service.deps.Clock.Now().UTC()
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
