// Package observability defines the reference Control Plane's audit and trace
// storage ports. The values are deliberately narrow: request bodies, headers,
// URLs, error strings, and arbitrary attributes cannot cross this boundary.
package observability

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeRejected  Outcome = "rejected"
	OutcomeFailed    Outcome = "failed"
)

type SpanStatus string

const (
	SpanStatusOK    SpanStatus = "ok"
	SpanStatusError SpanStatus = "error"
)

var (
	uuidV7Pattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	traceIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	operationPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
)

type AuditEntry struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	RequestID  string    `json:"request_id"`
	TraceID    string    `json:"trace_id"`
	Operation  string    `json:"operation"`
	Outcome    Outcome   `json:"outcome"`
	HTTPStatus int       `json:"http_status"`
}

func (entry AuditEntry) Validate() error {
	if !prefixedUUID("aud_", entry.ID) {
		return errors.New("invalid audit identifier")
	}
	if entry.OccurredAt.IsZero() || entry.OccurredAt.Location() != time.UTC {
		return errors.New("audit time must be non-zero UTC")
	}
	if !prefixedUUID("req_", entry.RequestID) {
		return errors.New("invalid request identifier")
	}
	if !traceIDPattern.MatchString(entry.TraceID) || allZero(entry.TraceID) {
		return errors.New("invalid trace identifier")
	}
	if !operationPattern.MatchString(entry.Operation) || len(entry.Operation) > 100 {
		return errors.New("invalid audit operation")
	}
	if entry.Outcome != OutcomeSucceeded && entry.Outcome != OutcomeRejected && entry.Outcome != OutcomeFailed {
		return errors.New("invalid audit outcome")
	}
	if entry.HTTPStatus < 100 || entry.HTTPStatus > 599 {
		return errors.New("invalid HTTP status")
	}
	return nil
}

type SpanRecord struct {
	TraceID      string     `json:"trace_id"`
	SpanID       string     `json:"span_id"`
	ParentSpanID string     `json:"parent_span_id,omitempty"`
	RequestID    string     `json:"request_id"`
	Operation    string     `json:"operation"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      time.Time  `json:"ended_at"`
	Status       SpanStatus `json:"status"`
}

func (span SpanRecord) Validate() error {
	if !traceIDPattern.MatchString(span.TraceID) || allZero(span.TraceID) {
		return errors.New("invalid trace identifier")
	}
	if !spanIDPattern.MatchString(span.SpanID) || allZero(span.SpanID) {
		return errors.New("invalid span identifier")
	}
	if span.ParentSpanID != "" && (!spanIDPattern.MatchString(span.ParentSpanID) || allZero(span.ParentSpanID)) {
		return errors.New("invalid parent span identifier")
	}
	if !prefixedUUID("req_", span.RequestID) {
		return errors.New("invalid request identifier")
	}
	if !operationPattern.MatchString(span.Operation) || len(span.Operation) > 100 {
		return errors.New("invalid trace operation")
	}
	if span.StartedAt.IsZero() || span.EndedAt.IsZero() || span.StartedAt.Location() != time.UTC || span.EndedAt.Location() != time.UTC {
		return errors.New("trace times must be non-zero UTC")
	}
	if span.EndedAt.Before(span.StartedAt) {
		return errors.New("trace end precedes start")
	}
	if span.Status != SpanStatusOK && span.Status != SpanStatusError {
		return errors.New("invalid trace status")
	}
	return nil
}

// ValidateObservationPair checks the correlations that make an audit entry
// and span one indivisible observation.
func ValidateObservationPair(audit AuditEntry, span SpanRecord) error {
	if err := audit.Validate(); err != nil {
		return err
	}
	if err := span.Validate(); err != nil {
		return err
	}
	if audit.RequestID != span.RequestID || audit.TraceID != span.TraceID || audit.Operation != span.Operation {
		return errors.New("audit and trace correlation mismatch")
	}
	expectedOutcome, expectedStatus := OutcomeFailed, SpanStatusError
	if audit.HTTPStatus < 400 {
		expectedOutcome, expectedStatus = OutcomeSucceeded, SpanStatusOK
	} else if audit.HTTPStatus < 500 {
		expectedOutcome = OutcomeRejected
	}
	if audit.Outcome != expectedOutcome || span.Status != expectedStatus {
		return errors.New("HTTP status, audit outcome, and trace status mismatch")
	}
	if !audit.OccurredAt.Equal(span.EndedAt) {
		return errors.New("audit occurrence and trace end time mismatch")
	}
	return nil
}

type AuditQuery struct {
	RequestID string
	TraceID   string
	Operation string
	Outcome   Outcome
	Limit     int
}

func (query AuditQuery) Validate() error {
	if query.RequestID != "" && !prefixedUUID("req_", query.RequestID) {
		return errors.New("invalid request identifier filter")
	}
	if query.TraceID != "" && (!traceIDPattern.MatchString(query.TraceID) || allZero(query.TraceID)) {
		return errors.New("invalid trace identifier filter")
	}
	if query.Operation != "" && (!operationPattern.MatchString(query.Operation) || len(query.Operation) > 100) {
		return errors.New("invalid operation filter")
	}
	if query.Outcome != "" && query.Outcome != OutcomeSucceeded && query.Outcome != OutcomeRejected && query.Outcome != OutcomeFailed {
		return errors.New("invalid outcome filter")
	}
	return validateLimit(query.Limit)
}

type TraceQuery struct {
	RequestID string
	TraceID   string
	Operation string
	Limit     int
}

func (query TraceQuery) Validate() error {
	if query.RequestID != "" && !prefixedUUID("req_", query.RequestID) {
		return errors.New("invalid request identifier filter")
	}
	if query.TraceID != "" && (!traceIDPattern.MatchString(query.TraceID) || allZero(query.TraceID)) {
		return errors.New("invalid trace identifier filter")
	}
	if query.Operation != "" && (!operationPattern.MatchString(query.Operation) || len(query.Operation) > 100) {
		return errors.New("invalid operation filter")
	}
	return validateLimit(query.Limit)
}

func validateLimit(limit int) error {
	if limit < 0 || limit > 1000 {
		return fmt.Errorf("query limit must be within 0..1000")
	}
	return nil
}

type AuditReader interface {
	QueryAudit(context.Context, AuditQuery) ([]AuditEntry, error)
	AuditHealth(context.Context) error
}

type TraceReader interface {
	QueryTrace(context.Context, TraceQuery) ([]SpanRecord, error)
	TraceHealth(context.Context) error
}

// ObservationWriter persists the audit entry and span as one indivisible
// observation. Implementations must validate the pair before publishing either.
type ObservationWriter interface {
	AppendObservation(context.Context, AuditEntry, SpanRecord) error
}

type Store interface {
	ObservationWriter
	AuditReader
	TraceReader
}

func prefixedUUID(prefix, value string) bool {
	return len(value) == len(prefix)+36 && value[:len(prefix)] == prefix && uuidV7Pattern.MatchString(value[len(prefix):])
}

func allZero(value string) bool {
	for _, character := range value {
		if character != '0' {
			return false
		}
	}
	return true
}
