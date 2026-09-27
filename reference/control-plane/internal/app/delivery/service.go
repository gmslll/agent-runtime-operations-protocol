// Package delivery implements the Control Plane Proxy transport profile.
package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

// Forwarder performs exactly one provider delivery using the validated durable
// Attempt endpoint. Implementations must re-resolve and classify every network
// connection and reject redirects without replaying credentials.
type Forwarder interface {
	Deliver(context.Context, dispatch.Attempt, []byte, string) ([]byte, error)
}

type TicketValidator interface {
	ValidateProxyDelivery(context.Context, dispatch.Caller, string, string, string) (dispatch.Attempt, dispatch.RunView, error)
}

type Service struct {
	dispatcher TicketValidator
	forwarder  Forwarder
	clock      func() time.Time
}

func New(dispatcher TicketValidator, forwarder Forwarder, clock func() time.Time) (*Service, error) {
	if dispatcher == nil || forwarder == nil || clock == nil {
		return nil, errors.New("delivery dependencies are required")
	}
	now := clock()
	if now.IsZero() || !now.Equal(now.UTC()) {
		return nil, errors.New("delivery clock must return UTC")
	}
	return &Service{dispatcher: dispatcher, forwarder: forwarder, clock: clock}, nil
}

type Request struct {
	Caller                     dispatch.Caller
	RunID, AttemptID, RunToken string
	Body                       []byte
}

func (service *Service) Deliver(ctx context.Context, request Request) (runwire.RunStatus, error) {
	attempt, view, err := service.dispatcher.ValidateProxyDelivery(ctx, request.Caller, request.RunID, request.AttemptID, request.RunToken)
	if err != nil {
		return runwire.RunStatus{}, err
	}
	if !service.clock().Before(attempt.TicketExpiresAt) {
		return runwire.RunStatus{}, dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonTicketExpired)
	}
	wire, err := runwire.DecodeRunRequest(request.Body)
	if err != nil || string(wire.Agent.ID) != view.Agent.ID || string(wire.Agent.Version) != view.Agent.Version || string(wire.Agent.SkillID) != view.Agent.SkillID || string(wire.Agent.ManifestDigest) != view.Agent.ManifestDigest || wire.Trace.Traceparent != view.Traceparent || !sameOptional(wire.Trace.Tracestate, view.Tracestate) || string(wire.DeadlineAt) != view.DeadlineAt.Format(time.RFC3339Nano) {
		return runwire.RunStatus{}, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	normalized, err := runwire.EncodeRunRequest(wire)
	if err != nil {
		return runwire.RunStatus{}, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	response, err := service.forwarder.Deliver(ctx, attempt, normalized, request.RunToken)
	if err != nil {
		return runwire.RunStatus{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	status, err := runwire.DecodeRunStatus(response)
	if err != nil || string(status.RunID) != request.RunID || status.Agent.ID != wire.Agent.ID || status.Agent.Version != wire.Agent.Version || status.Agent.SkillID != wire.Agent.SkillID || status.Agent.ManifestDigest != wire.Agent.ManifestDigest {
		return runwire.RunStatus{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	return status, nil
}

func (service *Service) Name() string { return "direct-proxy-delivery-service" }

func (service *Service) Check(context.Context) error { return nil }

func sameOptional(value *string, expected string) bool {
	return value == nil && expected == "" || value != nil && *value == expected
}
