package pullworker

import (
	"context"
	"testing"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
	workersdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/worker"
)

func TestEchoHandlerProducesTerminalSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 28, 3, 4, 5, 0, time.UTC)
	claim := workerwire.WorkerClaim{
		RunID:      "run_018f0c00-0000-7000-8000-000000000003",
		RunRequest: workerwire.AROPV1RunRequest{Input: []workerwire.AROPV1ContentPart{{Text: &workerwire.AROPV1ContentPartText{Type: "text", Text: "hello"}}}},
	}
	outcome, err := (EchoHandler{Clock: func() time.Time { return now }}).Handle(context.Background(), workersdk.Task{Claim: claim})
	if err != nil || outcome.Result.State != "succeeded" || outcome.Result.Snapshot == nil || len(outcome.Result.Snapshot.Content) != 1 || string(outcome.Result.Snapshot.Digest) == "" || string(outcome.Result.CompletedAt) != now.Format(time.RFC3339Nano) {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (EchoHandler{}).Handle(ctx, workersdk.Task{Claim: claim}); err == nil {
		t.Fatal("cancelled handler succeeded")
	}
}
