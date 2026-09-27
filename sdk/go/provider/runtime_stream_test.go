package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

type streamVerifier struct {
	claims Claims
	scope  string
}

func (verifier *streamVerifier) Verify(_ context.Context, token string, request VerifyRequest) (Claims, error) {
	verifier.scope = request.RequiredScope
	if token != "secret-token-000000" {
		return Claims{}, errors.New("invalid token")
	}
	return verifier.claims, nil
}

func TestRuntimeDirectStreamReplayResumeAndScope(t *testing.T) {
	store := newMemoryStore()
	claims := validClaims()
	claims.Scopes = []string{"agent:invoke", "run:stream"}
	inbox := InboxRecord{RunID: claims.RunID, AttemptID: claims.AttemptID, InstanceID: claims.InstanceID, Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", State: InboxSucceeded}
	store.inbox[inbox.RunID+"/"+inbox.AttemptID] = inbox
	for sequence, state := range []string{"accepted", "started", "succeeded"} {
		record := lifecycleEvent(inbox, "arop.run."+state, runtimeNow.Add(time.Duration(sequence)*time.Second))
		record.Sequence = uint64(sequence + 1)
		store.outbox[inbox.RunID+"/"+inbox.AttemptID] = append(store.outbox[inbox.RunID+"/"+inbox.AttemptID], record)
	}
	verifier := &streamVerifier{claims: claims}
	runtime, err := NewRuntime(Config{Store: store, Verifier: verifier, Handler: HandlerFunc(func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error) {
		return runwire.RunResult{}, nil
	}), Clock: func() time.Time { return runtimeNow }, StreamPollInterval: 10 * time.Millisecond, StreamHeartbeatInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	response := streamRequest(runtime, "")
	if response.Code != http.StatusOK || verifier.scope != "run:stream" || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d scope=%s headers=%v body=%s", response.Code, verifier.scope, response.Header(), response.Body.String())
	}
	events := streamData(t, response.Body.String())
	if len(events) != 3 {
		t.Fatalf("events=%d body=%s", len(events), response.Body.String())
	}
	for index, event := range events {
		if uint64(event.Producersequence) != uint64(index+1) || event.Runsequence != nil || string(event.Runid) != claims.RunID || string(event.Attemptid) != claims.AttemptID {
			t.Fatalf("event[%d]=%#v", index, event)
		}
	}
	response = streamRequest(runtime, "2")
	if response.Code != http.StatusOK || strings.Count(response.Body.String(), "data: ") != 1 || !strings.Contains(response.Body.String(), "id: 3\n") {
		t.Fatalf("resume status=%d body=%s", response.Code, response.Body.String())
	}
	response = streamRequest(runtime, "4")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "STREAM_CURSOR_AHEAD") {
		t.Fatalf("ahead status=%d body=%s", response.Code, response.Body.String())
	}
}

func streamRequest(runtime *Runtime, after string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+validClaims().RunID+"/events", nil)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer secret-token-000000")
	if after != "" {
		request.Header.Set("Last-Event-ID", after)
	}
	response := httptest.NewRecorder()
	runtime.ServeHTTP(response, request)
	return response
}

func streamData(t *testing.T, wire string) []streamwire.StreamEvent {
	t.Helper()
	var events []streamwire.StreamEvent
	for _, block := range strings.Split(wire, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				value, err := streamwire.DecodeStreamEvent([]byte(strings.TrimPrefix(line, "data: ")))
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, value)
			}
		}
	}
	return events
}

func TestRuntimeDirectStreamRejectsAmbiguousHeadersAndExpiredCursor(t *testing.T) {
	store := newMemoryStore()
	claims := validClaims()
	claims.Scopes = []string{"agent:invoke", "run:stream"}
	inbox := InboxRecord{RunID: claims.RunID, AttemptID: claims.AttemptID, InstanceID: claims.InstanceID, Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", State: InboxSucceeded}
	store.inbox[inbox.RunID+"/"+inbox.AttemptID] = inbox
	for sequence := uint64(7); sequence <= 8; sequence++ {
		data, _ := json.Marshal(map[string]any{"state": "started"})
		store.outbox[inbox.RunID+"/"+inbox.AttemptID] = append(store.outbox[inbox.RunID+"/"+inbox.AttemptID], OutboxRecord{RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "legacy", EventType: "arop.run.started", Sequence: sequence, Envelope: data, CreatedAt: runtimeNow})
	}
	runtime, _ := NewRuntime(Config{Store: store, Verifier: &streamVerifier{claims: claims}, Handler: HandlerFunc(func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error) {
		return runwire.RunResult{}, nil
	}), Clock: func() time.Time { return runtimeNow }})
	response := streamRequest(runtime, "2")
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "STREAM_CURSOR_EXPIRED") || !strings.Contains(response.Body.String(), `"first_available_sequence":7`) {
		t.Fatalf("expired status=%d body=%s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+claims.RunID+"/events", nil)
	request.Header = http.Header{"Accept": {"text/event-stream", "text/event-stream"}, "Authorization": {"Bearer secret-token-000000"}}
	response = httptest.NewRecorder()
	runtime.ServeHTTP(response, request)
	if response.Code != http.StatusNotAcceptable {
		t.Fatalf("duplicate accept status=%d", response.Code)
	}
}
