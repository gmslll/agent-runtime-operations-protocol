// Package identity implements the reference-only development identity and
// credential lifecycle. It deliberately exposes no HTTP or public wire types.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

const (
	defaultMaximumTTL = 15 * time.Minute
	defaultCacheTTL   = time.Minute
	credentialPrefix  = "arop_dev_"
)

var (
	ErrUnauthenticated    = errors.New("credential authentication failed")
	ErrUnauthorized       = errors.New("credential authorization failed")
	ErrCredentialNotFound = errors.New("credential not found")
	ErrCredentialConflict = errors.New("credential lifecycle conflict")
	ErrSecretUnavailable  = errors.New("credential secret is no longer available")
	ErrUnavailable        = errors.New("identity dependency unavailable")

	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	scopePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
	domainIDPattern   = regexp.MustCompile(`^(?:cred|prn)_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type CredentialStatus string

const (
	CredentialActive   CredentialStatus = "active"
	CredentialRevoked  CredentialStatus = "revoked"
	CredentialReplaced CredentialStatus = "replaced"
)

// CredentialRecord is safe to persist. SecretVerifier, IdempotencyDigest, and
// IdempotencyRequestDigest are one-way SHA-256 values; raw credentials and
// idempotency keys never cross the repository boundary.
type CredentialRecord struct {
	CredentialID             string
	PrincipalID              string
	SubjectID                string
	Kind                     string
	Audience                 string
	Scopes                   []string
	SecretVerifier           string
	IssuedAt                 time.Time
	NotBefore                time.Time
	ExpiresAt                time.Time
	Status                   CredentialStatus
	RevokedAt                time.Time
	ReplacedAt               time.Time
	ReplacementID            string
	Revision                 int64
	IdempotencyDigest        string
	IdempotencyRequestDigest string
}

type CredentialRepository interface {
	Create(context.Context, *CredentialRecord) error
	Get(context.Context, string) (CredentialRecord, error)
	GetByIdempotencyDigest(context.Context, string) (CredentialRecord, error)
	Replace(context.Context, CredentialRecord, *CredentialRecord, time.Time) error
	Revoke(context.Context, CredentialRecord, time.Time) error
}

type Dependencies struct {
	Clock            platformports.Clock
	IDs              platformports.IDSource
	Faults           platformports.FaultHook
	UoW              platformports.UnitOfWork
	Observability    observability.ObservationWriter
	Repository       CredentialRepository
	Random           func([]byte) (int, error)
	MaximumTTL       time.Duration
	CacheTTL         time.Duration
	AllowedKinds     []string
	AllowedAudiences []string
	AllowedScopes    []string
}

type Service struct {
	deps             Dependencies
	cache            *validationCache
	allowedKinds     map[string]struct{}
	allowedAudiences map[string]struct{}
	allowedScopes    map[string]struct{}
	mu               sync.RWMutex
	timeMu           sync.Mutex
	lastNow          time.Time
}

type IssueRequest struct {
	SubjectID      string
	Kind           string
	Audience       string
	Scopes         []string
	TTL            time.Duration
	NotBefore      time.Time
	IdempotencyKey string
	Metadata       platform.RequestMetadata
}

type AuthenticateRequest struct {
	Credential string
	Audience   string
	Scopes     []string
	Metadata   platform.RequestMetadata
}

type RotateRequest struct {
	CredentialID   string
	IdempotencyKey string
	TTL            time.Duration
	Metadata       platform.RequestMetadata
}

type RevokeRequest struct {
	CredentialID string
	Metadata     platform.RequestMetadata
}

type IssuedCredential struct {
	CredentialID string
	PrincipalID  string
	Credential   string
	ExpiresAt    time.Time
	Scopes       []string
}

type Principal struct {
	PrincipalID  string
	SubjectID    string
	CredentialID string
	Kind         string
	Audience     string
	Scopes       []string
	ExpiresAt    time.Time
}

func New(dependencies Dependencies) (*Service, error) {
	if dependencies.Clock == nil || dependencies.IDs == nil || dependencies.Faults == nil || dependencies.UoW == nil || dependencies.Observability == nil || dependencies.Repository == nil {
		return nil, errors.New("identity service requires clock, IDs, faults, unit of work, observability, and repository")
	}
	if dependencies.Random == nil {
		dependencies.Random = rand.Read
	}
	if dependencies.MaximumTTL == 0 {
		dependencies.MaximumTTL = defaultMaximumTTL
	}
	if dependencies.CacheTTL == 0 {
		dependencies.CacheTTL = defaultCacheTTL
	}
	if dependencies.MaximumTTL <= 0 || dependencies.MaximumTTL > 24*time.Hour || dependencies.CacheTTL <= 0 || dependencies.CacheTTL > dependencies.MaximumTTL {
		return nil, errors.New("identity TTL configuration is invalid")
	}
	allowedKinds, err := validatedAllowlist("credential kind", dependencies.AllowedKinds, identifierPattern)
	if err != nil {
		return nil, err
	}
	allowedAudiences, err := validatedAllowlist("credential audience", dependencies.AllowedAudiences, identifierPattern)
	if err != nil {
		return nil, err
	}
	allowedScopes, err := validatedAllowlist("credential scope", dependencies.AllowedScopes, scopePattern)
	if err != nil {
		return nil, err
	}
	now := dependencies.Clock.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return nil, errors.New("identity clock must return non-zero UTC time")
	}
	return &Service{deps: dependencies, cache: newValidationCache(dependencies.CacheTTL), allowedKinds: allowedKinds, allowedAudiences: allowedAudiences, allowedScopes: allowedScopes, lastNow: now}, nil
}

func (service *Service) Issue(ctx context.Context, request IssueRequest) (IssuedCredential, error) {
	request.Scopes = canonicalScopes(request.Scopes)
	if err := service.validateIssue(request); err != nil {
		return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.issue", 400, err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := service.deps.Faults.Check(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.issue", err)
	}
	idempotencyDigest := digestString(request.IdempotencyKey)
	requestDigest := issueRequestDigest(request)
	if existing, err := service.deps.Repository.GetByIdempotencyDigest(ctx, idempotencyDigest); err == nil {
		if existing.IdempotencyRequestDigest != requestDigest {
			return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.issue", 409, ErrCredentialConflict)
		}
		return IssuedCredential{CredentialID: existing.CredentialID, PrincipalID: existing.PrincipalID, ExpiresAt: existing.ExpiresAt, Scopes: cloneScopes(existing.Scopes)}, service.reject(ctx, request.Metadata, "credential.issue", 409, ErrSecretUnavailable)
	} else if !errors.Is(err, ErrCredentialNotFound) {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.issue", err)
	}
	now := service.now()
	notBefore := request.NotBefore
	if notBefore.IsZero() {
		notBefore = now
	}
	if notBefore.Before(now) {
		notBefore = now
	}
	if !notBefore.Before(now.Add(request.TTL)) {
		return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.issue", 400, errors.New("credential not-before exceeds expiry"))
	}
	record, raw, err := service.newRecord(ctx, request.SubjectID, request.Kind, request.Audience, request.Scopes, now, notBefore, now.Add(request.TTL), idempotencyDigest, requestDigest)
	if err != nil {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.issue", err)
	}
	started := now
	err = service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		if err := service.deps.Repository.Create(transactionContext, &record); err != nil {
			return err
		}
		if err := service.deps.Faults.Check(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := service.appendObservation(transactionContext, request.Metadata, "credential.issue", started, 200); err != nil {
			return err
		}
		return service.deps.Faults.Check(transactionContext, platformports.CheckpointAfterUseCase)
	})
	if err != nil {
		return IssuedCredential{}, service.classifyMutationFailure(ctx, request.Metadata, "credential.issue", err)
	}
	return IssuedCredential{CredentialID: record.CredentialID, PrincipalID: record.PrincipalID, Credential: raw, ExpiresAt: record.ExpiresAt, Scopes: cloneScopes(record.Scopes)}, nil
}

func (service *Service) Authenticate(ctx context.Context, request AuthenticateRequest) (Principal, error) {
	request.Scopes = canonicalScopes(request.Scopes)
	if request.Credential == "" || !identifierPattern.MatchString(request.Audience) || len(request.Scopes) == 0 {
		return Principal{}, service.rejectedAuthentication(ctx, request.Metadata, ErrUnauthenticated)
	}
	credentialID, verifier, ok := parseCredential(request.Credential)
	if !ok {
		return Principal{}, service.rejectedAuthentication(ctx, request.Metadata, ErrUnauthenticated)
	}
	service.mu.RLock()
	defer service.mu.RUnlock()
	if err := service.cache.Check(ctx); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	cacheKey := credentialID + "\x00" + verifier + "\x00" + request.Audience + "\x00" + strings.Join(request.Scopes, " ")
	now := service.now()
	if principal, found := service.cache.get(cacheKey, now); found {
		if err := service.auditAuthentication(ctx, request.Metadata, now, 200); err != nil {
			return Principal{}, err
		}
		return principal, nil
	}
	record, err := service.deps.Repository.Get(ctx, credentialID)
	if err != nil && !errors.Is(err, ErrCredentialNotFound) {
		return Principal{}, service.fail(ctx, request.Metadata, "credential.authenticate", err)
	}
	if err != nil || subtle.ConstantTimeCompare([]byte(record.SecretVerifier), []byte(verifier)) != 1 || record.Status != CredentialActive || now.Before(record.NotBefore) || !now.Before(record.ExpiresAt) {
		if auditErr := service.auditAuthentication(ctx, request.Metadata, now, 401); auditErr != nil {
			return Principal{}, auditErr
		}
		return Principal{}, ErrUnauthenticated
	}
	if record.Audience != request.Audience || !containsScopes(record.Scopes, request.Scopes) {
		if auditErr := service.auditAuthentication(ctx, request.Metadata, now, 403); auditErr != nil {
			return Principal{}, auditErr
		}
		return Principal{}, ErrUnauthorized
	}
	principal := Principal{PrincipalID: record.PrincipalID, SubjectID: record.SubjectID, CredentialID: record.CredentialID, Kind: record.Kind, Audience: record.Audience, Scopes: cloneScopes(record.Scopes), ExpiresAt: record.ExpiresAt}
	if err := service.auditAuthentication(ctx, request.Metadata, now, 200); err != nil {
		return Principal{}, err
	}
	service.cache.put(cacheKey, principal, record.ExpiresAt, now)
	return principal, nil
}

func (service *Service) Rotate(ctx context.Context, request RotateRequest) (IssuedCredential, error) {
	if !validCredentialID(request.CredentialID) || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 || request.TTL <= 0 || request.TTL > service.deps.MaximumTTL {
		return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.rotate", 400, errors.New("invalid credential rotation request"))
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := service.deps.Faults.Check(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.rotate", err)
	}
	old, err := service.deps.Repository.Get(ctx, request.CredentialID)
	if errors.Is(err, ErrCredentialNotFound) {
		return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.rotate", 409, ErrCredentialConflict)
	}
	if err != nil {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.rotate", err)
	}
	idempotencyDigest := digestString(request.IdempotencyKey)
	requestDigest := rotateRequestDigest(request, old)
	if existing, err := service.deps.Repository.GetByIdempotencyDigest(ctx, idempotencyDigest); err == nil {
		if existing.IdempotencyRequestDigest != requestDigest || old.Status != CredentialReplaced || old.ReplacementID != existing.CredentialID {
			return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.rotate", 409, ErrCredentialConflict)
		}
		return IssuedCredential{CredentialID: existing.CredentialID, PrincipalID: existing.PrincipalID, ExpiresAt: existing.ExpiresAt, Scopes: cloneScopes(existing.Scopes)}, service.reject(ctx, request.Metadata, "credential.rotate", 409, ErrSecretUnavailable)
	} else if !errors.Is(err, ErrCredentialNotFound) {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.rotate", err)
	}
	now := service.now()
	if old.Status != CredentialActive || !now.Before(old.ExpiresAt) {
		return IssuedCredential{}, service.reject(ctx, request.Metadata, "credential.rotate", 409, ErrCredentialConflict)
	}
	replacement, raw, err := service.newRecord(ctx, old.SubjectID, old.Kind, old.Audience, old.Scopes, now, now, now.Add(request.TTL), idempotencyDigest, requestDigest)
	if err != nil {
		return IssuedCredential{}, service.fail(ctx, request.Metadata, "credential.rotate", err)
	}
	replacement.PrincipalID = old.PrincipalID
	err = service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		if err := service.deps.Repository.Replace(transactionContext, old, &replacement, now); err != nil {
			return err
		}
		if err := service.deps.Faults.Check(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := service.appendObservation(transactionContext, request.Metadata, "credential.rotate", now, 200); err != nil {
			return err
		}
		if err := service.cache.invalidate(old.CredentialID); err != nil {
			return err
		}
		return service.deps.Faults.Check(transactionContext, platformports.CheckpointAfterUseCase)
	})
	if err != nil {
		return IssuedCredential{}, service.classifyMutationFailure(ctx, request.Metadata, "credential.rotate", err)
	}
	return IssuedCredential{CredentialID: replacement.CredentialID, PrincipalID: replacement.PrincipalID, Credential: raw, ExpiresAt: replacement.ExpiresAt, Scopes: cloneScopes(replacement.Scopes)}, nil
}

func (service *Service) Revoke(ctx context.Context, request RevokeRequest) error {
	if !validCredentialID(request.CredentialID) {
		return service.reject(ctx, request.Metadata, "credential.revoke", 400, ErrCredentialConflict)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := service.deps.Faults.Check(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return service.fail(ctx, request.Metadata, "credential.revoke", err)
	}
	record, err := service.deps.Repository.Get(ctx, request.CredentialID)
	if errors.Is(err, ErrCredentialNotFound) {
		return service.reject(ctx, request.Metadata, "credential.revoke", 409, ErrCredentialConflict)
	}
	if err != nil {
		return service.fail(ctx, request.Metadata, "credential.revoke", err)
	}
	if record.Status == CredentialRevoked {
		if err := service.cache.invalidate(record.CredentialID); err != nil {
			return service.fail(ctx, request.Metadata, "credential.revoke", err)
		}
		if err := service.auditOperation(ctx, request.Metadata, "credential.revoke", service.now(), 200); err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return nil
	}
	if record.Status != CredentialActive && record.Status != CredentialReplaced {
		return service.reject(ctx, request.Metadata, "credential.revoke", 409, ErrCredentialConflict)
	}
	now := service.now()
	err = service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		if err := service.deps.Repository.Revoke(transactionContext, record, now); err != nil {
			return err
		}
		if err := service.deps.Faults.Check(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := service.appendObservation(transactionContext, request.Metadata, "credential.revoke", now, 200); err != nil {
			return err
		}
		if err := service.cache.invalidate(record.CredentialID); err != nil {
			return err
		}
		return service.deps.Faults.Check(transactionContext, platformports.CheckpointAfterUseCase)
	})
	if err != nil {
		return service.classifyMutationFailure(ctx, request.Metadata, "credential.revoke", err)
	}
	return nil
}

func (service *Service) Name() string                    { return "identity-cache" }
func (service *Service) Check(ctx context.Context) error { return service.cache.Check(ctx) }
func (service *Service) RebuildCache() {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.cache.rebuild()
}

func (service *Service) validateIssue(request IssueRequest) error {
	if !identifierPattern.MatchString(request.SubjectID) || !identifierPattern.MatchString(request.Kind) || !identifierPattern.MatchString(request.Audience) {
		return errors.New("credential subject, kind, and audience are required")
	}
	if len(request.Scopes) == 0 || request.TTL <= 0 || request.TTL > service.deps.MaximumTTL || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 {
		return errors.New("credential scope, TTL, or idempotency key is invalid")
	}
	if _, ok := service.allowedKinds[request.Kind]; !ok {
		return errors.New("credential kind is not allowed")
	}
	if _, ok := service.allowedAudiences[request.Audience]; !ok {
		return errors.New("credential audience is not allowed")
	}
	for _, scope := range request.Scopes {
		_, allowed := service.allowedScopes[scope]
		if len(scope) > 100 || !scopePattern.MatchString(scope) || strings.Contains(scope, "*") || !allowed {
			return errors.New("credential scope is invalid")
		}
	}
	return nil
}

func (service *Service) newRecord(ctx context.Context, subject, kind, audience string, scopes []string, issued, notBefore, expires time.Time, idempotencyDigest, requestDigest string) (CredentialRecord, string, error) {
	credentialID, err := service.newDomainID(ctx, "cred_")
	if err != nil {
		return CredentialRecord{}, "", err
	}
	principalID, err := service.newDomainID(ctx, "prn_")
	if err != nil {
		return CredentialRecord{}, "", err
	}
	secret := make([]byte, 32)
	defer clear(secret)
	if count, err := service.deps.Random(secret); err != nil || count != len(secret) {
		return CredentialRecord{}, "", fmt.Errorf("generate credential entropy: %w", errors.Join(err, errors.New("entropy source returned incomplete data")))
	}
	allZero := true
	for _, value := range secret {
		allZero = allZero && value == 0
	}
	if allZero {
		return CredentialRecord{}, "", errors.New("credential entropy is all zero")
	}
	raw := credentialPrefix + credentialID + "." + base64.RawURLEncoding.EncodeToString(secret)
	return CredentialRecord{CredentialID: credentialID, PrincipalID: principalID, SubjectID: subject, Kind: kind, Audience: audience, Scopes: cloneScopes(scopes), SecretVerifier: digestString(raw), IssuedAt: issued.UTC(), NotBefore: notBefore.UTC(), ExpiresAt: expires.UTC(), Status: CredentialActive, Revision: 1, IdempotencyDigest: idempotencyDigest, IdempotencyRequestDigest: requestDigest}, raw, nil
}

func (service *Service) newDomainID(ctx context.Context, prefix string) (string, error) {
	identifier, err := service.deps.IDs.NewID(ctx, platformports.IDRequest)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(identifier, "req_") {
		return "", errors.New("ID source returned invalid request identifier")
	}
	return prefix + strings.TrimPrefix(identifier, "req_"), nil
}

func (service *Service) appendObservation(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, status int) error {
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
		observability.AuditEntry{ID: auditID, OccurredAt: ended, RequestID: metadata.RequestID, TraceID: metadata.TraceID, Operation: operation, Outcome: outcome, HTTPStatus: status},
		observability.SpanRecord{TraceID: metadata.TraceID, SpanID: metadata.SpanID, ParentSpanID: metadata.ParentSpanID, RequestID: metadata.RequestID, Operation: operation, StartedAt: started, EndedAt: ended, Status: spanStatus})
}

func (service *Service) auditAuthentication(ctx context.Context, metadata platform.RequestMetadata, started time.Time, status int) error {
	if err := service.auditOperation(ctx, metadata, "credential.authenticate", started, status); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (service *Service) auditOperation(ctx context.Context, metadata platform.RequestMetadata, operation string, started time.Time, status int) error {
	return service.deps.UoW.Within(ctx, func(transactionContext context.Context) error {
		return service.appendObservation(transactionContext, metadata, operation, started, status)
	})
}

func (service *Service) rejectedAuthentication(ctx context.Context, metadata platform.RequestMetadata, result error) error {
	if err := service.auditAuthentication(ctx, metadata, service.now(), 401); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return result
}

func (service *Service) reject(ctx context.Context, metadata platform.RequestMetadata, operation string, status int, result error) error {
	if err := service.auditOperation(ctx, metadata, operation, service.now(), status); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return result
}

func (service *Service) fail(ctx context.Context, metadata platform.RequestMetadata, operation string, cause error) error {
	if err := service.auditOperation(ctx, metadata, operation, service.now(), 503); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, errors.Join(cause, err))
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, cause)
}

func (service *Service) classifyMutationFailure(ctx context.Context, metadata platform.RequestMetadata, operation string, cause error) error {
	if errors.Is(cause, ErrCredentialConflict) {
		return service.reject(ctx, metadata, operation, 409, ErrCredentialConflict)
	}
	return service.fail(ctx, metadata, operation, cause)
}

func (service *Service) now() time.Time {
	current := service.deps.Clock.Now()
	service.timeMu.Lock()
	defer service.timeMu.Unlock()
	if current.Before(service.lastNow) {
		return service.lastNow
	}
	service.lastNow = current
	return current
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func issueRequestDigest(request IssueRequest) string {
	notBefore := "immediate"
	if !request.NotBefore.IsZero() {
		notBefore = request.NotBefore.UTC().Format(time.RFC3339Nano)
	}
	return digestString(strings.Join([]string{"credential.issue", request.SubjectID, request.Kind, request.Audience, strings.Join(request.Scopes, " "), strconv.FormatInt(int64(request.TTL), 10), notBefore}, "\n"))
}

func rotateRequestDigest(request RotateRequest, current CredentialRecord) string {
	return digestString(strings.Join([]string{"credential.rotate", request.CredentialID, current.SubjectID, current.Kind, current.Audience, strings.Join(current.Scopes, " "), strconv.FormatInt(int64(request.TTL), 10), "immediate"}, "\n"))
}

func parseCredential(value string) (string, string, bool) {
	if !strings.HasPrefix(value, credentialPrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(value, credentialPrefix), ".")
	if len(parts) != 2 || !validCredentialID(parts[0]) {
		return "", "", false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secret) != 32 {
		return "", "", false
	}
	return parts[0], digestString(value), true
}

func validCredentialID(value string) bool {
	return strings.HasPrefix(value, "cred_") && domainIDPattern.MatchString(value)
}

func canonicalScopes(values []string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func cloneScopes(values []string) []string { return append([]string(nil), values...) }
func containsScopes(granted, requested []string) bool {
	set := make(map[string]struct{}, len(granted))
	for _, value := range granted {
		set[value] = struct{}{}
	}
	for _, value := range requested {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func validatedAllowlist(name string, values []string, pattern *regexp.Regexp) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !pattern.MatchString(value) || strings.Contains(value, "*") {
			return nil, fmt.Errorf("%s allowlist contains invalid value", name)
		}
		result[value] = struct{}{}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%s allowlist is required", name)
	}
	return result, nil
}

type cacheEntry struct {
	principal Principal
	expiresAt time.Time
}
type validationCache struct {
	mu             sync.Mutex
	ttl            time.Duration
	entries        map[string]cacheEntry
	unhealthy      error
	invalidateHook func(string) error
}

func newValidationCache(ttl time.Duration) *validationCache {
	return &validationCache{ttl: ttl, entries: map[string]cacheEntry{}}
}
func (cache *validationCache) get(key string, now time.Time) (Principal, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		delete(cache.entries, key)
		return Principal{}, false
	}
	entry.principal.Scopes = cloneScopes(entry.principal.Scopes)
	return entry.principal, true
}
func (cache *validationCache) put(key string, principal Principal, credentialExpiry, now time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	expires := now.Add(cache.ttl)
	if credentialExpiry.Before(expires) {
		expires = credentialExpiry
	}
	principal.Scopes = cloneScopes(principal.Scopes)
	cache.entries[key] = cacheEntry{principal: principal, expiresAt: expires}
}
func (cache *validationCache) invalidate(credentialID string) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.invalidateHook != nil {
		if err := cache.invalidateHook(credentialID); err != nil {
			cache.unhealthy = fmt.Errorf("identity cache invalidation failed: %w", err)
			return cache.unhealthy
		}
	}
	for key := range cache.entries {
		if strings.HasPrefix(key, credentialID+"\x00") {
			delete(cache.entries, key)
		}
	}
	return nil
}
func (cache *validationCache) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.unhealthy
}
func (cache *validationCache) rebuild() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries = map[string]cacheEntry{}
	cache.unhealthy = nil
}

var _ platformports.ReadinessCheck = (*Service)(nil)
