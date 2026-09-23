package platform

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
)

type testClock struct{ now time.Time }

func (clock testClock) Now() time.Time { return clock.now }

type testIDs struct {
	mutex    sync.Mutex
	counters map[platformports.IDKind]int
}

func newTestIDs() *testIDs { return &testIDs{counters: map[platformports.IDKind]int{}} }
func (source *testIDs) NewID(ctx context.Context, kind platformports.IDKind) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.counters[kind]++
	value := source.counters[kind]
	switch kind {
	case platformports.IDRequest:
		return fmt.Sprintf("req_01956e7b-9abc-7def-8abc-%012x", value), nil
	case platformports.IDAudit:
		return fmt.Sprintf("aud_01956e7b-9abc-7def-8abc-%012x", value), nil
	case platformports.IDTrace:
		return fmt.Sprintf("4bf92f3577b34da6a3ce929d%08x", value), nil
	case platformports.IDSpan:
		return fmt.Sprintf("00f067aa%08x", value), nil
	default:
		return "", errors.New("unknown kind")
	}
}

type testFault struct{ checkpoint platformports.Checkpoint }

func (fault testFault) Check(_ context.Context, checkpoint platformports.Checkpoint) error {
	if checkpoint == fault.checkpoint {
		return errors.New("injected")
	}
	return nil
}

type testUoW struct{ calls int }

func (unit *testUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	unit.calls++
	return callback(ctx)
}

type transactionUoW struct {
	calls, commits, rollbacks int
}

func (unit *transactionUoW) Within(ctx context.Context, callback func(context.Context) error) (err error) {
	unit.calls++
	committed := false
	defer func() {
		if !committed {
			unit.rollbacks++
		}
	}()
	if err = callback(ctx); err != nil {
		return err
	}
	unit.commits++
	committed = true
	return nil
}

type testReadiness struct {
	name string
	err  error
}

func (check *testReadiness) Name() string                { return check.name }
func (check *testReadiness) Check(context.Context) error { return check.err }

func TestConfigValidationFailsClosed(t *testing.T) {
	t.Parallel()
	valid, err := ParseConfig([]string{"--listen=127.0.0.1:0"}, nil)
	if err != nil || valid.ListenAddress != "127.0.0.1:0" {
		t.Fatalf("valid config rejected: %#v %v", valid, err)
	}
	tests := []struct {
		name              string
		args, environment []string
	}{
		{"unknown-flag", []string{"--unknown=x"}, nil},
		{"duplicate-flag", []string{"--listen=127.0.0.1:1", "--listen=127.0.0.1:2"}, nil},
		{"unknown-env", nil, []string{"AROP_CP_UNKNOWN=p08-secret-value"}},
		{"env-flag-conflict", []string{"--listen=127.0.0.1:1"}, []string{"AROP_CP_LISTEN=127.0.0.1:2"}},
		{"bad-duration", nil, []string{"AROP_CP_REQUEST_TIMEOUT=forever"}},
		{"zero-capacity", []string{"--audit-capacity=0"}, nil},
		{"unequal-observability-capacity-rejected", []string{"--audit-capacity=2", "--trace-capacity=3"}, nil},
		{"unsafe-listen", []string{"--listen=0.0.0.0:8080"}, nil},
		{"unsupported-mode", []string{"--mode=production"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseConfig(test.args, test.environment); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestInjectedClockIDAndFaultAreDeterministic(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	store, _ := memory.New(10, 10)
	ids := newTestIDs()
	unit := &testUoW{}
	application, err := New(DefaultConfig(), Dependencies{Clock: testClock{now}, IDs: ids, Faults: testFault{checkpoint: platformports.CheckpointBeforeUseCase}, UoW: unit, Observability: store}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := application.Now(); !got.Equal(now) {
		t.Fatalf("Now=%s", got)
	}
	id, err := application.NewID(context.Background(), platformports.IDRequest)
	if err != nil || id != "req_01956e7b-9abc-7def-8abc-000000000002" {
		t.Fatalf("deterministic ID=%q err=%v", id, err)
	}
	if application.CheckFault(context.Background(), platformports.CheckpointBeforeUseCase) == nil {
		t.Fatal("fault did not fire")
	}
	called := 0
	if err := application.Within(context.Background(), func(context.Context) error { called++; return nil }); err != nil || called != 1 {
		t.Fatalf("UoW called=%d err=%v", called, err)
	}
	if unit.calls != 2 {
		t.Fatalf("UoW calls=%d want self-check+request", unit.calls)
	}

	for _, scenario := range []struct {
		name          string
		fault         platformports.Checkpoint
		callbackError error
		cancel        bool
		wantCallback  int
		wantCommit    int
	}{
		{name: "success", wantCallback: 1, wantCommit: 2},
		{name: "before-fault", fault: platformports.CheckpointBeforeUseCase, wantCallback: 0, wantCommit: 1},
		{name: "after-fault-rolls-back-inside-transaction", fault: platformports.CheckpointAfterUseCase, wantCallback: 1, wantCommit: 1},
		{name: "callback-error", callbackError: errors.New("callback"), wantCallback: 1, wantCommit: 1},
		{name: "callback-cancel", cancel: true, wantCallback: 1, wantCommit: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			transactions := &transactionUoW{}
			scenarioStore, storeErr := memory.New(10, 10)
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			app, newErr := New(DefaultConfig(), Dependencies{Clock: testClock{now}, IDs: newTestIDs(), Faults: testFault{checkpoint: scenario.fault}, UoW: transactions, Observability: scenarioStore}, "arop", "test")
			if newErr != nil {
				t.Fatal(newErr)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callbacks := 0
			executeErr := app.Execute(ctx, func(context.Context) error {
				callbacks++
				if scenario.cancel {
					cancel()
					return ctx.Err()
				}
				return scenario.callbackError
			})
			wantError := scenario.fault != "" || scenario.callbackError != nil || scenario.cancel
			if (executeErr != nil) != wantError || callbacks != scenario.wantCallback || transactions.calls != 2 || transactions.commits != scenario.wantCommit || transactions.rollbacks != 2-scenario.wantCommit {
				t.Fatalf("err=%v callbacks=%d transaction=%+v", executeErr, callbacks, transactions)
			}
		})
	}

	t.Run("callback-panic-rolls-back-once", func(t *testing.T) {
		transactions := &transactionUoW{}
		panicStore, storeErr := memory.New(10, 10)
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		app, newErr := New(DefaultConfig(), Dependencies{Clock: testClock{now}, IDs: newTestIDs(), Faults: testFault{}, UoW: transactions, Observability: panicStore}, "arop", "test")
		if newErr != nil {
			t.Fatal(newErr)
		}
		callbacks := 0
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("callback panic was swallowed")
				}
			}()
			_ = app.Execute(context.Background(), func(context.Context) error {
				callbacks++
				panic("rollback")
			})
		}()
		if callbacks != 1 || transactions.calls != 2 || transactions.commits != 1 || transactions.rollbacks != 1 {
			t.Fatalf("panic path callbacks=%d transaction=%+v", callbacks, transactions)
		}
	})
}

func TestReadinessIsTruthful(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)
	store, _ := memory.New(10, 10)
	check := &testReadiness{name: "dependency"}
	application, err := New(DefaultConfig(), Dependencies{Clock: testClock{now}, IDs: newTestIDs(), Faults: testFault{}, UoW: &testUoW{}, Observability: store, Checks: []platformports.ReadinessCheck{check}}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := application.Readiness(context.Background())
	if !snapshot.Ready || snapshot.Scope != ReadinessScope || snapshot.Durability != DurabilityMode {
		t.Fatalf("unexpected ready snapshot: %#v", snapshot)
	}
	check.err = errors.New("p08-secret-value")
	if application.Readiness(context.Background()).Ready {
		t.Fatal("failed dependency reported ready")
	}
	check.err = nil
	application.SetDraining(true)
	if application.Readiness(context.Background()).Ready {
		t.Fatal("draining platform reported ready")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	application.SetDraining(false)
	if application.Readiness(expired).Ready {
		t.Fatal("expired readiness context reported ready")
	}
	if (&Platform{}).Readiness(context.Background()).Ready {
		t.Fatal("uninitialized platform reported ready")
	}
	if _, err := New(DefaultConfig(), Dependencies{Clock: testClock{now}, IDs: newTestIDs(), Faults: testFault{}, UoW: &testUoW{}, Observability: store, Checks: []platformports.ReadinessCheck{check, check}}, "arop", "test"); err == nil {
		t.Fatal("duplicate readiness names accepted")
	}
}
