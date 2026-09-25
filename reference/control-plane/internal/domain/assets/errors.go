package assets

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
	CategoryNetwork        ErrorCategory = "network"
)

type ErrorReason string

const (
	ReasonInvalidRequest         ErrorReason = "invalid-asset-request"
	ReasonAuthenticationRequired ErrorReason = "authentication-required"
	ReasonGrantForbidden         ErrorReason = "run-grant-forbidden"
	ReasonBindingMismatch        ErrorReason = "asset-binding-mismatch"
	ReasonGrantExpired           ErrorReason = "asset-grant-expired"
	ReasonGrantNotYetValid       ErrorReason = "asset-grant-not-yet-valid"
	ReasonGrantRevoked           ErrorReason = "asset-grant-revoked"
	ReasonGrantUsesExhausted     ErrorReason = "asset-grant-uses-exhausted"
	ReasonNotReady               ErrorReason = "asset-not-ready"
	ReasonDigestMismatch         ErrorReason = "asset-digest-mismatch"
	ReasonNotFound               ErrorReason = "asset-not-found"
	ReasonIdempotencyConflict    ErrorReason = "idempotency-conflict"
	ReasonAssetTooLarge          ErrorReason = "asset-too-large"
	ReasonNetworkDenied          ErrorReason = "asset-network-denied"
	ReasonDependencyUnavailable  ErrorReason = "dependency-unavailable"
)

// Error is an internal typed failure. Error returns only the stable reason.
// It has no Unwrap method and stores no cause, URL, credential, or token.
type Error struct {
	Category  ErrorCategory
	Reason    ErrorReason
	Retryable bool
}

// NewError discards optional causes so callers cannot recover them.
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
		return reason == ReasonInvalidRequest || reason == ReasonBindingMismatch || reason == ReasonDigestMismatch
	case CategoryAuthentication:
		return reason == ReasonAuthenticationRequired
	case CategoryAuthorization:
		return reason == ReasonGrantForbidden || reason == ReasonGrantExpired || reason == ReasonGrantNotYetValid || reason == ReasonGrantRevoked || reason == ReasonGrantUsesExhausted || reason == ReasonNotReady
	case CategoryNotFound:
		return reason == ReasonNotFound
	case CategoryConflict:
		return reason == ReasonIdempotencyConflict
	case CategoryCapacity:
		return reason == ReasonAssetTooLarge
	case CategoryNetwork:
		return reason == ReasonNetworkDenied
	case CategoryDependency:
		return reason == ReasonDependencyUnavailable
	default:
		return false
	}
}

func (category ErrorCategory) Validate() error {
	switch category {
	case CategoryValidation, CategoryAuthentication, CategoryAuthorization, CategoryNotFound, CategoryConflict, CategoryCapacity, CategoryNetwork, CategoryDependency:
		return nil
	default:
		return fmt.Errorf("unknown asset error category %q", category)
	}
}
