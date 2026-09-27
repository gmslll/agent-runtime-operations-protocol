package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

// PullAPI is the transport seam used by Runner and deterministic tests.
type PullAPI interface {
	Claim(context.Context, string, workerwire.WorkerClaimRequest) (workerwire.WorkerClaim, bool, error)
	Renew(context.Context, string, string, workerwire.WorkerRenewRequest) (workerwire.WorkerClaim, error)
	Complete(context.Context, string, string, string, workerwire.WorkerComplete) error
	Release(context.Context, string, string, workerwire.WorkerReleaseRequest) error
}

// Handler executes one fenced claim. Delivery is at-least-once: a handler can
// observe the same Run after a crash or lease loss and must use EffectID for
// externally visible write/irreversible effects.
type Handler interface {
	Handle(context.Context, Task) (Outcome, error)
}

type HandlerFunc func(context.Context, Task) (Outcome, error)

func (handler HandlerFunc) Handle(ctx context.Context, task Task) (Outcome, error) {
	return handler(ctx, task)
}

type Task struct {
	Claim workerwire.WorkerClaim
}

func (task Task) EffectID(operation string) (string, error) {
	return EffectID(string(task.Claim.RunID), operation)
}

type Outcome struct {
	Result    workerwire.AROPV1TerminalRunResult
	EffectIDs []string
}

// CompletionSource creates a UUIDv7 completion id and a stable request key.
// It is called once per handled claim; retries reuse both values.
type CompletionSource interface {
	Completion(context.Context, workerwire.WorkerClaim) (completionID, idempotencyKey string, err error)
}

type RunnerConfig struct {
	API               PullAPI
	Handler           Handler
	CompletionSource  CompletionSource
	WorkerID          string
	SessionID         string
	Generation        uint64
	SupportedBindings []workerwire.AgentBinding
	Concurrency       int
	WaitSeconds       uint64
	LeaseSeconds      uint64
	RenewInterval     time.Duration
	BackoffMinimum    time.Duration
	BackoffMaximum    time.Duration
	Clock             func() time.Time
}

// Runner owns the claim loop and graceful drain lifecycle.
type Runner struct {
	config    RunnerConfig
	draining  atomic.Bool
	drainOnce sync.Once
	drain     chan struct{}
	activeMu  sync.Mutex
	active    map[string]context.CancelFunc
	activeWG  sync.WaitGroup
	claimMu   sync.Mutex
	claimStop context.CancelFunc
	runOnce   sync.Once
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.API == nil || config.Handler == nil || !validWorkerID(config.WorkerID) || config.SessionID == "" || config.Generation == 0 || config.Generation > 9007199254740991 || len(config.SupportedBindings) == 0 {
		return nil, errors.New("invalid worker runner configuration")
	}
	if config.Concurrency == 0 {
		config.Concurrency = 1
	}
	if config.Concurrency < 1 || config.Concurrency > 1024 || config.WaitSeconds > 30 {
		return nil, errors.New("invalid worker runner capacity")
	}
	if config.LeaseSeconds == 0 {
		config.LeaseSeconds = 60
	}
	if config.LeaseSeconds < 15 || config.LeaseSeconds > 300 {
		return nil, errors.New("invalid worker lease duration")
	}
	if config.RenewInterval == 0 {
		config.RenewInterval = time.Duration(config.LeaseSeconds) * time.Second / 3
	}
	if config.RenewInterval < time.Second || config.RenewInterval >= time.Duration(config.LeaseSeconds)*time.Second {
		return nil, errors.New("invalid worker renew interval")
	}
	if config.BackoffMinimum == 0 {
		config.BackoffMinimum = 100 * time.Millisecond
	}
	if config.BackoffMaximum == 0 {
		config.BackoffMaximum = 5 * time.Second
	}
	if config.BackoffMinimum <= 0 || config.BackoffMaximum < config.BackoffMinimum || config.BackoffMaximum > time.Minute {
		return nil, errors.New("invalid worker backoff")
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.CompletionSource == nil {
		config.CompletionSource = SecureCompletionSource{Clock: config.Clock}
	}
	return &Runner{config: config, drain: make(chan struct{}), active: map[string]context.CancelFunc{}}, nil
}

func (runner *Runner) Run(ctx context.Context) error {
	started := false
	runner.runOnce.Do(func() { started = true })
	if !started {
		return errors.New("worker runner already started")
	}
	backoff := runner.config.BackoffMinimum
	for {
		if runner.draining.Load() {
			runner.activeWG.Wait()
			return nil
		}
		if err := ctx.Err(); err != nil {
			runner.beginDrain()
			runner.cancelActive()
			runner.activeWG.Wait()
			return err
		}
		available := runner.availableSlots()
		if available == 0 {
			if err := runner.pause(ctx, 10*time.Millisecond); err != nil {
				continue
			}
			continue
		}
		leaseSeconds := workerwire.SafeInteger(runner.config.LeaseSeconds)
		claimContext, cancelClaim := context.WithCancel(ctx)
		runner.claimMu.Lock()
		if runner.draining.Load() {
			runner.claimMu.Unlock()
			cancelClaim()
			continue
		}
		runner.claimStop = cancelClaim
		runner.claimMu.Unlock()
		claim, found, err := runner.config.API.Claim(claimContext, runner.config.WorkerID, workerwire.WorkerClaimRequest{
			SchemaVersion:     1,
			SessionID:         workerwire.SessionId(runner.config.SessionID),
			Generation:        workerwire.PositiveSafeInteger(runner.config.Generation),
			AvailableSlots:    workerwire.PositiveSafeInteger(available),
			SupportedBindings: append([]workerwire.AgentBinding(nil), runner.config.SupportedBindings...),
			WaitSeconds:       workerwire.SafeInteger(runner.config.WaitSeconds),
			LeaseSeconds:      &leaseSeconds,
		})
		runner.claimMu.Lock()
		runner.claimStop = nil
		runner.claimMu.Unlock()
		cancelClaim()
		if runner.draining.Load() {
			continue
		}
		if err != nil {
			if waitErr := runner.pause(ctx, backoffFor(err, backoff, runner.config.BackoffMaximum)); waitErr != nil {
				continue
			}
			backoff = growBackoff(backoff, runner.config.BackoffMaximum)
			continue
		}
		if !found {
			backoff = runner.config.BackoffMinimum
			if runner.config.WaitSeconds == 0 {
				_ = runner.pause(ctx, backoff)
			}
			continue
		}
		backoff = runner.config.BackoffMinimum
		runner.start(ctx, claim)
	}
}

// Drain stops new claims and waits for in-flight claims. If ctx expires, their
// handler contexts are cancelled; each task then attempts a fenced release.
func (runner *Runner) Drain(ctx context.Context) error {
	runner.beginDrain()
	done := make(chan struct{})
	go func() {
		runner.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		runner.cancelActive()
		return ctx.Err()
	}
}

func (runner *Runner) beginDrain() {
	runner.drainOnce.Do(func() {
		runner.draining.Store(true)
		close(runner.drain)
		runner.claimMu.Lock()
		if runner.claimStop != nil {
			runner.claimStop()
		}
		runner.claimMu.Unlock()
	})
}

func (runner *Runner) availableSlots() int {
	runner.activeMu.Lock()
	defer runner.activeMu.Unlock()
	return runner.config.Concurrency - len(runner.active)
}

func (runner *Runner) start(parent context.Context, claim workerwire.WorkerClaim) {
	ctx, cancel := context.WithCancel(parent)
	claimID := string(claim.ClaimID)
	runner.activeMu.Lock()
	if runner.draining.Load() || len(runner.active) >= runner.config.Concurrency {
		runner.activeMu.Unlock()
		cancel()
		return
	}
	if _, duplicate := runner.active[claimID]; duplicate {
		runner.activeMu.Unlock()
		cancel()
		return
	}
	runner.active[claimID] = cancel
	runner.activeWG.Add(1)
	runner.activeMu.Unlock()
	go func() {
		defer runner.activeWG.Done()
		defer cancel()
		defer func() {
			runner.activeMu.Lock()
			delete(runner.active, claimID)
			runner.activeMu.Unlock()
		}()
		runner.handle(ctx, claim)
	}()
}

func (runner *Runner) handle(parent context.Context, claim workerwire.WorkerClaim) {
	runDeadline, err := time.Parse(time.RFC3339Nano, string(claim.RunRequest.DeadlineAt))
	if err != nil {
		runner.release(context.WithoutCancel(parent), claim, "retryable_failure")
		return
	}
	ctx, cancel := context.WithDeadline(parent, runDeadline)
	defer cancel()
	renewContext, stopRenew := context.WithCancel(ctx)
	renewStopped := make(chan struct{})
	go func() {
		defer close(renewStopped)
		runner.renew(renewContext, cancel, claim)
	}()
	defer func() {
		stopRenew()
		<-renewStopped
	}()
	outcome, handleErr := runner.config.Handler.Handle(ctx, Task{Claim: claim})
	if handleErr != nil || ctx.Err() != nil {
		reason := "retryable_failure"
		if runner.draining.Load() {
			reason = "worker_draining"
		}
		runner.release(context.WithoutCancel(parent), claim, reason)
		return
	}
	completionID, key, err := runner.config.CompletionSource.Completion(ctx, claim)
	if err != nil || !validIdempotencyKey(key) {
		runner.release(context.WithoutCancel(parent), claim, "retryable_failure")
		return
	}
	effectIDs := make([]workerwire.EffectId, len(outcome.EffectIDs))
	for index, value := range outcome.EffectIDs {
		effectIDs[index] = workerwire.EffectId(value)
	}
	request := workerwire.WorkerComplete{
		SchemaVersion: 1,
		CompletionID:  completionID,
		ClaimID:       claim.ClaimID,
		AttemptID:     claim.AttemptID,
		FencingToken:  workerwire.SafeInteger(claim.FencingToken),
		LeaseToken:    claim.LeaseToken,
		Result:        outcome.Result,
		CompletedAt:   workerwire.DateTime(runner.config.Clock().UTC().Format(time.RFC3339Nano)),
	}
	if len(effectIDs) > 0 {
		request.EffectIDs = &effectIDs
	}
	backoff := runner.config.BackoffMinimum
	for {
		err = runner.config.API.Complete(ctx, runner.config.WorkerID, string(claim.ClaimID), key, request)
		if err == nil || !retryable(err) {
			return
		}
		if runner.pause(ctx, backoffFor(err, backoff, runner.config.BackoffMaximum)) != nil {
			return
		}
		backoff = growBackoff(backoff, runner.config.BackoffMaximum)
	}
}

func (runner *Runner) renew(ctx context.Context, cancel context.CancelFunc, claim workerwire.WorkerClaim) {
	ticker := time.NewTicker(runner.config.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := runner.config.API.Renew(ctx, runner.config.WorkerID, string(claim.ClaimID), workerwire.WorkerRenewRequest{
				SchemaVersion: 1,
				LeaseToken:    claim.LeaseToken,
				FencingToken:  workerwire.PositiveSafeInteger(claim.FencingToken),
				LeaseSeconds:  workerwire.SafeInteger(runner.config.LeaseSeconds),
			})
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func (runner *Runner) release(ctx context.Context, claim workerwire.WorkerClaim, reason string) {
	timeout := runner.timeoutForCleanup()
	cleanup, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = runner.config.API.Release(cleanup, runner.config.WorkerID, string(claim.ClaimID), workerwire.WorkerReleaseRequest{
		SchemaVersion: 1,
		LeaseToken:    claim.LeaseToken,
		FencingToken:  workerwire.PositiveSafeInteger(claim.FencingToken),
		Reason:        reason,
	})
}

func (runner *Runner) timeoutForCleanup() time.Duration {
	if runner.config.BackoffMaximum < 5*time.Second {
		return 5 * time.Second
	}
	return runner.config.BackoffMaximum
}

func (runner *Runner) pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-runner.drain:
		return errors.New("worker draining")
	case <-timer.C:
		return nil
	}
}

func (runner *Runner) cancelActive() {
	runner.activeMu.Lock()
	defer runner.activeMu.Unlock()
	for _, cancel := range runner.active {
		cancel()
	}
}

func retryable(err error) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && remote.Retryable
}

func backoffFor(err error, fallback, maximum time.Duration) time.Duration {
	var remote *RemoteError
	if errors.As(err, &remote) && remote.RetryAfterSeconds != nil {
		value := time.Duration(*remote.RetryAfterSeconds) * time.Second
		if value > maximum {
			return maximum
		}
		return value
	}
	return fallback
}

func growBackoff(value, maximum time.Duration) time.Duration {
	if value >= maximum/2 {
		return maximum
	}
	return value * 2
}

var _ PullAPI = (*Client)(nil)
