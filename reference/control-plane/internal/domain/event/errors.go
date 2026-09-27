package event

import "errors"

type Category string
type Reason string

const (
	CategoryValidation     Category = "validation"
	CategoryAuthentication Category = "authentication"
	CategoryAuthorization  Category = "authorization"
	CategoryNotFound       Category = "not_found"
	CategoryConflict       Category = "conflict"
	CategoryCapacity       Category = "capacity"
	CategoryTimeout        Category = "timeout"
	CategoryDependency     Category = "dependency"
)

const (
	ReasonInvalidRequest        Reason = "invalid-request"
	ReasonAuthentication        Reason = "authentication-required"
	ReasonAuthorization         Reason = "authorization-denied"
	ReasonAttemptNotFound       Reason = "attempt-not-found"
	ReasonAttemptFenced         Reason = "attempt-fenced"
	ReasonSessionExpired        Reason = "event-session-expired"
	ReasonEventIDConflict       Reason = "event-id-conflict"
	ReasonProducerSequence      Reason = "producer-sequence-conflict"
	ReasonIdempotencyConflict   Reason = "idempotency-conflict"
	ReasonBatchTooLarge         Reason = "event-batch-too-large"
	ReasonDependencyUnavailable Reason = "dependency-unavailable"
)

type Error struct {
	Category  Category
	Reason    Reason
	Retryable bool
}

func (failure Error) Error() string { return string(failure.Category) + ": " + string(failure.Reason) }

func NewError(category Category, reason Reason) error {
	return Error{Category: category, Reason: reason, Retryable: category == CategoryDependency || category == CategoryTimeout}
}

func AsError(err error) (Error, bool) {
	var typed Error
	return typed, errors.As(err, &typed)
}
