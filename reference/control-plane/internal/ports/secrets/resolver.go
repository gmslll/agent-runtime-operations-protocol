// Package secrets defines the reference Control Plane's deployment-local
// SecretRef resolution port. It deliberately exposes no transport binding and
// no serializable secret value type.
package secrets

import (
	"context"
	"errors"
	"regexp"
	"time"
)

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	identityPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]*$`)
	requestIDPattern  = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	traceIDPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern     = regexp.MustCompile(`^[0-9a-f]{16}$`)

	// ErrDenied intentionally reveals neither whether a reference exists nor
	// which authorization check rejected the request.
	ErrDenied = errors.New("secret reference denied")
	// ErrUnavailable intentionally hides provider and audit implementation
	// details, which can contain sensitive deployment information.
	ErrUnavailable = errors.New("secret resolver unavailable")
)

// ResolveRequest carries only deployment-local authorization context. The
// request and trace identifiers correlate safe P08 audit records; they never
// carry the secret value.
type ResolveRequest struct {
	Reference    string
	Operation    string
	SubjectID    string
	CredentialID string
	Scopes       []string
	Deadline     time.Time
	RequestID    string
	TraceID      string
	ParentSpanID string
}

// Validate performs structural validation without revealing which field was
// rejected to callers.
func (request ResolveRequest) Validate() error {
	if !validIdentifier(request.Reference) || !validIdentifier(request.Operation) {
		return ErrDenied
	}
	if !validIdentity(request.SubjectID) || !validIdentity(request.CredentialID) {
		return ErrDenied
	}
	if request.Deadline.IsZero() || request.Deadline.Location() != time.UTC {
		return ErrDenied
	}
	if !requestIDPattern.MatchString(request.RequestID) ||
		!traceIDPattern.MatchString(request.TraceID) || allZero(request.TraceID) ||
		(request.ParentSpanID != "" && (!spanIDPattern.MatchString(request.ParentSpanID) || allZero(request.ParentSpanID))) {
		return ErrDenied
	}
	if len(request.Scopes) == 0 || len(request.Scopes) > 64 {
		return ErrDenied
	}
	seen := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if !validIdentifier(scope) {
			return ErrDenied
		}
		if _, duplicate := seen[scope]; duplicate {
			return ErrDenied
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func allZero(value string) bool {
	for _, character := range value {
		if character != '0' {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	return len(value) <= 100 && identifierPattern.MatchString(value)
}

func validIdentity(value string) bool {
	return len(value) > 0 && len(value) <= 200 && identityPattern.MatchString(value)
}

// SecretView is intentionally non-serializable. Bytes is valid only during
// the Resolver.Use callback. Implementations overwrite its backing storage as
// soon as the callback returns or panics; callers must not retain it.
type SecretView struct{ value []byte }

// NewView is for Resolver implementations. The supplied storage must be an
// invocation-local copy owned by that implementation.
func NewView(value []byte) SecretView { return SecretView{value: value} }

// Bytes returns the callback-scoped storage. It does not make a second copy so
// the Resolver can overwrite every byte after callback completion.
func (view SecretView) Bytes() []byte { return view.value }

type UseFunc func(SecretView) error

// Resolver exposes a secret only for the duration of callback. Implementations
// must fail closed and must not cache values.
type Resolver interface {
	Use(context.Context, ResolveRequest, UseFunc) error
}
