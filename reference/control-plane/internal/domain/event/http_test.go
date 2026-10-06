package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
)

var httpTestNow = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

type eventTestClock struct{ now time.Time }

func (clock eventTestClock) Now() time.Time { return clock.now }

type eventTestIDs struct{ fail bool }

func (ids eventTestIDs) NewAuditID(context.Context) (string, error) {
	if ids.fail {
		return "", errors.New("audit identifiers unavailable")
	}
	return "aud_01932f13-0cd2-7a82-8fa3-1cb5ce13ef18", nil
}

type eventTestTokens struct{ fail bool }

func (tokens eventTestTokens) NewEventToken(context.Context) (string, error) {
	if tokens.fail {
		return "", errors.New("entropy unavailable")
	}
	return "evtcap_" + strings.Repeat("A", 43), nil
}

type eventTxKey struct{}
type eventTestUoW struct{}

func (eventTestUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return callback(context.WithValue(ctx, eventTxKey{}, true))
}

type eventTestObservations struct {
	count int
	fail  bool
}

func (observations *eventTestObservations) AppendObservation(ctx context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if observations.fail || ctx.Value(eventTxKey{}) != true {
		return errors.New("observation unavailable")
	}
	if err := observability.ValidateObservationPair(audit, span); err != nil {
		return err
	}
	observations.count++
	return nil
}

type eventTestRepository struct {
	fail     error
	sessions int
	batches  int
}

func (repository *eventTestRepository) CreateSession(ctx context.Context, command SessionCommand) (Session, error) {
	if ctx.Value(eventTxKey{}) != true {
		return Session{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if repository.fail != nil {
		return Session{}, repository.fail
	}
	repository.sessions++
	return Session{
		TenantID: command.Request.TenantID, RunID: command.Request.RunID,
		AttemptID: command.Request.AttemptID, DeploymentID: command.Request.DeploymentID,
		InstanceID: "runtime-a", SessionID: "ses_01932f13-0cd2-7a82-8fa3-1cb5ce13ef14",
		Generation: command.Request.Generation, FencingToken: command.Request.FencingToken,
		Token: command.Token, TokenDigest: command.TokenDigest,
		CreatedAt: command.Now, ExpiresAt: command.ExpiresAt,
	}, nil
}

func (repository *eventTestRepository) Append(ctx context.Context, command AppendCommand) (Ack, error) {
	if ctx.Value(eventTxKey{}) != true {
		return Ack{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	if repository.fail != nil {
		return Ack{}, repository.fail
	}
	repository.batches++
	return Ack{AcceptedThroughProducerSequence: 1, AssignedRunSequence: 7, DuplicateEventIDs: []string{}, RunState: "running"}, nil
}

type eventTestMetadata struct{ tenant bool }

func (eventTestMetadata) Metadata(*http.Request) (platform.RequestMetadata, bool) {
	return platform.RequestMetadata{RequestID: "req_01932f13-0cd2-7a82-8fa3-1cb5ce13ef19", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceFlags: "01"}, true
}
func (metadata eventTestMetadata) TenantID(*http.Request) (string, bool) {
	return "tenant-a", metadata.tenant
}

func newEventHTTPTest(t *testing.T, repository *eventTestRepository, metadata eventTestMetadata) (http.Handler, *Service, *eventTestObservations) {
	t.Helper()
	observations := &eventTestObservations{}
	service, err := New(Dependencies{
		Clock: eventTestClock{httpTestNow}, IDs: eventTestIDs{}, Tokens: eventTestTokens{},
		UoW: eventTestUoW{}, Observability: observations, Repository: repository,
		ControlPlaneBaseURL: "https://control.example.invalid", SessionTTL: 2 * time.Minute,
		AttemptLeaseTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return handler, service, observations
}

func TestEventHTTPCreatesSessionAndAppendsBatch(t *testing.T) {
	repository := &eventTestRepository{}
	handler, _, observations := newEventHTTPTest(t, repository, eventTestMetadata{tenant: true})
	sessionRequest := eventwire.EventSessionRequest{
		AttemptID:    "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12",
		DeploymentID: "dep_01932f13-0cd2-7a82-8fa3-1cb5ce13ef13",
		Generation:   7, FencingToken: 1,
	}
	body, err := eventwire.EncodeEventSessionRequest(sessionRequest)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+testRunID+"/event-session", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("session response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	session, err := eventwire.DecodeEventSession(response.Body.Bytes())
	if err != nil || !strings.HasPrefix(session.EventToken, "evtcap_") || string(session.EventBatchURL) != "https://control.example.invalid/v1/agent-runs/"+testRunID+"/events:batch" {
		t.Fatalf("session=%#v err=%v", session, err)
	}

	eventBody := `{"batch_id":"batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef30","attempt_id":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","fencing_token":1,"events":[{"specversion":"1.0","id":"evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef21","source":"https://runtime.example.invalid/instances/runtime-a","type":"io.arop.run.started.v1","subject":"runs/run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","time":"2026-09-27T10:00:01Z","datacontenttype":"application/json","dataschema":"https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json","runid":"run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","attemptid":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","producersequence":1,"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","data":{"state":"started"}}]}`
	batch := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+testRunID+"/events:batch", strings.NewReader(eventBody))
	batch.Header.Set("Content-Type", "application/json")
	batch.Header.Set("Authorization", "Bearer "+session.EventToken)
	batch.Header.Set("Idempotency-Key", "event-batch-key-0001")
	batchResponse := httptest.NewRecorder()
	handler.ServeHTTP(batchResponse, batch)
	if batchResponse.Code != http.StatusOK || batchResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("batch response=%d headers=%v body=%s", batchResponse.Code, batchResponse.Header(), batchResponse.Body.String())
	}
	ack, err := eventwire.DecodeEventBatchAck(batchResponse.Body.Bytes())
	if err != nil || uint64(ack.AcceptedThroughProducerSequence) != 1 || uint64(ack.AssignedRunSequence) != 7 || ack.RunState != "running" {
		t.Fatalf("ack=%#v err=%v", ack, err)
	}
	if repository.sessions != 1 || repository.batches != 1 || observations.count != 2 {
		t.Fatalf("durable calls sessions=%d batches=%d observations=%d", repository.sessions, repository.batches, observations.count)
	}
}

func TestEventHTTPFailsClosedAtCapabilityAndWireBoundaries(t *testing.T) {
	repository := &eventTestRepository{}
	handler, _, _ := newEventHTTPTest(t, repository, eventTestMetadata{})
	tests := []struct {
		name, target, body string
		headers            http.Header
		status             int
	}{
		{name: "missing-runtime-identity", target: "/v1/agent-runs/" + testRunID + "/event-session", body: `{}`, headers: http.Header{"Content-Type": {"application/json"}}, status: 401},
		{name: "missing-event-capability", target: "/v1/agent-runs/" + testRunID + "/events:batch", body: `{}`, headers: http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {"event-key-0001"}}, status: 401},
		{name: "duplicate-event-capability", target: "/v1/agent-runs/" + testRunID + "/events:batch", body: `{}`, headers: http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer evtcap_" + strings.Repeat("A", 43), "Bearer evtcap_" + strings.Repeat("B", 43)}, "Idempotency-Key": {"event-key-0001"}}, status: 401},
		{name: "duplicate-idempotency", target: "/v1/agent-runs/" + testRunID + "/events:batch", body: `{}`, headers: http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer evtcap_" + strings.Repeat("A", 43)}, "Idempotency-Key": {"event-key-0001", "event-key-0002"}}, status: 400},
		{name: "non-json", target: "/v1/agent-runs/" + testRunID + "/events:batch", body: `{}`, headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, status: 400},
		{name: "oversize", target: "/v1/agent-runs/" + testRunID + "/events:batch", body: strings.Repeat("x", MaxBatchBytes+1), headers: http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer evtcap_" + strings.Repeat("A", 43)}, "Idempotency-Key": {"event-key-0001"}}, status: 413},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.body))
			request.Header = test.headers.Clone()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			var failure map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure["code"] == nil {
				t.Fatalf("untyped error: %s err=%v", response.Body.String(), err)
			}
			if test.status == 401 && response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("401 response omitted exact bearer challenge")
			}
		})
	}
	if repository.sessions != 0 || repository.batches != 0 {
		t.Fatal("invalid wire input reached the durable repository")
	}
}

func TestEventHTTPRejectsProducerAssignedRunSequenceAndFormatsDependencyError(t *testing.T) {
	repository := &eventTestRepository{}
	handler, _, _ := newEventHTTPTest(t, repository, eventTestMetadata{tenant: true})
	body := `{"batch_id":"batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef30","attempt_id":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","fencing_token":1,"events":[{"specversion":"1.0","id":"evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef21","source":"https://runtime.example.invalid/instances/runtime-a","type":"io.arop.run.started.v1","subject":"runs/run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","time":"2026-09-27T10:00:01Z","datacontenttype":"application/json","dataschema":"https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json","runid":"run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","attemptid":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","producersequence":1,"runsequence":9,"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","data":{"state":"started"}}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+testRunID+"/events:batch", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer evtcap_"+strings.Repeat("A", 43))
	request.Header.Set("Idempotency-Key", "event-batch-key-0001")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || repository.batches != 0 {
		t.Fatalf("producer run sequence accepted: status=%d body=%s", response.Code, response.Body.String())
	}

	repository.fail = NewError(CategoryDependency, ReasonDependencyUnavailable)
	sessionRequest := `{"attempt_id":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","deployment_id":"dep_01932f13-0cd2-7a82-8fa3-1cb5ce13ef13","generation":7,"fencing_token":1}`
	failed := httptest.NewRequest(http.MethodPost, "/v1/agent-runs/"+testRunID+"/event-session", strings.NewReader(sessionRequest))
	failed.Header.Set("Content-Type", "application/json")
	failedResponse := httptest.NewRecorder()
	handler.ServeHTTP(failedResponse, failed)
	if failedResponse.Code != 503 || failedResponse.Header().Get("Retry-After") != "1" || !strings.Contains(failedResponse.Body.String(), `"retry_after_seconds":1`) {
		t.Fatalf("dependency error contract: status=%d headers=%v body=%s", failedResponse.Code, failedResponse.Header(), failedResponse.Body.String())
	}
}
