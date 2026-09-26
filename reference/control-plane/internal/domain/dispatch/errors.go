package dispatch

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

	ReasonInvalidRequest         ErrorReason = "invalid-dispatch-request"
	ReasonAuthenticationRequired ErrorReason = "authentication-required"
	ReasonDispatchForbidden      ErrorReason = "dispatch-forbidden"
	ReasonRunNotFound            ErrorReason = "run-not-found"
	ReasonRunNotDispatchable     ErrorReason = "run-not-dispatchable"
	ReasonIdempotencyConflict    ErrorReason = "idempotency-conflict"
	ReasonNoCapacity             ErrorReason = "no-dispatch-capacity"
	ReasonAttemptFenced          ErrorReason = "attempt-fenced"
	ReasonTicketExpired          ErrorReason = "ticket-expired"
	ReasonTicketInvalid          ErrorReason = "ticket-invalid"
	ReasonDependencyUnavailable  ErrorReason = "dependency-unavailable"
)

type Error struct {
	Category  ErrorCategory
	Reason    ErrorReason
	Retryable bool
}

func NewError(category ErrorCategory, reason ErrorReason) error {
	return Error{Category: category, Reason: reason, Retryable: category == CategoryDependency || category == CategoryCapacity || category == CategoryTimeout}
}

func (failure Error) Error() string { return string(failure.Reason) }

func AsError(err error) (Error, bool) {
	var failure Error
	return failure, errors.As(err, &failure)
}
