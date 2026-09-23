package observability

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestObservabilityValuesValidateAndRedact(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	audit := AuditEntry{
		ID: "aud_01956e7b-9abc-7def-8abc-0123456789ab", OccurredAt: now,
		RequestID: "req_01956e7b-9abc-7def-8abc-0123456789ab",
		TraceID:   "4bf92f3577b34da6a3ce929d0e0e4736", Operation: "health.live",
		Outcome: OutcomeSucceeded, HTTPStatus: 200,
	}
	if err := audit.Validate(); err != nil {
		t.Fatalf("valid audit rejected: %v", err)
	}
	span := SpanRecord{
		TraceID: audit.TraceID, SpanID: "00f067aa0ba902b7", RequestID: audit.RequestID,
		Operation: audit.Operation, StartedAt: now, EndedAt: now, Status: SpanStatusOK,
	}
	if err := span.Validate(); err != nil {
		t.Fatalf("valid span rejected: %v", err)
	}
	encoded, err := json.Marshal(struct {
		Audit AuditEntry
		Span  SpanRecord
	}{audit, span})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authorization", "cookie", "body", "raw_url", "error_message", "secret", "p08-secret-value", "p08-bearer-value", "p08-cookie-value"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("typed observability value exposed forbidden field %q: %s", forbidden, encoded)
		}
	}

	bad := audit
	bad.Operation = "health/live?token=secret"
	if err := bad.Validate(); err == nil {
		t.Fatal("unsafe free-form operation was accepted")
	}
	bad = audit
	bad.OccurredAt = now.Local()
	if err := bad.Validate(); err == nil {
		t.Fatal("non-UTC audit time was accepted")
	}
	if err := (AuditQuery{Limit: 1001}).Validate(); err == nil {
		t.Fatal("oversized query was accepted")
	}
}
