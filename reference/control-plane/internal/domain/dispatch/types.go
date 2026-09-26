package dispatch

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"errors"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const MaxSafeInteger uint64 = 9007199254740991

var (
	slugPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	uuidV7Pattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	keyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)
	scopePattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
	failurePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

type Operation string

const (
	OperationIssue    Operation = "dispatch.issue"
	OperationReadJWKS Operation = "dispatch.jwks.read"
)

type Caller struct {
	TenantID, PrincipalID, CredentialID string
	Scopes                              []string
}

func (caller Caller) Validate() error {
	if !slugPattern.MatchString(caller.TenantID) || len(caller.TenantID) > 128 || !prefixedUUID("prn_", caller.PrincipalID) || !prefixedUUID("cred_", caller.CredentialID) || len(caller.Scopes) == 0 || len(caller.Scopes) > 128 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	seen := map[string]struct{}{}
	for _, scope := range caller.Scopes {
		if !scopePattern.MatchString(scope) || len(scope) > 128 {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, exists := seen[scope]; exists {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

type RunView struct {
	TenantID                  string
	RunID                     string
	Agent                     run.AgentBinding
	State                     run.State
	StateVersion              uint64
	AuthorizationSnapshotHash string
	Traceparent               string
	Tracestate                string
	DeadlineAt                time.Time
}

func (value RunView) Validate() error {
	if !slugPattern.MatchString(value.TenantID) || len(value.TenantID) > 128 || !prefixedUUID("run_", value.RunID) || value.Agent.Validate() != nil || value.StateVersion == 0 || value.StateVersion > MaxSafeInteger || !digestPattern.MatchString(value.AuthorizationSnapshotHash) || !utc(value.DeadlineAt) || (protocolcore.TraceContext{Traceparent: value.Traceparent, Tracestate: value.Tracestate}).Validate() != nil {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	switch value.State {
	case run.StateQueued, run.StateDispatching:
	default:
		return NewError(CategoryConflict, ReasonRunNotDispatchable)
	}
	return nil
}

type Candidate struct {
	DeploymentID, InstanceID, SessionID, ServiceID                string
	Generation, ResourceVersion, Priority, Weight, AvailableSlots uint64
	TransportProfile                                              string
	Endpoint                                                      string
	LeaseExpiresAt                                                time.Time
}

func (candidate Candidate) Validate(now time.Time) error {
	endpoint, err := url.Parse(candidate.Endpoint)
	if candidate.DeploymentID != "" && !prefixedUUID("dep_", candidate.DeploymentID) || !slugPattern.MatchString(candidate.InstanceID) || len(candidate.InstanceID) > 128 || !prefixedUUID("ses_", candidate.SessionID) || !slugPattern.MatchString(candidate.ServiceID) || len(candidate.ServiceID) > 128 || candidate.Generation == 0 || candidate.Generation > MaxSafeInteger || candidate.ResourceVersion == 0 || candidate.ResourceVersion > MaxSafeInteger || candidate.Priority > MaxSafeInteger || candidate.Weight == 0 || candidate.Weight > 1000 || candidate.AvailableSlots == 0 || candidate.AvailableSlots > MaxSafeInteger || candidate.TransportProfile != "direct" && candidate.TransportProfile != "proxy" && candidate.TransportProfile != "worker_pull" || err != nil || len(candidate.Endpoint) > 2048 || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" && endpoint.Path != "/" || !utc(candidate.LeaseExpiresAt) || !candidate.LeaseExpiresAt.After(now) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func (candidate Candidate) InvocationEndpoint() string {
	return strings.TrimSuffix(candidate.Endpoint, "/") + "/v1/runs"
}

type AttemptState string

const (
	StateIssued    AttemptState = "issued"
	StateAccepted  AttemptState = "accepted"
	StateFailed    AttemptState = "failed"
	StateExpired   AttemptState = "expired"
	StateFenced    AttemptState = "fenced"
	StateCancelled AttemptState = "cancelled"
)

type Attempt struct {
	TenantID, RunID, AttemptID, TokenID                      string
	AttemptNumber, FencingToken                              uint64
	DeploymentID, InstanceID, SessionID, ServiceID           string
	Generation, RegistryResourceVersion                      uint64
	State                                                    AttemptState
	TransportProfile, Endpoint, Audience, SigningKeyID       string
	LeaseExpiresAt, TicketExpiresAt, CreatedAt               time.Time
	AcceptedAt, ClosedAt                                     *time.Time
	FailureCode, Traceparent, Tracestate, IdempotencyKeyHash string
	IdempotencyRequestHash                                   string
}

func (attempt Attempt) Validate() error {
	if !slugPattern.MatchString(attempt.TenantID) || len(attempt.TenantID) > 128 || !prefixedUUID("run_", attempt.RunID) || !prefixedUUID("att_", attempt.AttemptID) || !prefixedUUID("tok_", attempt.TokenID) || attempt.AttemptNumber == 0 || attempt.AttemptNumber > MaxSafeInteger || attempt.FencingToken == 0 || attempt.FencingToken > MaxSafeInteger || !prefixedUUID("dep_", attempt.DeploymentID) || !slugPattern.MatchString(attempt.InstanceID) || len(attempt.InstanceID) > 128 || !prefixedUUID("ses_", attempt.SessionID) || !slugPattern.MatchString(attempt.ServiceID) || len(attempt.ServiceID) > 128 || attempt.Generation == 0 || attempt.Generation > MaxSafeInteger || attempt.RegistryResourceVersion == 0 || attempt.RegistryResourceVersion > MaxSafeInteger || attempt.TransportProfile != "direct" && attempt.TransportProfile != "proxy" && attempt.TransportProfile != "worker_pull" || !keyIDPattern.MatchString(attempt.SigningKeyID) || !utc(attempt.LeaseExpiresAt) || !utc(attempt.TicketExpiresAt) || !utc(attempt.CreatedAt) || !attempt.LeaseExpiresAt.After(attempt.CreatedAt) || attempt.TicketExpiresAt.After(attempt.LeaseExpiresAt) || !attempt.TicketExpiresAt.After(attempt.CreatedAt) || !digestPattern.MatchString(attempt.IdempotencyRequestHash) || !hexPattern.MatchString(attempt.IdempotencyKeyHash) || (protocolcore.TraceContext{Traceparent: attempt.Traceparent, Tracestate: attempt.Tracestate}).Validate() != nil {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if parsed, err := url.Parse(attempt.Endpoint); err != nil || len(attempt.Endpoint) > 2048 || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v1/runs" {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if parsed, err := url.Parse(attempt.Audience); err != nil || len(attempt.Audience) > 2048 || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/deployments/"+attempt.DeploymentID {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	switch attempt.State {
	case StateIssued:
		if attempt.AcceptedAt != nil || attempt.ClosedAt != nil || attempt.FailureCode != "" {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case StateAccepted:
		if attempt.AcceptedAt == nil || attempt.ClosedAt != nil || attempt.FailureCode != "" {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case StateFailed:
		if attempt.ClosedAt == nil || !failurePattern.MatchString(attempt.FailureCode) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case StateExpired, StateFenced, StateCancelled:
		if attempt.ClosedAt == nil || attempt.FailureCode != "" && !failurePattern.MatchString(attempt.FailureCode) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	default:
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if attempt.AcceptedAt != nil && (!utc(*attempt.AcceptedAt) || attempt.AcceptedAt.Before(attempt.CreatedAt)) || attempt.ClosedAt != nil && (!utc(*attempt.ClosedAt) || attempt.ClosedAt.Before(attempt.CreatedAt) || attempt.AcceptedAt != nil && attempt.ClosedAt.Before(*attempt.AcceptedAt)) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type DispatchRequest struct {
	Caller         Caller
	RunID          string
	IdempotencyKey string
	Metadata       platform.RequestMetadata
}

func (request DispatchRequest) Validate() error {
	if request.Caller.Validate() != nil || !prefixedUUID("run_", request.RunID) || len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 200 || strings.TrimSpace(request.IdempotencyKey) != request.IdempotencyKey || strings.ContainsAny(request.IdempotencyKey, "\x00\r\n") {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type Delivery struct {
	Mode, DeploymentID, InstanceID, Audience, Endpoint, StreamEndpoint string
	Generation                                                         uint64
	ExpiresAt                                                          time.Time
}

type Ticket struct {
	RunID, AttemptID string
	FencingToken     uint64
	Agent            run.AgentBinding
	Delivery         Delivery
	RunToken         string
	Traceparent      string
	Tracestate       string
}

type KeyStatus string

const (
	KeyActive   KeyStatus = "active"
	KeyRetiring KeyStatus = "retiring"
)

type KeyMetadata struct {
	KeyID                  string
	Status                 KeyStatus
	X, Y                   string
	NotBefore, SignUntil   time.Time
	VerifyUntil, CreatedAt time.Time
}

func (key KeyMetadata) Validate(maxTTL time.Duration) error {
	if !keyIDPattern.MatchString(key.KeyID) || key.Status != KeyActive && key.Status != KeyRetiring || !utc(key.NotBefore) || !utc(key.SignUntil) || !utc(key.VerifyUntil) || !utc(key.CreatedAt) || key.SignUntil.Before(key.NotBefore) || key.VerifyUntil.Before(key.SignUntil.Add(2*maxTTL)) || key.CreatedAt.After(key.NotBefore) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if _, err := key.PublicKey(); err != nil {
		return err
	}
	return nil
}

func (key KeyMetadata) PublicKey() (*ecdsa.PublicKey, error) {
	x, errX := base64.RawURLEncoding.Strict().DecodeString(key.X)
	y, errY := base64.RawURLEncoding.Strict().DecodeString(key.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, NewError(CategoryValidation, ReasonTicketInvalid)
	}
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !public.Curve.IsOnCurve(public.X, public.Y) {
		return nil, NewError(CategoryValidation, ReasonTicketInvalid)
	}
	return public, nil
}

type JWKS struct {
	Issuer     string
	CacheUntil time.Time
	Keys       []KeyMetadata
}

func (jwks JWKS) Validate(now time.Time, maxTTL time.Duration) error {
	issuer, err := url.Parse(jwks.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" || issuer.Path != "" || !utc(jwks.CacheUntil) || jwks.CacheUntil.Before(now) || len(jwks.Keys) == 0 || len(jwks.Keys) > 32 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	keys := slices.Clone(jwks.Keys)
	sort.Slice(keys, func(i, j int) bool { return keys[i].KeyID < keys[j].KeyID })
	active := 0
	for index, key := range keys {
		if key.Validate(maxTTL) != nil || index > 0 && key.KeyID == keys[index-1].KeyID || !key.VerifyUntil.After(now) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if key.Status == KeyActive {
			active++
		}
	}
	if active != 1 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func prefixedUUID(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidV7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}

func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }

func normalizeKeys(keys []KeyMetadata) ([]KeyMetadata, error) {
	result := slices.Clone(keys)
	sort.Slice(result, func(i, j int) bool { return result[i].KeyID < result[j].KeyID })
	for index, key := range result {
		if index > 0 && key.KeyID == result[index-1].KeyID {
			return nil, errors.New("duplicate signing key")
		}
	}
	return result, nil
}
