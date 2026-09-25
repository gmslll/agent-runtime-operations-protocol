package publication

import (
	"errors"
	"fmt"
)

type ErrorCategory string

const (
	CategoryValidation     ErrorCategory = "validation"
	CategoryAuthentication ErrorCategory = "authentication"
	CategoryAuthorization  ErrorCategory = "authorization"
	CategoryNotFound       ErrorCategory = "not_found"
	CategoryConflict       ErrorCategory = "conflict"
	CategoryCapacity       ErrorCategory = "capacity"
	CategoryDependency     ErrorCategory = "dependency"
)

type ErrorReason string

const (
	ReasonInvalidRequest         ErrorReason = "invalid-publication-request"
	ReasonAuthenticationRequired ErrorReason = "authentication-required"
	ReasonPublicationForbidden   ErrorReason = "publication-forbidden"
	ReasonAgentIDMismatch        ErrorReason = "agent-id-mismatch"
	ReasonBundleInvalid          ErrorReason = "bundle-invalid"
	ReasonBundleTooLarge         ErrorReason = "bundle-too-large"
	ReasonReferenceDenied        ErrorReason = "offline-reference-denied"
	ReasonAllowedHostDenied      ErrorReason = "allowed-host-denied"
	ReasonNotFound               ErrorReason = "agent-version-not-found"
	ReasonImmutableConflict      ErrorReason = "agent-version-conflict"
	ReasonIdempotencyConflict    ErrorReason = "idempotency-conflict"
	ReasonDependencyUnavailable  ErrorReason = "dependency-unavailable"
)

// Error is an internal typed failure. Error intentionally returns only the
// stable reason, never database, archive, path, URL, or credential text. It
// intentionally has no Unwrap method or stored cause.
type Error struct {
	Category  ErrorCategory
	Reason    ErrorReason
	Retryable bool
}

// NewError accepts optional internal causes only to make redaction explicit:
// they are deliberately discarded and cannot be recovered with errors.Is/As.
func NewError(category ErrorCategory, reason ErrorReason, _ ...error) error {
	if !validErrorPair(category, reason) {
		return Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true}
	}
	return Error{Category: category, Reason: reason, Retryable: category == CategoryDependency}
}

func (failure Error) Error() string { return string(failure.Reason) }

func AsError(err error) (Error, bool) {
	var failure Error
	if !errors.As(err, &failure) {
		return Error{}, false
	}
	return failure, true
}

func validErrorPair(category ErrorCategory, reason ErrorReason) bool {
	switch category {
	case CategoryValidation:
		return reason == ReasonInvalidRequest || reason == ReasonAgentIDMismatch || reason == ReasonBundleInvalid || reason == ReasonReferenceDenied || reason == ReasonAllowedHostDenied
	case CategoryAuthentication:
		return reason == ReasonAuthenticationRequired
	case CategoryAuthorization:
		return reason == ReasonPublicationForbidden
	case CategoryNotFound:
		return reason == ReasonNotFound
	case CategoryConflict:
		return reason == ReasonImmutableConflict || reason == ReasonIdempotencyConflict
	case CategoryCapacity:
		return reason == ReasonBundleTooLarge
	case CategoryDependency:
		return reason == ReasonDependencyUnavailable
	default:
		return false
	}
}

func (category ErrorCategory) Validate() error {
	switch category {
	case CategoryValidation, CategoryAuthentication, CategoryAuthorization, CategoryNotFound, CategoryConflict, CategoryCapacity, CategoryDependency:
		return nil
	default:
		return fmt.Errorf("unknown publication error category %q", category)
	}
}
