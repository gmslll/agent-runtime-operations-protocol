package registry

import "errors"

type ErrorReason string

const (
	ReasonInvalidRequest        ErrorReason = "REGISTRY_INVALID_REQUEST"
	ReasonNotFound              ErrorReason = "REGISTRY_INSTANCE_NOT_FOUND"
	ReasonSessionReused         ErrorReason = "REGISTRY_SESSION_REUSED"
	ReasonGenerationFenced      ErrorReason = "INSTANCE_GENERATION_FENCED"
	ReasonLeaseExpired          ErrorReason = "REGISTRY_LEASE_EXPIRED"
	ReasonHeartbeatStale        ErrorReason = "REGISTRY_HEARTBEAT_STALE"
	ReasonResourceConflict      ErrorReason = "RESOURCE_VERSION_CONFLICT"
	ReasonIdempotencyConflict   ErrorReason = "IDEMPOTENCY_KEY_CONFLICT"
	ReasonRevisionOverflow      ErrorReason = "REGISTRY_REVISION_OVERFLOW"
	ReasonResourceOverflow      ErrorReason = "REGISTRY_RESOURCE_VERSION_OVERFLOW"
	ReasonGenerationOverflow    ErrorReason = "REGISTRY_GENERATION_OVERFLOW"
	ReasonDependencyUnavailable ErrorReason = "REGISTRY_DEPENDENCY_UNAVAILABLE"
)

type Error struct{ Reason ErrorReason }

func (failure Error) Error() string { return "registry: " + string(failure.Reason) }

func NewError(reason ErrorReason) error { return Error{Reason: reason} }

func HasReason(err error, reason ErrorReason) bool {
	var failure Error
	return errors.As(err, &failure) && failure.Reason == reason
}
