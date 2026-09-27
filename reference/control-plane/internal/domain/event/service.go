package event

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type Service struct {
	deps      Dependencies
	mu        sync.Mutex
	unhealthy bool
}

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Validate() != nil || !utc(dependencies.Clock.Now()) {
		return nil, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	dependencies.ControlPlaneBaseURL = strings.TrimSuffix(dependencies.ControlPlaneBaseURL, "/")
	return &Service{deps: dependencies}, nil
}

func (service *Service) CreateSession(ctx context.Context, request SessionRequest) (Session, error) {
	started := service.deps.Clock.Now()
	if request.Validate() != nil {
		return Session{}, service.reject(ctx, request.Metadata, started, 400, NewError(CategoryValidation, ReasonInvalidRequest))
	}
	token, err := service.deps.Tokens.NewEventToken(ctx)
	if err != nil || !strings.HasPrefix(token, "evtcap_") || len(token) < 50 || len(token) > 178 {
		return Session{}, service.fail(ctx, request.Metadata, started)
	}
	digest := TokenDigest(token)
	var session Session
	err = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var createErr error
		session, createErr = service.deps.Repository.CreateSession(txctx, SessionCommand{Request: request, Token: token, TokenDigest: digest, Now: started, ExpiresAt: started.Add(service.deps.SessionTTL), LeaseExpiresAt: started.Add(service.deps.AttemptLeaseTTL)})
		if createErr != nil {
			return createErr
		}
		return service.observe(txctx, request.Metadata, started, 201, "event.session.create")
	})
	if err != nil {
		return Session{}, service.normalize(ctx, request.Metadata, started, err)
	}
	if session.Validate() != nil || session.Token != token || session.TokenDigest != digest {
		return Session{}, service.fail(ctx, request.Metadata, started)
	}
	service.healthy()
	return session, nil
}

func (service *Service) Append(ctx context.Context, request BatchRequest) (Ack, error) {
	started := service.deps.Clock.Now()
	if request.Validate() != nil {
		failure := NewError(CategoryValidation, ReasonInvalidRequest)
		if typed, ok := AsError(request.Validate()); ok && typed.Category == CategoryCapacity {
			failure = typed
		}
		return Ack{}, service.reject(ctx, request.Metadata, started, statusFor(failure), failure)
	}
	keyHash := sha256.Sum256([]byte(request.IdempotencyKey))
	command := AppendCommand{Request: request, TokenDigest: TokenDigest(request.Token), IdempotencyKeyDigest: hex.EncodeToString(keyHash[:]), RequestDigest: BatchDigest(request), Now: started}
	var ack Ack
	err := service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		var appendErr error
		ack, appendErr = service.deps.Repository.Append(txctx, command)
		if appendErr != nil {
			return appendErr
		}
		return service.observe(txctx, request.Metadata, started, 200, "event.batch.append")
	})
	if err != nil {
		return Ack{}, service.normalize(ctx, request.Metadata, started, err)
	}
	if ack.Validate() != nil {
		return Ack{}, service.fail(ctx, request.Metadata, started)
	}
	service.healthy()
	return ack, nil
}

func (service *Service) BatchURL(runID string) string {
	return service.deps.ControlPlaneBaseURL + "/v1/agent-runs/" + runID + "/events:batch"
}
func (service *Service) Name() string { return "event-ledger-service" }
func (service *Service) Check(context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.unhealthy {
		return errors.New("event ledger unhealthy")
	}
	return nil
}

func (service *Service) normalize(ctx context.Context, metadata platform.RequestMetadata, started time.Time, err error) error {
	if typed, ok := AsError(err); ok && typed.Category != CategoryDependency {
		return service.reject(ctx, metadata, started, statusFor(typed), typed)
	}
	return service.fail(ctx, metadata, started)
}
func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int, failure error) error {
	if service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		return service.observe(txctx, metadata, started, status, "event.reject")
	}) != nil {
		service.unhealthyState()
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return failure
}
func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, started time.Time) error {
	service.unhealthyState()
	_ = service.deps.UoW.Within(ctx, func(txctx context.Context) error {
		return service.observe(txctx, metadata, started, 503, "event.failure")
	})
	return NewError(CategoryDependency, ReasonDependencyUnavailable)
}
func (service *Service) observe(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int, operation string) error {
	id, err := service.deps.IDs.NewAuditID(ctx)
	if err != nil || !prefixedUUID("aud_", id) {
		return errors.New("invalid audit id")
	}
	ended := service.deps.Clock.Now()
	outcome, span := observability.OutcomeSucceeded, observability.SpanStatusOK
	if status >= 500 {
		outcome, span = observability.OutcomeFailed, observability.SpanStatusError
	} else if status >= 400 {
		outcome, span = observability.OutcomeRejected, observability.SpanStatusError
	}
	return service.deps.Observability.AppendObservation(ctx, observability.AuditEntry{ID: id, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: operation, Outcome: outcome, HTTPStatus: status}, observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: operation, StartedAt: started, EndedAt: ended, Status: span})
}
func (service *Service) unhealthyState() {
	service.mu.Lock()
	service.unhealthy = true
	service.mu.Unlock()
}
func (service *Service) healthy() { service.mu.Lock(); service.unhealthy = false; service.mu.Unlock() }

func statusFor(err error) int {
	typed, ok := AsError(err)
	if !ok {
		return 503
	}
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
		return 413
	case CategoryTimeout:
		return 408
	default:
		return 503
	}
}
func validateBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
