package registrywatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type sequenceReader struct {
	mu      sync.Mutex
	windows []registry.EventWindow
	err     error
	calls   int
}

func (reader *sequenceReader) EventWindow(_ context.Context, _ string, _, _ uint64) (registry.EventWindow, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.calls++
	if reader.err != nil {
		return registry.EventWindow{}, reader.err
	}
	index := reader.calls - 1
	if index >= len(reader.windows) {
		index = len(reader.windows) - 1
	}
	return reader.windows[index], nil
}

type allowAuthorizer struct{ err error }

func (authorizer allowAuthorizer) Authorize(_ context.Context, request registryapi.AuthorizationRequest) error {
	if authorizer.err != nil || request.Operation != registryapi.OperationWatch || !request.Caller.HasScope("registry:discover") {
		return registryapi.ErrForbidden
	}
	return nil
}

type noopCoordinator struct{}

func (noopCoordinator) Acquire(context.Context, string) (Leadership, error) {
	return noopLeadership{}, nil
}
func (noopCoordinator) Check(context.Context) error { return nil }

type noopLeadership struct{}

func (noopLeadership) Compact(_ context.Context, target uint64) (CompactionResult, error) {
	return CompactionResult{Revision: target, Watermark: target}, nil
}
func (noopLeadership) Close() error { return nil }

func validCaller() registryapi.Caller {
	return registryapi.Caller{TenantID: "tenant-a", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:discover"}}
}

func validEvent(revision uint64) registry.Event {
	return registry.Event{EventID: "evt_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TenantID: "tenant-a", Revision: revision, Type: registry.EventRegistered, InstanceID: "instance-a", SessionID: "ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Generation: 1, OccurredAt: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)}
}

func newTestService(t *testing.T, reader Reader, hub *Hub) *Service {
	t.Helper()
	service, err := New(Dependencies{Reader: reader, Authorizer: allowAuthorizer{}, Notifier: hub, Coordinator: noopCoordinator{}, PollInterval: 5 * time.Millisecond, MaxEvents: 10000})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestWatchImmediateDoubleReadNotifyAndCompaction(t *testing.T) {
	t.Run("immediate", func(t *testing.T) {
		reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 2, Events: []registry.Event{validEvent(2)}}}}
		result, err := newTestService(t, reader, NewHub()).Watch(context.Background(), validCaller(), WatchInput{AfterRevision: 1, Wait: time.Second})
		if err != nil || result.Revision != 2 || len(result.Events) != 1 || reader.calls != 1 {
			t.Fatalf("result=%+v calls=%d err=%v", result, reader.calls, err)
		}
	})
	t.Run("double-read-closes-race", func(t *testing.T) {
		reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 1}, {Revision: 2, Events: []registry.Event{validEvent(2)}}}}
		hub := NewHub()
		result, err := newTestService(t, reader, hub).Watch(context.Background(), validCaller(), WatchInput{AfterRevision: 1, Wait: time.Second})
		if err != nil || result.Revision != 2 || reader.calls != 2 || hub.Subscribers("tenant-a") != 0 {
			t.Fatalf("result=%+v calls=%d subscribers=%d err=%v", result, reader.calls, hub.Subscribers("tenant-a"), err)
		}
	})
	t.Run("compacted", func(t *testing.T) {
		reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 9, CompactionWatermark: 5}}}
		_, err := newTestService(t, reader, NewHub()).Watch(context.Background(), validCaller(), WatchInput{AfterRevision: 4, Wait: time.Second})
		if !errors.Is(err, ErrCompacted) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestWatchNotificationPollingCancellationAndTenantIsolation(t *testing.T) {
	reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 1}, {Revision: 1}, {Revision: 2, Events: []registry.Event{validEvent(2)}}}}
	hub := NewHub()
	service := newTestService(t, reader, hub)
	done := make(chan error, 1)
	go func() {
		_, err := service.Watch(context.Background(), validCaller(), WatchInput{AfterRevision: 1, Wait: time.Second})
		done <- err
	}()
	for deadline := time.Now().Add(time.Second); hub.Subscribers("tenant-a") != 1; {
		if time.Now().After(deadline) {
			t.Fatal("watch did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	hub.Notify("tenant-b", 2)
	select {
	case err := <-done:
		t.Fatalf("wrong tenant woke watch: %v", err)
	case <-time.After(2 * time.Millisecond):
	}
	hub.Notify("tenant-a", 2)
	if err := <-done; err != nil || hub.Subscribers("tenant-a") != 0 {
		t.Fatalf("err=%v subscribers=%d", err, hub.Subscribers("tenant-a"))
	}

	cancelReader := &sequenceReader{windows: []registry.EventWindow{{Revision: 1}}}
	cancelHub := NewHub()
	cancelService := newTestService(t, cancelReader, cancelHub)
	ctx, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan error, 1)
	go func() {
		_, err := cancelService.Watch(ctx, validCaller(), WatchInput{AfterRevision: 1, Wait: time.Second})
		cancelDone <- err
	}()
	for cancelHub.Subscribers("tenant-a") != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-cancelDone; !errors.Is(err, context.Canceled) || cancelHub.Subscribers("tenant-a") != 0 {
		t.Fatalf("err=%v subscribers=%d", err, cancelHub.Subscribers("tenant-a"))
	}
}

func TestWatchPollsAcrossNodesWithoutLocalNotification(t *testing.T) {
	reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 1}, {Revision: 1}, {Revision: 2, Events: []registry.Event{validEvent(2)}}}}
	hub := NewHub()
	result, err := newTestService(t, reader, hub).Watch(context.Background(), validCaller(), WatchInput{AfterRevision: 1, Wait: time.Second})
	if err != nil || result.Revision != 2 || len(result.Events) != 1 || reader.calls < 3 || hub.Subscribers("tenant-a") != 0 {
		t.Fatalf("result=%+v calls=%d subscribers=%d err=%v", result, reader.calls, hub.Subscribers("tenant-a"), err)
	}
}

func TestWatchHTTPContract(t *testing.T) {
	event := validEvent(2)
	reader := &sequenceReader{windows: []registry.EventWindow{{Revision: 2, Events: []registry.Event{event}}}}
	service := newTestService(t, reader, NewHub())
	handler, err := NewHTTPHandler(service, func(*http.Request) (registryapi.Caller, platform.RequestMetadata, bool) {
		return validCaller(), platform.RequestMetadata{}, true
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/discovery/changes?after_revision=1&wait_seconds=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var body map[string]any
	if json.Unmarshal(response.Body.Bytes(), &body) != nil || body["revision"] != float64(2) {
		t.Fatalf("body=%s", response.Body.String())
	}
	events := body["events"].([]any)
	if _, leaked := events[0].(map[string]any)["tenant_id"]; leaked {
		t.Fatal("tenant_id leaked into public event wire")
	}

	for _, target := range []string{
		"/v1/discovery/changes?after_revision=1&wait_seconds=1&extra=x",
		"/v1/discovery/changes?after_revision=1&after_revision=2&wait_seconds=1",
		"/v1/discovery/changes?after_revision=1&wait_seconds=31",
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, strings.NewReader("")))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("target=%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestWatchHTTPCompactionAndDependencyErrorsAreTypedAndRedacted(t *testing.T) {
	secret := "postgres://user:password@example.invalid/db"
	for name, reader := range map[string]*sequenceReader{
		"compacted":  {windows: []registry.EventWindow{{Revision: 9, CompactionWatermark: 5}}},
		"dependency": {err: errors.New(secret)},
	} {
		t.Run(name, func(t *testing.T) {
			handler, err := NewHTTPHandler(newTestService(t, reader, NewHub()), func(*http.Request) (registryapi.Caller, platform.RequestMetadata, bool) {
				return validCaller(), platform.RequestMetadata{}, true
			})
			if err != nil {
				t.Fatal(err)
			}
			after := "4"
			if name == "dependency" {
				after = "0"
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/discovery/changes?after_revision="+after+"&wait_seconds=1", nil))
			want := http.StatusGone
			if name == "dependency" {
				want = http.StatusServiceUnavailable
			}
			if response.Code != want || strings.Contains(response.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if name == "compacted" && !strings.Contains(response.Body.String(), `"code":"REGISTRY_REVISION_COMPACTED"`) {
				t.Fatalf("body=%s", response.Body.String())
			}
			if name == "dependency" && (response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), `"retry_after_seconds":1`)) {
				t.Fatalf("headers=%v body=%s", response.Header(), response.Body.String())
			}
		})
	}
}
