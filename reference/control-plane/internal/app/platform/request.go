package platform

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type RequestMetadata struct {
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string
	TraceFlags   string
}

func (metadata RequestMetadata) Traceparent() string {
	if metadata.TraceID == "" || metadata.SpanID == "" || metadata.TraceFlags == "" {
		return ""
	}
	return "00-" + metadata.TraceID + "-" + metadata.SpanID + "-" + metadata.TraceFlags
}

type Observation struct {
	Metadata   RequestMetadata
	Operation  string
	StartedAt  time.Time
	EndedAt    time.Time
	HTTPStatus int
}

func (platform *Platform) BeginRequest(ctx context.Context, requestIDs, traceparents, tracestates []string) (RequestMetadata, error) {
	requestID, requestErr := platform.NewID(ctx, platformports.IDRequest)
	traceID, traceErr := platform.NewID(ctx, platformports.IDTrace)
	spanID, spanErr := platform.NewID(ctx, platformports.IDSpan)
	metadata := RequestMetadata{RequestID: requestID, TraceID: traceID, SpanID: spanID, TraceFlags: "00"}
	if requestErr != nil || traceErr != nil || spanErr != nil {
		return metadata, errors.Join(requestErr, traceErr, spanErr)
	}
	if len(requestIDs) > 1 || len(requestIDs) == 1 && (!strings.HasPrefix(requestIDs[0], "req_") || !uuidV7Pattern.MatchString(strings.TrimPrefix(requestIDs[0], "req_"))) {
		return metadata, errors.New("invalid request identifier")
	}
	if len(traceparents) > 1 || len(tracestates) > 1 {
		return metadata, errors.New("trace context headers must be singular")
	}
	if len(traceparents) == 0 {
		if len(tracestates) != 0 {
			return metadata, errors.New("tracestate requires traceparent")
		}
		return metadata, nil
	}
	state := ""
	if len(tracestates) == 1 {
		state = tracestates[0]
	}
	traceContext := protocolcore.TraceContext{Traceparent: traceparents[0], Tracestate: state}
	if err := traceContext.Validate(); err != nil {
		return metadata, err
	}
	parts := strings.SplitN(traceContext.Traceparent, "-", 5)
	metadata.TraceID, metadata.ParentSpanID, metadata.TraceFlags = parts[1], parts[2], parts[3]
	return metadata, nil
}

func (platform *Platform) Execute(ctx context.Context, callback func(context.Context) error) error {
	if callback == nil {
		return errors.New("request callback is required")
	}
	if err := platform.CheckFault(ctx, platformports.CheckpointRequestAccepted); err != nil {
		return err
	}
	return platform.Within(ctx, func(transactionContext context.Context) error {
		if err := platform.CheckFault(transactionContext, platformports.CheckpointBeforeUseCase); err != nil {
			return err
		}
		if err := callback(transactionContext); err != nil {
			return err
		}
		return platform.CheckFault(transactionContext, platformports.CheckpointAfterUseCase)
	})
}

func (platform *Platform) RecordObservation(ctx context.Context, observation Observation) error {
	auditID, err := platform.NewID(ctx, platformports.IDAudit)
	if err != nil {
		return err
	}
	outcome, spanStatus := observability.OutcomeFailed, observability.SpanStatusError
	if observation.HTTPStatus < 400 {
		outcome, spanStatus = observability.OutcomeSucceeded, observability.SpanStatusOK
	} else if observation.HTTPStatus < 500 {
		outcome = observability.OutcomeRejected
	}
	return platform.deps.Observability.AppendObservation(ctx,
		observability.AuditEntry{ID: auditID, OccurredAt: observation.EndedAt, RequestID: observation.Metadata.RequestID, TraceID: observation.Metadata.TraceID, Operation: observation.Operation, Outcome: outcome, HTTPStatus: observation.HTTPStatus},
		observability.SpanRecord{TraceID: observation.Metadata.TraceID, SpanID: observation.Metadata.SpanID, ParentSpanID: observation.Metadata.ParentSpanID, RequestID: observation.Metadata.RequestID, Operation: observation.Operation, StartedAt: observation.StartedAt, EndedAt: observation.EndedAt, Status: spanStatus},
	)
}
