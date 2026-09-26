package run

import "errors"

type ErrorCategory string
type ErrorReason string

const (
	CategoryValidation     ErrorCategory = "validation"
	CategoryAuthentication ErrorCategory = "authentication"
	CategoryAuthorization  ErrorCategory = "authorization"
	CategoryNotFound       ErrorCategory = "not_found"
	CategoryConflict       ErrorCategory = "conflict"
	CategoryCapacity       ErrorCategory = "capacity"
	CategoryTimeout        ErrorCategory = "timeout"
	CategoryDependency     ErrorCategory = "dependency"

	ReasonInvalidRequest         ErrorReason = "invalid-run-request"
	ReasonAuthenticationRequired ErrorReason = "authentication-required"
	ReasonRunForbidden           ErrorReason = "run-forbidden"
	ReasonRunNotFound            ErrorReason = "run-not-found"
	ReasonIdempotencyConflict    ErrorReason = "idempotency-conflict"
	ReasonStateVersionConflict   ErrorReason = "state-version-conflict"
	ReasonTerminalStateConflict  ErrorReason = "terminal-state-conflict"
	ReasonEffectConflict         ErrorReason = "effect-conflict"
	ReasonDeadlineExceeded       ErrorReason = "deadline-exceeded"
	ReasonRunRequestTooLarge     ErrorReason = "run-request-too-large"
	ReasonDependencyUnavailable  ErrorReason = "dependency-unavailable"
)

// Error contains only stable public classification. Internal causes are
// intentionally discarded and cannot escape through Error or Unwrap.
type Error struct {
	Category  ErrorCategory
	Reason    ErrorReason
	Retryable bool
}

func NewError(category ErrorCategory, reason ErrorReason, _ ...error) error {
	retryable := category == CategoryDependency
	return Error{Category: category, Reason: reason, Retryable: retryable}
}

func (failure Error) Error() string { return string(failure.Reason) }

func AsError(err error) (Error, bool) {
	var failure Error
	return failure, errors.As(err, &failure)
}
