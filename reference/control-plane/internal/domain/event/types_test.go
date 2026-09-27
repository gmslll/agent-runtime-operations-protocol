package event

import (
	"encoding/json"
	"testing"
	"time"
)

const (
	testRunID     = "run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10"
	testAttemptID = "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12"
	testTrace     = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
)

func TestBatchDigestBindsWireOrder(t *testing.T) {
	first := testEnvelope(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef21", "io.kinglucky.arop.run.started.v1", "lifecycle-events-v1.schema.json", `{"state":"started"}`)
	second := testEnvelope(2, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef22", "io.kinglucky.arop.progress.updated.v1", "progress-events-v1.schema.json", `{"step_id":"prepare","progress":25,"state":"running"}`)
	request := BatchRequest{RunID: testRunID, AttemptID: testAttemptID, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef30", FencingToken: 1, Events: []Envelope{first, second}}
	reordered := request
	reordered.Events = []Envelope{second, first}
	if BatchDigest(request) == BatchDigest(reordered) {
		t.Fatal("batch digest did not bind event order")
	}
	if BatchDigest(request) != BatchDigest(request) {
		t.Fatal("batch digest is not deterministic")
	}
}

func TestEnvelopeRejectsNonCanonicalSourceAndTerminalWithoutFinalResult(t *testing.T) {
	value := testEnvelope(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef23", "io.kinglucky.arop.run.started.v1", "lifecycle-events-v1.schema.json", `{"state":"started"}`)
	value.Source = "https://runtime.example.invalid/instances/instance-a?secret=1"
	if value.Validate() == nil {
		t.Fatal("event source with query was accepted")
	}
	terminal := testEnvelope(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef24", "io.kinglucky.arop.run.succeeded.v1", "lifecycle-events-v1.schema.json", `{"state":"succeeded","usage":{"input_tokens":1,"output_tokens":1,"duration_ms":1},"completed_at":"2026-09-21T08:00:01Z"}`)
	if terminal.Validate() == nil {
		t.Fatal("terminal event without snapshot or result_ref was accepted")
	}
}

func testEnvelope(sequence uint64, id, kind, schema, data string) Envelope {
	return Envelope{
		SpecVersion: "1.0", ID: id, Source: "https://runtime.example.invalid/instances/instance-a",
		Type: kind, Subject: "runs/" + testRunID, Time: time.Date(2026, 9, 21, 8, 0, int(sequence), 0, time.UTC),
		DataContentType: "application/json", DataSchema: "https://arop.invalid/schemas/v1/events/" + schema,
		RunID: testRunID, AttemptID: testAttemptID, ProducerSequence: sequence, Traceparent: testTrace, Data: json.RawMessage(data),
	}
}
