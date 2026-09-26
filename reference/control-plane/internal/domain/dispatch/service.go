package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type Service struct {
	deps                 Dependencies
	healthMu             sync.Mutex
	dependencyUnhealthy  bool
	observationUnhealthy bool
}

func New(dependencies Dependencies) (*Service, error) {
	if err := dependencies.Validate(); err != nil || !utc(dependencies.Clock.Now()) {
		return nil, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	dependencies.Issuer = strings.TrimSuffix(dependencies.Issuer, "/")
	return &Service{deps: dependencies}, nil
}

func (service *Service) Dispatch(ctx context.Context, request DispatchRequest) (Ticket, Attempt, error) {
	started := service.now()
	if request.Validate() != nil {
		return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	view, err := service.deps.Runs.DispatchableRun(ctx, request.Caller.TenantID, request.RunID)
	if err != nil {
		if typed, ok := AsError(err); ok && typed.Category == CategoryNotFound {
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 404, err)
		}
		return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
	}
	if view.Validate() != nil || view.TenantID != request.Caller.TenantID || view.RunID != request.RunID || !started.Before(view.DeadlineAt) {
		return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonRunNotDispatchable))
	}
	if err = service.deps.Authorizer.Authorize(ctx, request.Caller, OperationIssue, view.Agent); err != nil {
		return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, statusFor(err), normalizeAuthorization(err))
	}
	keyHash := sha256.Sum256([]byte(request.IdempotencyKey))
	keyDigest := hex.EncodeToString(keyHash[:])
	requestDigest := dispatchRequestDigest(request, view)
	if existing, stored, lookupErr := service.deps.Repository.GetByIdempotency(ctx, request.Caller.TenantID, keyDigest); lookupErr == nil {
		if stored != requestDigest || existing.RunID != view.RunID {
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonIdempotencyConflict))
		}
		switch existing.State {
		case StateIssued, StateAccepted:
		case StateExpired:
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonTicketExpired))
		case StateFenced:
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonAttemptFenced))
		default:
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonRunNotDispatchable))
		}
		if !started.Before(existing.TicketExpiresAt) {
			return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, NewError(CategoryConflict, ReasonTicketExpired))
		}
		ticket, issueErr := service.ticket(ctx, existing, view, request.Caller)
		if issueErr != nil {
			return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
		}
		if auditErr := service.audit(ctx, request.Metadata, started, 201); auditErr != nil {
			return Ticket{}, Attempt{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
		}
		service.healthyState()
		return ticket, existing, nil
	} else if !isNotFound(lookupErr) {
		return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
	}
	key, err := service.deps.Signer.ActiveKey(ctx, started)
	if err != nil || key.Validate(service.deps.MaxTokenTTL) != nil || key.Status != KeyActive {
		return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
	}
	keys, err := service.deps.Signer.VerificationKeys(ctx, started)
	if err != nil || validateSigningSet(keys, key.KeyID, started, service.deps.MaxTokenTTL) != nil {
		return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
	}
	candidates, err := service.deps.Candidates.Candidates(ctx, view)
	if err != nil {
		return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
	}
	candidates = eligibleCandidates(candidates, started)
	if len(candidates) == 0 {
		return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 429, NewError(CategoryCapacity, ReasonNoCapacity))
	}
	for _, candidate := range candidates {
		attemptID, deploymentID, tokenID, idErr := service.identifiers(ctx, candidate)
		if idErr != nil {
			return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
		}
		leaseExpiry := minimumTime(started.Add(service.deps.AttemptLease), candidate.LeaseExpiresAt, view.DeadlineAt)
		ticketExpiry := minimumTime(started.Add(service.deps.TicketTTL), leaseExpiry, view.DeadlineAt)
		command := ReserveCommand{Run: view, Candidate: candidate, AttemptID: attemptID, DeploymentID: deploymentID, TokenID: tokenID, SigningKey: key, SigningKeys: keys, KeyDigest: keyDigest, RequestDigest: requestDigest, Now: started, LeaseExpires: leaseExpiry, TicketExpires: ticketExpiry}
		var attempt Attempt
		var replay bool
		err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
			var reserveErr error
			attempt, replay, reserveErr = service.deps.Repository.Reserve(txctx, command)
			if reserveErr != nil {
				return reserveErr
			}
			return service.observe(txctx, request.Metadata, started, 201)
		})
		if err != nil {
			if typed, ok := AsError(err); ok && typed.Category == CategoryCapacity {
				continue
			}
			if typed, ok := AsError(err); ok && typed.Category == CategoryConflict {
				return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 409, err)
			}
			return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
		}
		if attempt.Validate() != nil || attempt.TenantID != view.TenantID || attempt.RunID != view.RunID || !replay && attempt.SigningKeyID != key.KeyID {
			return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
		}
		ticket, issueErr := service.ticket(ctx, attempt, view, request.Caller)
		if issueErr != nil {
			return Ticket{}, Attempt{}, service.fail(ctx, request.Metadata, started)
		}
		service.healthyState()
		return ticket, attempt, nil
	}
	return Ticket{}, Attempt{}, service.reject(ctx, request.Metadata, started, 429, NewError(CategoryCapacity, ReasonNoCapacity))
}

func (service *Service) JWKS(ctx context.Context) (JWKS, error) {
	now := service.now()
	publicKeys, err := service.deps.Signer.VerificationKeys(ctx, now)
	active, activeErr := service.deps.Signer.ActiveKey(ctx, now)
	if err != nil || activeErr != nil || validateSigningSet(publicKeys, active.KeyID, now, service.deps.MaxTokenTTL) != nil {
		service.markDependencyUnhealthy()
		return JWKS{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if err = service.deps.UoW.Within(ctx, func(txctx context.Context) error { return service.deps.Repository.SyncKeys(txctx, publicKeys, now) }); err != nil {
		service.markDependencyUnhealthy()
		return JWKS{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	keys, err := service.deps.Repository.Keys(ctx, now)
	if err != nil {
		service.markDependencyUnhealthy()
		return JWKS{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	keys, err = normalizeKeys(keys)
	if err != nil {
		service.markDependencyUnhealthy()
		return JWKS{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	cacheUntil := now.Add(time.Minute)
	for _, key := range keys {
		if key.VerifyUntil.Before(cacheUntil) {
			cacheUntil = key.VerifyUntil
		}
	}
	result := JWKS{Issuer: service.deps.Issuer, CacheUntil: cacheUntil, Keys: keys}
	if result.Validate(now, service.deps.MaxTokenTTL) != nil {
		service.markDependencyUnhealthy()
		return JWKS{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	service.markDependencyHealthy()
	return result, nil
}

func validateSigningSet(keys []KeyMetadata, activeID string, now time.Time, maxTTL time.Duration) error {
	normalized, err := normalizeKeys(keys)
	if err != nil || len(normalized) == 0 || len(normalized) > 32 {
		return errors.New("invalid signing key set")
	}
	active := 0
	for _, key := range normalized {
		if key.Validate(maxTTL) != nil || !key.VerifyUntil.After(now) {
			return errors.New("invalid signing key metadata")
		}
		if key.Status == KeyActive {
			active++
			if key.KeyID != activeID || now.Before(key.NotBefore) || !now.Before(key.SignUntil) {
				return errors.New("active signing key mismatch")
			}
		}
	}
	if active != 1 {
		return errors.New("signing key set must contain one active key")
	}
	return nil
}

func (service *Service) Name() string { return "dispatch-service" }

func (service *Service) Check(ctx context.Context) error {
	if _, err := service.JWKS(ctx); err != nil {
		return errors.New("dispatch service unhealthy")
	}
	service.healthMu.Lock()
	defer service.healthMu.Unlock()
	if service.dependencyUnhealthy || service.observationUnhealthy {
		return errors.New("dispatch service unhealthy")
	}
	return nil
}

func (service *Service) ticket(ctx context.Context, attempt Attempt, view RunView, caller Caller) (Ticket, error) {
	now := service.now()
	if !now.Before(attempt.TicketExpiresAt) {
		return Ticket{}, NewError(CategoryConflict, ReasonTicketExpired)
	}
	key, err := service.deps.Signer.ActiveKey(ctx, now)
	if err != nil {
		return Ticket{}, err
	}
	token, err := issueToken(ctx, service.deps.Signer, key, tokenClaims(attempt, view, caller, service.deps.Issuer, now))
	if err != nil {
		return Ticket{}, err
	}
	stream := strings.TrimSuffix(attempt.Endpoint, "/v1/runs") + "/v1/runs/" + attempt.RunID + "/events"
	return Ticket{RunID: attempt.RunID, AttemptID: attempt.AttemptID, FencingToken: attempt.FencingToken, Agent: view.Agent, Delivery: Delivery{Mode: attempt.TransportProfile, DeploymentID: attempt.DeploymentID, InstanceID: attempt.InstanceID, Generation: attempt.Generation, Audience: attempt.Audience, Endpoint: attempt.Endpoint, StreamEndpoint: stream, ExpiresAt: attempt.TicketExpiresAt}, RunToken: token, Traceparent: attempt.Traceparent, Tracestate: attempt.Tracestate}, nil
}

func (service *Service) identifiers(ctx context.Context, candidate Candidate) (string, string, string, error) {
	attemptID, err := service.deps.IDs.NewAttemptID(ctx)
	if err != nil || !prefixedUUID("att_", attemptID) {
		return "", "", "", errors.New("invalid attempt ID")
	}
	deploymentID := candidate.DeploymentID
	if deploymentID == "" {
		deploymentID, err = service.deps.IDs.NewDeploymentID(ctx)
	}
	if err != nil || !prefixedUUID("dep_", deploymentID) {
		return "", "", "", errors.New("invalid deployment ID")
	}
	tokenID, err := service.deps.IDs.NewTokenID(ctx)
	if err != nil || !prefixedUUID("tok_", tokenID) {
		return "", "", "", errors.New("invalid token ID")
	}
	return attemptID, deploymentID, tokenID, nil
}

func eligibleCandidates(values []Candidate, now time.Time) []Candidate {
	result := make([]Candidate, 0, len(values))
	for _, candidate := range values {
		if candidate.Validate(now) == nil {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		if result[i].AvailableSlots != result[j].AvailableSlots {
			return result[i].AvailableSlots > result[j].AvailableSlots
		}
		if result[i].Weight != result[j].Weight {
			return result[i].Weight > result[j].Weight
		}
		if result[i].ServiceID != result[j].ServiceID {
			return result[i].ServiceID < result[j].ServiceID
		}
		return result[i].InstanceID < result[j].InstanceID
	})
	return result
}

func dispatchRequestDigest(request DispatchRequest, view RunView) string {
	value := struct {
		TenantID, PrincipalID, CredentialID, RunID, AgentID, AgentVersion, SkillID, ManifestDigest, AuthorizationSnapshotHash string
	}{request.Caller.TenantID, request.Caller.PrincipalID, request.Caller.CredentialID, request.RunID, view.Agent.ID, view.Agent.Version, view.Agent.SkillID, view.Agent.ManifestDigest, view.AuthorizationSnapshotHash}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func minimumTime(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}

func (service *Service) now() time.Time { return service.deps.Clock.Now() }

func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int, failure error) error {
	if service.audit(ctx, metadata, started, status) != nil {
		service.markObservationUnhealthy()
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	service.markObservationHealthy()
	return failure
}

func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, started time.Time) error {
	service.markDependencyUnhealthy()
	if service.audit(ctx, metadata, started, 503) != nil {
		service.markObservationUnhealthy()
	} else {
		service.markObservationHealthy()
	}
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}

func (service *Service) audit(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int) error {
	return service.deps.UoW.Within(ctx, func(txctx context.Context) error { return service.observe(txctx, metadata, started, status) })
}

func (service *Service) observe(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int) error {
	id, err := service.deps.IDs.NewAuditID(ctx)
	if err != nil || !prefixedUUID("aud_", id) {
		return errors.New("invalid audit ID")
	}
	ended := service.now()
	outcome, span := observability.OutcomeSucceeded, observability.SpanStatusOK
	if status >= 500 {
		outcome, span = observability.OutcomeFailed, observability.SpanStatusError
	} else if status >= 400 {
		outcome, span = observability.OutcomeRejected, observability.SpanStatusError
	}
	return service.deps.Observability.AppendObservation(ctx, observability.AuditEntry{ID: id, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: string(OperationIssue), Outcome: outcome, HTTPStatus: status}, observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: string(OperationIssue), StartedAt: started, EndedAt: ended, Status: span})
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

func (service *Service) markDependencyHealthy() {
	service.healthMu.Lock()
	service.dependencyUnhealthy = false
	service.healthMu.Unlock()
}

func (service *Service) markObservationUnhealthy() {
	service.healthMu.Lock()
	service.observationUnhealthy = true
	service.healthMu.Unlock()
}

func (service *Service) markObservationHealthy() {
	service.healthMu.Lock()
	service.observationUnhealthy = false
	service.healthMu.Unlock()
}

func normalizeAuthorization(err error) error {
	if typed, ok := AsError(err); ok && (typed.Category == CategoryAuthentication || typed.Category == CategoryAuthorization || typed.Category == CategoryDependency) {
		return err
	}
	return NewError(CategoryAuthorization, ReasonDispatchForbidden)
}

func statusFor(err error) int {
	if typed, ok := AsError(err); ok {
		switch typed.Category {
		case CategoryAuthentication:
			return 401
		case CategoryAuthorization:
			return 403
		case CategoryNotFound:
			return 404
		case CategoryCapacity:
			return 429
		}
	}
	return 503
}

func isNotFound(err error) bool {
	typed, ok := AsError(err)
	return ok && typed.Category == CategoryNotFound
}
