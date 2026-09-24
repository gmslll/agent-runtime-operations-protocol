// Package secrets implements the deployment-local SecretRef resolver. It has
// no HTTP surface and never discovers bindings from ambient configuration.
package secrets

import (
	"context"
	"errors"
	"regexp"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	observability "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	secretports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/secrets"
)

const auditOperation = "secret.resolve"

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// Provider returns a fresh caller-owned byte slice for one resolution. It must
// not return shared or cached storage. Provider errors are never exposed.
type Provider interface {
	Resolve(context.Context) ([]byte, error)
}

type ProviderFunc func(context.Context) ([]byte, error)

func (function ProviderFunc) Resolve(ctx context.Context) ([]byte, error) { return function(ctx) }

// Binding is explicit deployment configuration. Every identity and operation
// allowlist is required; an empty list denies all access.
type Binding struct {
	Reference          string
	AllowedOperations  []string
	AllowedSubjects    []string
	AllowedCredentials []string
	RequiredScopes     []string
	ExpiresAt          time.Time
	Provider           Provider
}

type Config struct {
	Bindings      []Binding
	Clock         platformports.Clock
	IDs           platformports.IDSource
	Observability observability.ObservationWriter
}

type Resolver struct {
	bindings      map[string]Binding
	clock         platformports.Clock
	ids           platformports.IDSource
	observability observability.ObservationWriter
}

func New(config Config) (*Resolver, error) {
	if config.Clock == nil || config.IDs == nil || config.Observability == nil {
		return nil, errors.New("secret resolver dependencies are required")
	}
	bindings := make(map[string]Binding, len(config.Bindings))
	for _, binding := range config.Bindings {
		if err := validateBinding(binding); err != nil {
			return nil, errors.New("invalid secret resolver binding")
		}
		if _, duplicate := bindings[binding.Reference]; duplicate {
			return nil, errors.New("duplicate secret resolver binding")
		}
		bindings[binding.Reference] = cloneBinding(binding)
	}
	return &Resolver{
		bindings: bindings, clock: config.Clock, ids: config.IDs,
		observability: config.Observability,
	}, nil
}

func (resolver *Resolver) Use(ctx context.Context, request secretports.ResolveRequest, callback secretports.UseFunc) error {
	if callback == nil {
		return secretports.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return secretports.ErrDenied
	}
	startedAt := resolver.clock.Now()
	if err := request.Validate(); err != nil {
		return secretports.ErrDenied
	}
	if !request.Deadline.After(startedAt) {
		return resolver.reject(ctx, request, startedAt)
	}
	binding, allowed := resolver.authorize(request, startedAt)
	if !allowed {
		return resolver.reject(ctx, request, startedAt)
	}

	effectiveDeadline := request.Deadline
	if binding.ExpiresAt.Before(effectiveDeadline) {
		effectiveDeadline = binding.ExpiresAt
	}
	resolveCtx, cancel := context.WithDeadline(ctx, effectiveDeadline)
	defer cancel()
	providerValue, err := resolveProvider(binding.Provider, resolveCtx)
	if err != nil || len(providerValue) == 0 {
		zero(providerValue)
		if ctx.Err() != nil {
			return secretports.ErrDenied
		}
		if !resolver.active(resolveCtx, effectiveDeadline) {
			return resolver.reject(ctx, request, startedAt)
		}
		return resolver.fail(ctx, request, startedAt)
	}
	if !resolver.active(resolveCtx, effectiveDeadline) {
		zero(providerValue)
		return resolver.reject(ctx, request, startedAt)
	}
	value := append([]byte(nil), providerValue...)
	zero(providerValue)
	defer zero(value)

	if err := resolver.observe(resolveCtx, request, startedAt, observability.OutcomeSucceeded, 200); err != nil {
		if !resolver.active(resolveCtx, effectiveDeadline) {
			return secretports.ErrDenied
		}
		return secretports.ErrUnavailable
	}
	if !resolver.active(resolveCtx, effectiveDeadline) {
		return secretports.ErrDenied
	}
	return invoke(callback, secretports.NewView(value))
}

func (resolver *Resolver) active(ctx context.Context, deadline time.Time) bool {
	return ctx.Err() == nil && deadline.After(resolver.clock.Now())
}

func resolveProvider(provider Provider, ctx context.Context) (value []byte, err error) {
	defer func() {
		if recover() != nil {
			zero(value)
			value = nil
			err = secretports.ErrUnavailable
		}
	}()
	return provider.Resolve(ctx)
}

func (resolver *Resolver) authorize(request secretports.ResolveRequest, now time.Time) (Binding, bool) {
	binding, ok := resolver.bindings[request.Reference]
	if !ok || !binding.ExpiresAt.After(now) {
		return Binding{}, false
	}
	if !contains(binding.AllowedOperations, request.Operation) ||
		!contains(binding.AllowedSubjects, request.SubjectID) ||
		!contains(binding.AllowedCredentials, request.CredentialID) {
		return Binding{}, false
	}
	for _, required := range binding.RequiredScopes {
		if !contains(request.Scopes, required) {
			return Binding{}, false
		}
	}
	return binding, true
}

func (resolver *Resolver) reject(ctx context.Context, request secretports.ResolveRequest, startedAt time.Time) error {
	if err := resolver.observe(ctx, request, startedAt, observability.OutcomeRejected, 403); err != nil {
		return secretports.ErrUnavailable
	}
	return secretports.ErrDenied
}

func (resolver *Resolver) fail(ctx context.Context, request secretports.ResolveRequest, startedAt time.Time) error {
	if err := resolver.observe(ctx, request, startedAt, observability.OutcomeFailed, 503); err != nil {
		return secretports.ErrUnavailable
	}
	return secretports.ErrUnavailable
}

func (resolver *Resolver) observe(ctx context.Context, request secretports.ResolveRequest, startedAt time.Time, outcome observability.Outcome, status int) error {
	endedAt := resolver.clock.Now()
	if endedAt.Before(startedAt) {
		return errors.New("secret resolver clock moved backwards")
	}
	auditID, err := resolver.ids.NewID(ctx, platformports.IDAudit)
	if err != nil {
		return err
	}
	spanID, err := resolver.ids.NewID(ctx, platformports.IDSpan)
	if err != nil {
		return err
	}
	return resolver.observability.AppendObservation(ctx,
		observability.AuditEntry{
			ID: auditID, OccurredAt: endedAt, RequestID: request.RequestID,
			TraceID: request.TraceID, Operation: auditOperation, Outcome: outcome,
			HTTPStatus: status,
		},
		observability.SpanRecord{
			TraceID: request.TraceID, SpanID: spanID, ParentSpanID: request.ParentSpanID,
			RequestID: request.RequestID, Operation: auditOperation, StartedAt: startedAt,
			EndedAt: endedAt, Status: spanStatus(outcome),
		})
}

func spanStatus(outcome observability.Outcome) observability.SpanStatus {
	if outcome == observability.OutcomeSucceeded {
		return observability.SpanStatusOK
	}
	return observability.SpanStatusError
}

func invoke(callback secretports.UseFunc, view secretports.SecretView) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = secretports.ErrUnavailable
		}
	}()
	if err := callback(view); err != nil {
		return secretports.ErrUnavailable
	}
	return nil
}

func validateBinding(binding Binding) error {
	if !validIdentifier(binding.Reference) || binding.Provider == nil ||
		binding.ExpiresAt.IsZero() || binding.ExpiresAt.Location() != time.UTC {
		return errors.New("invalid binding")
	}
	if !validSet(binding.AllowedOperations, validIdentifier) ||
		!validSet(binding.AllowedSubjects, validIdentity) ||
		!validSet(binding.AllowedCredentials, validIdentity) ||
		!validSet(binding.RequiredScopes, validIdentifier) {
		return errors.New("invalid binding allowlist")
	}
	return nil
}

func validSet(values []string, valid func(string) bool) bool {
	if len(values) == 0 || len(values) > 64 {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !valid(value) {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validIdentifier(value string) bool {
	return len(value) <= 100 && identifierPattern.MatchString(value)
}

func validIdentity(value string) bool {
	if len(value) == 0 || len(value) > 200 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' ||
			character == ':' || character == '@' || character == '/' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func cloneBinding(binding Binding) Binding {
	binding.AllowedOperations = append([]string(nil), binding.AllowedOperations...)
	binding.AllowedSubjects = append([]string(nil), binding.AllowedSubjects...)
	binding.AllowedCredentials = append([]string(nil), binding.AllowedCredentials...)
	binding.RequiredScopes = append([]string(nil), binding.RequiredScopes...)
	return binding
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ secretports.Resolver = (*Resolver)(nil)
