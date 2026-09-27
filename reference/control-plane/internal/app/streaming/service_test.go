package streaming

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

const testRunID = "run_01999999-9999-7999-8999-999999999999"

type fakeReader struct {
	mu      sync.Mutex
	pages   []Page
	binding run.AgentBinding
}

func (reader *fakeReader) Binding(context.Context, string, string) (run.AgentBinding, error) {
	return reader.binding, nil
}
func (reader *fakeReader) Read(context.Context, string, string, uint64, int) (Page, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.pages) == 0 {
		return Page{}, errors.New("unexpected read")
	}
	value := reader.pages[0]
	reader.pages = reader.pages[1:]
	return value, nil
}

type fakeAuthorizer struct{ calls int }

func (authorizer *fakeAuthorizer) Authorize(context.Context, run.Caller, run.Operation, run.AgentBinding) (run.AuthorizationSnapshot, error) {
	authorizer.calls++
	return run.AuthorizationSnapshot{}, nil
}

type fakeWaiter struct{ calls int }

func (waiter *fakeWaiter) Wait(context.Context, time.Duration) error { waiter.calls++; return nil }

type fakeSink struct {
	started, heartbeats int
	records             []Record
	fail                bool
}

func (sink *fakeSink) Start() error { sink.started++; return nil }
func (sink *fakeSink) Event(record Record) error {
	if sink.fail {
		return errors.New("slow consumer")
	}
	sink.records = append(sink.records, record)
	return nil
}
func (sink *fakeSink) Heartbeat() error { sink.heartbeats++; return nil }

func TestServiceReplayWaitLiveTerminal(t *testing.T) {
	reader := &fakeReader{binding: binding(), pages: []Page{
		{FirstAvailable: 1, Latest: 2, Records: []Record{record(1), record(2)}},
		{FirstAvailable: 1, Latest: 2},
		{FirstAvailable: 1, Latest: 3, Terminal: true, Records: []Record{record(3)}},
	}}
	waiter, authorizer, sink := &fakeWaiter{}, &fakeAuthorizer{}, &fakeSink{}
	service, err := New(Dependencies{Reader: reader, Authorizer: authorizer, Waiter: waiter, PollInterval: 10 * time.Millisecond, HeartbeatEvery: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Stream(context.Background(), request(0), sink); err != nil {
		t.Fatal(err)
	}
	if sink.started != 1 || len(sink.records) != 3 || sink.records[2].Sequence != 3 || sink.heartbeats != 1 || waiter.calls != 2 || authorizer.calls != 1 {
		t.Fatalf("unexpected stream: %#v waiter=%d auth=%d", sink, waiter.calls, authorizer.calls)
	}
}

func TestServiceCursorAndSlowConsumerFailClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		page     Page
		after    uint64
		failSink bool
		reason   string
	}{
		{name: "expired", page: Page{FirstAvailable: 7, Latest: 9, Records: []Record{record(7)}}, after: 2, reason: ReasonCursorExpired},
		{name: "ahead", page: Page{FirstAvailable: 1, Latest: 3}, after: 4, reason: ReasonCursorAhead},
		{name: "gap", page: Page{FirstAvailable: 1, Latest: 4, Records: []Record{record(3)}}, after: 1, reason: ReasonDependencyUnavailable},
		{name: "slow", page: Page{FirstAvailable: 1, Latest: 1, Terminal: true, Records: []Record{record(1)}}, failSink: true, reason: ReasonDependencyUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeReader{binding: binding(), pages: []Page{test.page}}
			service, _ := New(Dependencies{Reader: reader, Authorizer: &fakeAuthorizer{}, Waiter: &fakeWaiter{}, PollInterval: 10 * time.Millisecond, HeartbeatEvery: 1})
			sink := &fakeSink{fail: test.failSink}
			err := service.Stream(context.Background(), request(test.after), sink)
			typed, ok := AsError(err)
			if !ok || typed.Reason != test.reason {
				t.Fatalf("err=%v", err)
			}
			if (test.name == "expired" || test.name == "ahead") && sink.started != 0 {
				t.Fatal("cursor rejection started response")
			}
			if test.name == "expired" && (typed.LatestSequence != 9 || typed.SnapshotURL != "/v1/agent-runs/"+testRunID) {
				t.Fatalf("expired=%#v", typed)
			}
		})
	}
}

func request(after uint64) Request {
	return Request{Caller: run.Caller{TenantID: "tenant-a", PrincipalID: "prn_01999999-9999-7999-8999-999999999998", CredentialID: "cred_01999999-9999-7999-8999-999999999997", Scopes: []string{"agent:read"}}, RunID: testRunID, After: after}
}

func binding() run.AgentBinding {
	return run.AgentBinding{ID: "agent-a", Version: "1.0.0", SkillID: "answer", ManifestDigest: "sha256:" + string(makeBytes('a', 64))}
}

func record(sequence uint64) Record {
	payload := map[string]any{
		"specversion": "1.0", "id": "evt_01999999-9999-7999-8999-999999999996", "source": "https://runtime.example.invalid/instances/runtime-a",
		"type": "io.kinglucky.arop.output.delta.v1", "subject": "runs/" + testRunID, "time": "2026-09-27T00:00:00Z", "datacontenttype": "application/json",
		"dataschema": "https://arop.invalid/schemas/v1/events/output-events-v1.schema.json", "runid": testRunID, "attemptid": "att_01999999-9999-7999-8999-999999999995",
		"producersequence": sequence, "runsequence": sequence, "traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "data": map[string]any{"output_id": "answer", "offset": sequence - 1, "delta": "x"},
	}
	encoded, _ := json.Marshal(payload)
	return Record{Sequence: sequence, EventType: "io.kinglucky.arop.output.delta.v1", Envelope: encoded}
}

func makeBytes(value byte, count int) []byte {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return result
}
