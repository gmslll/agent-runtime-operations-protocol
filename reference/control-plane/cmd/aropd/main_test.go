package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompositionRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	if _, _, err := compose([]string{"--listen=127.0.0.1:0"}, nil); err != nil {
		t.Fatalf("valid composition rejected: %v", err)
	}
	for _, test := range []struct {
		name              string
		args, environment []string
	}{
		{"unknown-environment", nil, []string{"AROP_CP_NOT_ALLOWED=p08-secret-value"}},
		{"unsafe-bind", []string{"--listen=0.0.0.0:8080"}, nil},
		{"invalid-mode", []string{"--mode=production"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := compose(test.args, test.environment)
			if err == nil {
				t.Fatal("invalid composition was accepted")
			}
			if strings.Contains(err.Error(), "p08-secret-value") {
				t.Fatalf("configuration error leaked value: %v", err)
			}
		})
	}

	t.Run("normal-shutdown", func(t *testing.T) {
		events := &eventLog{}
		server := newLifecycleServer(events, http.ErrServerClosed)
		shutdown, cancel := context.WithCancel(context.Background())
		cancel()
		if err := serve(shutdown, drainingRecorder{events}, server, time.Second); err != nil {
			t.Fatalf("normal shutdown failed: %v", err)
		}
		assertLifecycle(t, events.snapshot(), true, false)
		server.assertReturned(t)
	})

	t.Run("shutdown-error-forces-close-and-waits", func(t *testing.T) {
		events := &eventLog{}
		server := newLifecycleServer(events, http.ErrServerClosed)
		server.shutdownFunc = func(context.Context) error { return errors.New("shutdown marker") }
		shutdown, cancel := context.WithCancel(context.Background())
		cancel()
		err := serve(shutdown, drainingRecorder{events}, server, time.Second)
		if err == nil || !strings.Contains(err.Error(), "shutdown marker") {
			t.Fatalf("shutdown error not returned: %v", err)
		}
		assertLifecycle(t, events.snapshot(), true, true)
		server.assertReturned(t)
	})

	t.Run("shutdown-timeout-forces-close-and-waits", func(t *testing.T) {
		events := &eventLog{}
		server := newLifecycleServer(events, errors.New("serve marker"))
		server.shutdownFunc = func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		server.closeErr = errors.New("close marker")
		shutdown, cancel := context.WithCancel(context.Background())
		cancel()
		err := serve(shutdown, drainingRecorder{events}, server, time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || !strings.Contains(err.Error(), "close marker") || !strings.Contains(err.Error(), "serve marker") {
			t.Fatalf("shutdown errors were not joined: %v", err)
		}
		assertLifecycle(t, events.snapshot(), true, true)
		server.assertReturned(t)
	})

	t.Run("serve-error", func(t *testing.T) {
		events := &eventLog{}
		server := newLifecycleServer(events, errors.New("serve marker"))
		server.immediate = true
		err := serve(context.Background(), drainingRecorder{events}, server, time.Second)
		if err == nil || !strings.Contains(err.Error(), "serve marker") {
			t.Fatalf("serve error not returned: %v", err)
		}
		if got := strings.Join(events.snapshot(), ","); got != "serve" {
			t.Fatalf("unexpected serve-error lifecycle: %s", got)
		}
		server.assertReturned(t)
	})
}

type eventLog struct {
	mutex  sync.Mutex
	events []string
}

func (log *eventLog) add(event string) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	log.events = append(log.events, event)
}

func (log *eventLog) snapshot() []string {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return append([]string(nil), log.events...)
}

type drainingRecorder struct{ events *eventLog }

func (recorder drainingRecorder) SetDraining(draining bool) {
	if draining {
		recorder.events.add("drain")
	}
}

type lifecycleServer struct {
	events       *eventLog
	stopped      chan struct{}
	returned     chan struct{}
	stopOnce     sync.Once
	immediate    bool
	serveErr     error
	shutdownFunc func(context.Context) error
	closeErr     error
}

func newLifecycleServer(events *eventLog, serveErr error) *lifecycleServer {
	return &lifecycleServer{events: events, stopped: make(chan struct{}), returned: make(chan struct{}), serveErr: serveErr}
}

func (server *lifecycleServer) ListenAndServe() error {
	server.events.add("serve")
	defer close(server.returned)
	if !server.immediate {
		<-server.stopped
	}
	return server.serveErr
}

func (server *lifecycleServer) Shutdown(ctx context.Context) error {
	server.events.add("shutdown")
	if server.shutdownFunc != nil {
		return server.shutdownFunc(ctx)
	}
	server.stopOnce.Do(func() { close(server.stopped) })
	return nil
}

func (server *lifecycleServer) Close() error {
	server.events.add("close")
	server.stopOnce.Do(func() { close(server.stopped) })
	return server.closeErr
}

func (server *lifecycleServer) assertReturned(t *testing.T) {
	t.Helper()
	select {
	case <-server.returned:
	default:
		t.Fatal("serve goroutine was not collected")
	}
}

func assertLifecycle(t *testing.T, events []string, wantShutdown, wantClose bool) {
	t.Helper()
	positions := map[string]int{}
	for index, event := range events {
		positions[event] = index + 1
	}
	if positions["serve"] == 0 || wantShutdown != (positions["shutdown"] != 0) || wantClose != (positions["close"] != 0) {
		t.Fatalf("unexpected lifecycle events: %v", events)
	}
	if wantShutdown && (positions["drain"] == 0 || positions["drain"] > positions["shutdown"]) {
		t.Fatalf("draining did not precede shutdown: %v", events)
	}
	if wantClose && positions["shutdown"] > positions["close"] {
		t.Fatalf("close preceded shutdown: %v", events)
	}
}
