package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCompositionRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	_, _, cleanup, err := compose([]string{"--listen=127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("valid composition rejected: %v", err)
	}
	defer cleanup()
	for _, test := range []struct {
		name              string
		args, environment []string
	}{
		{"unknown-environment", nil, []string{"AROP_CP_NOT_ALLOWED=p08-secret-value"}},
		{"unsafe-bind", []string{"--listen=0.0.0.0:8080"}, nil},
		{"invalid-mode", []string{"--mode=production"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := compose(test.args, test.environment)
			if err == nil {
				t.Fatal("invalid composition was accepted")
			}
			if strings.Contains(err.Error(), "p08-secret-value") {
				t.Fatalf("configuration error leaked value: %v", err)
			}
		})
	}

	t.Run("durable-sqlite-composes-reference-auth-without-secret-route", func(t *testing.T) {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		migrationRoot := filepath.Join(root, "migrations")
		for _, relative := range []string{"sqlite/0001_base.sql", "sqlite/0005_identity.sql", "sqlite/0010_publication.sql", "postgres/0001_base.sql", "postgres/0005_identity.sql", "postgres/0010_publication.sql"} {
			source := filepath.Join("..", "..", "migrations", filepath.FromSlash(relative))
			contents, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(migrationRoot, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		backup := filepath.Join(root, "backup")
		if err := os.Mkdir(backup, 0o700); err != nil {
			t.Fatal(err)
		}
		application, server, cleanup, err := compose([]string{
			"--listen=127.0.0.1:0", "--mode=sqlite", "--database-dsn=" + filepath.Join(root, "identity.db"),
			"--migration-root=" + migrationRoot, "--backup-directory=" + backup,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		snapshot := application.Readiness(context.Background())
		foundIdentity, foundPublication := false, false
		for _, check := range snapshot.Checks {
			if check.Name == "identity-cache" && check.Ready {
				foundIdentity = true
			}
			if check.Name == "publication-service" && check.Ready {
				foundPublication = true
			}
		}
		if !snapshot.Ready || !foundIdentity || !foundPublication || application.Config().MaxBodyBytes != 10<<20 {
			t.Fatalf("P12 durable readiness/config missing: %+v config=%+v", snapshot, application.Config())
		}
		for _, path := range []string{"/v1/secrets", "/v1/secret-values", "/v1/credentials/cred_test/value"} {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer p10-production-auth-sentinel")
			server.Handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || response.Body.String() != "{\"status\":\"unauthorized\"}\n" {
				t.Fatalf("reference authentication did not fail closed for %s: status=%d body=%s", path, response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "p10-production-auth-sentinel") {
				t.Fatal("authentication response leaked presented credential")
			}
		}
		health := httptest.NewRecorder()
		server.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/health/live", nil))
		if health.Code != http.StatusOK {
			t.Fatalf("health did not bypass authentication: %d %s", health.Code, health.Body.String())
		}
	})

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
