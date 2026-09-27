package streaming

import "errors"

type Category string

const (
	CategoryValidation     Category = "validation"
	CategoryAuthentication Category = "authentication"
	CategoryAuthorization  Category = "authorization"
	CategoryNotFound       Category = "not_found"
	CategoryCursor         Category = "cursor"
	CategoryDependency     Category = "dependency"
)

const (
	ReasonInvalidRequest        = "INVALID_REQUEST"
	ReasonAuthentication        = "AUTHENTICATION_REQUIRED"
	ReasonAuthorization         = "FORBIDDEN"
	ReasonRunNotFound           = "RUN_NOT_FOUND"
	ReasonCursorExpired         = "STREAM_CURSOR_EXPIRED"
	ReasonCursorAhead           = "STREAM_CURSOR_AHEAD"
	ReasonDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
)

// Error is deliberately cause-free so database, token and network details can
// never cross the public streaming boundary.
type Error struct {
	Category       Category
	Reason         string
	LatestSequence uint64
	SnapshotURL    string
}

func (value *Error) Error() string { return value.Reason }

func NewError(category Category, reason string) error {
	return &Error{Category: category, Reason: reason}
}

func CursorExpired(runID string, latest uint64) error {
	return &Error{Category: CategoryCursor, Reason: ReasonCursorExpired, LatestSequence: latest, SnapshotURL: "/v1/agent-runs/" + runID}
}

func AsError(err error) (*Error, bool) {
	var typed *Error
	return typed, errors.As(err, &typed)
}
