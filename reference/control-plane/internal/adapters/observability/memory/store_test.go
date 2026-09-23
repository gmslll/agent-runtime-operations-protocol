package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

func TestStoreIsBoundedQueryableAndRedacted(t *testing.T) {
	t.Parallel()
	store, err := New(2, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	baseTime := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	requestIDs := []string{
		"req_01956e7b-9abc-7def-8abc-0123456789ab",
		"req_01956e7b-9abd-7def-8abc-0123456789ab",
		"req_01956e7b-9abe-7def-8abc-0123456789ab",
	}
	for index, requestID := range requestIDs {
		audit := observability.AuditEntry{
			ID:         "aud_01956e7b-9ab" + string(rune('c'+index)) + "-7def-8abc-0123456789ab",
			OccurredAt: baseTime.Add(time.Duration(index) * time.Second), RequestID: requestID,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Operation: "health.live",
			Outcome: observability.OutcomeSucceeded, HTTPStatus: 200,
		}
		span := observability.SpanRecord{
			TraceID: audit.TraceID, SpanID: []string{"00f067aa0ba902b1", "00f067aa0ba902b2", "00f067aa0ba902b3"}[index],
			RequestID: requestID, Operation: audit.Operation, StartedAt: audit.OccurredAt,
			EndedAt: audit.OccurredAt, Status: observability.SpanStatusOK,
		}
		if err := store.AppendObservation(ctx, audit, span); err != nil {
			t.Fatal(err)
		}
	}
	audits, err := store.QueryAudit(ctx, observability.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 || audits[0].RequestID != requestIDs[1] || audits[1].RequestID != requestIDs[2] {
		t.Fatalf("bounded audit order = %#v", audits)
	}
	audits[0].Operation = "mutated"
	again, err := store.QueryAudit(ctx, observability.AuditQuery{RequestID: requestIDs[1]})
	if err != nil || len(again) != 1 || again[0].Operation != "health.live" {
		t.Fatalf("store did not defensively copy query result: %#v, %v", again, err)
	}
	if store.AuditHealth(ctx) == nil || store.TraceHealth(ctx) == nil {
		t.Fatal("eviction was not made visible through health")
	}
	t.Run("paired-eviction-keeps-observation-aligned", func(t *testing.T) {
		if _, unequalErr := New(1, 2); unequalErr == nil {
			t.Fatal("unequal observation capacities were accepted")
		}
		spans, queryErr := store.QueryTrace(ctx, observability.TraceQuery{})
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		if len(spans) != len(audits) || len(spans) != 2 {
			t.Fatalf("paired retention lengths diverged: audits=%d traces=%d", len(audits), len(spans))
		}
		for index := range audits {
			if audits[index].RequestID != spans[index].RequestID {
				t.Fatalf("paired retention diverged at %d: audit=%s trace=%s", index, audits[index].RequestID, spans[index].RequestID)
			}
		}
		if store.AuditHealth(ctx) == nil || store.TraceHealth(ctx) == nil {
			t.Fatal("paired eviction did not degrade both health surfaces")
		}
	})
	t.Run("atomic-observation-rejects-partial-publish", func(t *testing.T) {
		validAudit := observability.AuditEntry{
			ID: "aud_01956e7b-9abf-7def-8abc-0123456789ab", OccurredAt: baseTime,
			RequestID: "req_01956e7b-9abf-7def-8abc-0123456789ab",
			TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736", Operation: "health.live",
			Outcome: observability.OutcomeSucceeded, HTTPStatus: 200,
		}
		invalidSpan := observability.SpanRecord{TraceID: validAudit.TraceID}
		if err := store.AppendObservation(ctx, validAudit, invalidSpan); err == nil {
			t.Fatal("invalid second observation value was accepted")
		}
		partialAudits, queryErr := store.QueryAudit(ctx, observability.AuditQuery{RequestID: validAudit.RequestID})
		if queryErr != nil || len(partialAudits) != 0 {
			t.Fatalf("failed atomic append published audit: %#v, %v", partialAudits, queryErr)
		}
		partialSpans, queryErr := store.QueryTrace(ctx, observability.TraceQuery{RequestID: validAudit.RequestID})
		if queryErr != nil || len(partialSpans) != 0 {
			t.Fatalf("failed atomic append published span: %#v, %v", partialSpans, queryErr)
		}
	})
	t.Run("mismatched-observation-pair-rejected-without-publish", func(t *testing.T) {
		baseAudit := observability.AuditEntry{
			ID: "aud_01956e7b-9abf-7def-8abc-0123456789ab", OccurredAt: baseTime,
			RequestID: "req_01956e7b-9abf-7def-8abc-0123456789ab",
			TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736", Operation: "health.live",
			Outcome: observability.OutcomeSucceeded, HTTPStatus: 200,
		}
		baseSpan := observability.SpanRecord{
			TraceID: baseAudit.TraceID, SpanID: "00f067aa0ba902bf", RequestID: baseAudit.RequestID,
			Operation: baseAudit.Operation, StartedAt: baseTime, EndedAt: baseTime, Status: observability.SpanStatusOK,
		}
		for _, mismatch := range []string{"request-id", "trace-id", "operation", "outcome-status", "http-status-outcome", "occurred-at"} {
			t.Run(mismatch, func(t *testing.T) {
				audit, span := baseAudit, baseSpan
				switch mismatch {
				case "request-id":
					span.RequestID = "req_01956e7b-9ac0-7def-8abc-0123456789ab"
				case "trace-id":
					span.TraceID = "5bf92f3577b34da6a3ce929d0e0e4736"
				case "operation":
					span.Operation = "health.ready"
				case "outcome-status":
					span.Status = observability.SpanStatusError
				case "http-status-outcome":
					audit.HTTPStatus = 500
				case "occurred-at":
					span.EndedAt = baseTime.Add(time.Second)
				}
				isolated, newErr := New(2, 2)
				if newErr != nil {
					t.Fatal(newErr)
				}
				if appendErr := isolated.AppendObservation(ctx, audit, span); appendErr == nil {
					t.Fatalf("%s mismatch was accepted", mismatch)
				}
				auditsAfter, auditErr := isolated.QueryAudit(ctx, observability.AuditQuery{})
				spansAfter, traceErr := isolated.QueryTrace(ctx, observability.TraceQuery{})
				if auditErr != nil || traceErr != nil || len(auditsAfter) != 0 || len(spansAfter) != 0 {
					t.Fatalf("%s mismatch partially published: audits=%#v spans=%#v errors=%v/%v", mismatch, auditsAfter, spansAfter, auditErr, traceErr)
				}
			})
		}
	})
	encoded, err := json.Marshal(struct {
		Audits []observability.AuditEntry
	}{again})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"p08-secret-value", "p08-bearer-value", "p08-cookie-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("memory store leaked %q", secret)
		}
	}
}
