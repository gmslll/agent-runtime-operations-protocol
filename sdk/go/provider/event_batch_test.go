package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

func TestEventBatchPlanPartialAckRetriesOnlySuffix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemoryStore()
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	inbox := InboxRecord{
		RunID: "run_01999999-9999-7999-8999-999999999990", AttemptID: "att_01999999-9999-7999-8999-999999999991",
		DeploymentID: "dep_01999999-9999-7999-8999-999999999992", InstanceID: "ins_01999999-9999-7999-8999-999999999993",
		Generation: 1, FencingToken: 7, Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		State: InboxRunning, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour),
	}
	if err := store.Within(ctx, func(ctx context.Context, transaction Transaction) error {
		if err := transaction.CreateInbox(ctx, inbox); err != nil {
			return err
		}
		for index := 1; index <= 3; index++ {
			if _, err := transaction.AppendOutbox(ctx, OutboxRecord{
				RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "evt_source_" + string(rune('0'+index)),
				EventType: "arop.run.running", Envelope: json.RawMessage(`{"state":"running"}`), CreatedAt: now.Add(time.Duration(index) * time.Second),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(Config{Store: store, Verifier: staticVerifier{}, Handler: HandlerFunc(func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error) {
		return runwire.RunResult{}, nil
	}), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.BuildEventBatch(ctx, inbox.RunID, inbox.AttemptID)
	if err != nil || first.FirstSequence != 1 || first.LastSequence != 3 {
		t.Fatalf("first plan=%#v err=%v", first, err)
	}
	retry, err := runtime.BuildEventBatch(ctx, inbox.RunID, inbox.AttemptID)
	if err != nil || !EventBatchPayloadEqual(first, retry) {
		t.Fatalf("ambiguous retry changed plan: %v", err)
	}
	partial := []byte(`{"accepted_through_producer_sequence":2,"assigned_run_sequence":12,"duplicate_event_ids":[],"run_state":"running"}`)
	if err = runtime.ApplyEventBatchAck(ctx, first, partial); err != nil {
		t.Fatal(err)
	}
	suffix, err := runtime.BuildEventBatch(ctx, inbox.RunID, inbox.AttemptID)
	if err != nil || suffix.FirstSequence != 3 || suffix.LastSequence != 3 || EventBatchPayloadEqual(first, suffix) {
		t.Fatalf("suffix plan=%#v err=%v", suffix, err)
	}
	final := []byte(`{"accepted_through_producer_sequence":3,"assigned_run_sequence":13,"duplicate_event_ids":[],"run_state":"running"}`)
	if err = runtime.ApplyEventBatchAck(ctx, suffix, final); err != nil {
		t.Fatal(err)
	}
	if _, err = runtime.BuildEventBatch(ctx, inbox.RunID, inbox.AttemptID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("all acknowledged build err=%v", err)
	}
	// Replayed ACK is harmless and never marks beyond the acknowledged prefix.
	if err = runtime.ApplyEventBatchAck(ctx, suffix, final); err != nil {
		t.Fatalf("replayed acknowledgement: %v", err)
	}
}

func TestEventBatchAckRejectsMutationAndOverAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemoryStore()
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	inbox := InboxRecord{RunID: "run_01999999-9999-7999-8999-999999999990", AttemptID: "att_01999999-9999-7999-8999-999999999991", DeploymentID: "dep_01999999-9999-7999-8999-999999999992", InstanceID: "ins_01999999-9999-7999-8999-999999999993", Generation: 1, FencingToken: 1, Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", State: InboxRunning, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
	if err := store.Within(ctx, func(ctx context.Context, transaction Transaction) error {
		if err := transaction.CreateInbox(ctx, inbox); err != nil {
			return err
		}
		_, err := transaction.AppendOutbox(ctx, OutboxRecord{RunID: inbox.RunID, AttemptID: inbox.AttemptID, EventID: "evt_source_1", EventType: "arop.run.running", Envelope: json.RawMessage(`{"state":"running"}`), CreatedAt: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(Config{Store: store, Verifier: staticVerifier{}, Handler: HandlerFunc(func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error) {
		return runwire.RunResult{}, nil
	}), Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runtime.BuildEventBatch(ctx, inbox.RunID, inbox.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	mutated := plan
	mutated.Payload = append(json.RawMessage(nil), plan.Payload...)
	mutated.Payload[0] = '['
	ack := []byte(`{"accepted_through_producer_sequence":1,"assigned_run_sequence":1,"duplicate_event_ids":[],"run_state":"running"}`)
	if runtime.ApplyEventBatchAck(ctx, mutated, ack) == nil {
		t.Fatal("mutated plan accepted")
	}
	over := []byte(`{"accepted_through_producer_sequence":2,"assigned_run_sequence":2,"duplicate_event_ids":[],"run_state":"running"}`)
	if runtime.ApplyEventBatchAck(ctx, plan, over) == nil {
		t.Fatal("over-ack accepted")
	}
	badDuplicate := []byte(`{"accepted_through_producer_sequence":1,"assigned_run_sequence":1,"duplicate_event_ids":["evt_01999999-9999-7999-8999-999999999999"],"run_state":"running"}`)
	if runtime.ApplyEventBatchAck(ctx, plan, badDuplicate) == nil {
		t.Fatal("foreign duplicate id accepted")
	}
}
