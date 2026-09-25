package publication

import (
	"errors"
	"fmt"
)

type ErrorCategory string

const (
	CategoryValidation ErrorCategory = "validation"
	CategoryNotFound   ErrorCategory = "not_found"
	CategoryConflict   ErrorCategory = "conflict"
	CategoryCapacity   ErrorCategory = "capacity"
	CategoryDependency ErrorCategory = "dependency"
)

type ErrorReason string

const (
	ReasonInvalidRequest        ErrorReason = "invalid-publication-request"
	ReasonAgentIDMismatch       ErrorReason = "agent-id-mismatch"
	ReasonBundleInvalid         ErrorReason = "bundle-invalid"
	ReasonBundleTooLarge        ErrorReason = "bundle-too-large"
	ReasonReferenceDenied       ErrorReason = "offline-reference-denied"
	ReasonAllowedHostDenied     ErrorReason = "allowed-host-denied"
	ReasonNotFound              ErrorReason = "agent-version-not-found"
	ReasonImmutableConflict     ErrorReason = "agent-version-conflict"
	ReasonIdempotencyConflict   ErrorReason = "idempotency-conflict"
	ReasonDependencyUnavailable ErrorReason = "dependency-unavailable"
)

// Error is an internal typed failure. Error intentionally returns only the
// stable reason, never the wrapped database, archive, path, URL, or credential
// text. Unwrap exists for internal classification, not public serialization.
type Error struct {
	Category  ErrorCategory
	Reason    ErrorReason
	Retryable bool
	cause     error
}

func NewError(category ErrorCategory, reason ErrorReason, cause error) error {
	if !validErrorPair(category, reason) {
		return Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable, Retryable: true, cause: errors.New("invalid publication error classification")}
	}
	return Error{Category: category, Reason: reason, Retryable: category == CategoryDependency, cause: cause}
}

func (failure Error) Error() string { return string(failure.Reason) }
func (failure Error) Unwrap() error { return failure.cause }

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
	case CategoryValidation, CategoryNotFound, CategoryConflict, CategoryCapacity, CategoryDependency:
		return nil
	default:
		return fmt.Errorf("unknown publication error category %q", category)
	}
}
