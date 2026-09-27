package worker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

type fakePullAPI struct {
	mu          sync.Mutex
	claims      []workerwire.WorkerClaim
	completeErr error
	completes   []completeCall
	releases    []string
	renews      int
	completed   chan struct{}
}

type completeCall struct {
	key     string
	request workerwire.WorkerComplete
}

func (api *fakePullAPI) Claim(ctx context.Context, _ string, _ workerwire.WorkerClaimRequest) (workerwire.WorkerClaim, bool, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.claims) == 0 {
		select {
		case <-ctx.Done():
			return workerwire.WorkerClaim{}, false, ctx.Err()
		default:
		}
		return workerwire.WorkerClaim{}, false, nil
	}
	claim := api.claims[0]
	api.claims = api.claims[1:]
	return claim, true, nil
}

func (api *fakePullAPI) Renew(context.Context, string, string, workerwire.WorkerRenewRequest) (workerwire.WorkerClaim, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.renews++
	return testClaim(nil, 1), nil
}

func (api *fakePullAPI) Complete(_ context.Context, _, _ string, key string, request workerwire.WorkerComplete) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.completes = append(api.completes, completeCall{key: key, request: request})
	if api.completeErr != nil {
		err := api.completeErr
		api.completeErr = nil
		return err
	}
	if api.completed != nil {
		select {
		case <-api.completed:
		default:
			close(api.completed)
		}
	}
	return nil
}

func (api *fakePullAPI) Release(_ context.Context, _ string, claimID string, _ workerwire.WorkerReleaseRequest) error {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.releases = append(api.releases, claimID)
	return nil
}

type fixedCompletionSource struct{}

func (fixedCompletionSource) Completion(context.Context, workerwire.WorkerClaim) (string, string, error) {
	return "cmp_018f0c00-0000-7000-8000-000000000099", "complete-fixed-key", nil
}

func TestRunnerRetriesIdenticalCompletionAndRenews(t *testing.T) {
	retry := uint64(0)
	api := &fakePullAPI{
		claims:      []workerwire.WorkerClaim{testClaim(t, 1)},
		completeErr: &RemoteError{StatusCode: 503, Code: "DEPENDENCY_UNAVAILABLE", Retryable: true, RetryAfterSeconds: &retry},
		completed:   make(chan struct{}),
	}
	runner, err := NewRunner(RunnerConfig{
		API: api, Handler: HandlerFunc(func(ctx context.Context, task Task) (Outcome, error) {
			select {
			case <-time.After(1100 * time.Millisecond):
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
			effect, err := task.EffectID("charge-customer")
			if err != nil {
				return Outcome{}, err
			}
			return Outcome{Result: successfulResult(task.Claim), EffectIDs: []string{effect}}, nil
		}),
		CompletionSource: fixedCompletionSource{}, WorkerID: "worker-a", SessionID: "ses_018f0c00-0000-7000-8000-000000000002", Generation: 7,
		SupportedBindings: []workerwire.AgentBinding{claimBinding()}, Concurrency: 1, LeaseSeconds: 15, RenewInterval: time.Second, BackoffMinimum: time.Millisecond, BackoffMaximum: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(context.Background()) }()
	select {
	case <-api.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("completion timed out")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runner.Drain(drainCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.completes) != 2 || api.completes[0].key != api.completes[1].key || api.completes[0].request.CompletionID != api.completes[1].request.CompletionID {
		t.Fatalf("completion retry changed identity: %#v", api.completes)
	}
	if api.renews == 0 || len(api.releases) != 0 {
		t.Fatalf("renew/release mismatch: renews=%d releases=%v", api.renews, api.releases)
	}
}

func TestRunnerBackpressureAndDrainCancellation(t *testing.T) {
	api := &fakePullAPI{claims: []workerwire.WorkerClaim{testClaim(t, 1), testClaim(t, 2), testClaim(t, 3)}}
	var active atomic.Int64
	var maximum atomic.Int64
	handler := HandlerFunc(func(ctx context.Context, _ Task) (Outcome, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	runner, err := NewRunner(RunnerConfig{API: api, Handler: handler, CompletionSource: fixedCompletionSource{}, WorkerID: "worker-a", SessionID: "ses_018f0c00-0000-7000-8000-000000000002", Generation: 7, SupportedBindings: []workerwire.AgentBinding{claimBinding()}, Concurrency: 2, LeaseSeconds: 15, RenewInterval: 10 * time.Second, BackoffMinimum: time.Millisecond, BackoffMaximum: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for active.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := runner.Drain(drainCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error=%v", err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if maximum.Load() > 2 || len(api.releases) != 2 || len(api.claims) != 1 {
		t.Fatalf("backpressure/drain mismatch: max=%d releases=%d remaining=%d", maximum.Load(), len(api.releases), len(api.claims))
	}
}

func TestEffectIDStableAcrossAttempts(t *testing.T) {
	first, err := EffectID("run_018f0c00-0000-7000-8000-000000000003", "send-email")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Task{Claim: testClaim(t, 2)}.EffectID("send-email")
	if err != nil || first != second || !strings.HasPrefix(first, "eff_") {
		t.Fatalf("effect identity mismatch: %q %q %v", first, second, err)
	}
	different, _ := EffectID("run_018f0c00-0000-7000-8000-000000000003", "send-sms")
	if different == first {
		t.Fatal("different operation reused effect id")
	}
}

func testClaim(t *testing.T, index int) workerwire.WorkerClaim {
	data, err := os.ReadFile("../../../conformance/fixtures/worker/claim.valid.json")
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	claim, err := workerwire.DecodeWorkerClaim(data)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	suffix := byte('0' + index)
	claim.ClaimID = workerwire.ClaimId(strings.TrimSuffix(string(claim.ClaimID), "1") + string(suffix))
	claim.AttemptID = workerwire.AttemptId(strings.TrimSuffix(string(claim.AttemptID), "4") + string(suffix))
	claim.AttemptNumber = workerwire.PositiveSafeInteger(index)
	claim.RunRequest.DeadlineAt = workerwire.DateTime(time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	claim.LeaseExpiresAt = workerwire.DateTime(time.Now().UTC().Add(15 * time.Second).Format(time.RFC3339Nano))
	return claim
}

func successfulResult(claim workerwire.WorkerClaim) workerwire.AROPV1TerminalRunResult {
	return workerwire.AROPV1TerminalRunResult{
		SchemaVersion: 1,
		RunID:         claim.RunID,
		State:         "succeeded",
		Snapshot: &workerwire.Snapshot{
			Revision: 1,
			Content:  claim.RunRequest.Input,
			Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Usage:       workerwire.Usage{InputTokens: 1, OutputTokens: 1, DurationMs: 1},
		CompletedAt: workerwire.DateTime(time.Now().UTC().Format(time.RFC3339Nano)),
	}
}
