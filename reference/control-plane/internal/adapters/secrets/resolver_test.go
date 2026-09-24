package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	secretports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/secrets"
)

const sentinel = "CANARY-secret-value-never-egresses-8f9e"

var testNow = time.Now().UTC().Truncate(time.Second)

func TestResolverDenyByDefaultAndAuthorizationMatrix(t *testing.T) {
	providerCalls := 0
	resolver := newResolver(t, nil, &captureWriter{}, ProviderFunc(func(context.Context) ([]byte, error) {
		providerCalls++
		return []byte(sentinel), nil
	}))
	if err := resolver.Use(context.Background(), validRequest(), func(secretports.SecretView) error {
		t.Fatal("callback ran without a binding")
		return nil
	}); !errors.Is(err, secretports.ErrDenied) {
		t.Fatalf("empty policy returned %v", err)
	}
	if providerCalls != 0 {
		t.Fatal("provider ran without an authorized binding")
	}

	binding := validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		providerCalls++
		return []byte(sentinel), nil
	}))
	resolver = newResolver(t, []Binding{binding}, &captureWriter{}, binding.Provider)
	cases := map[string]func(*secretports.ResolveRequest){
		"unknown reference":  func(r *secretports.ResolveRequest) { r.Reference = "unknown" },
		"wrong operation":    func(r *secretports.ResolveRequest) { r.Operation = "write" },
		"wrong subject":      func(r *secretports.ResolveRequest) { r.SubjectID = "svc:other" },
		"wrong credential":   func(r *secretports.ResolveRequest) { r.CredentialID = "cred_other" },
		"insufficient scope": func(r *secretports.ResolveRequest) { r.Scopes = []string{"other.read"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			request := validRequest()
			mutate(&request)
			called := false
			if err := resolver.Use(context.Background(), request, func(secretports.SecretView) error {
				called = true
				return nil
			}); !errors.Is(err, secretports.ErrDenied) {
				t.Fatalf("got %v, want generic denial", err)
			}
			if called {
				t.Fatal("callback ran for denied request")
			}
		})
	}
	if providerCalls != 0 {
		t.Fatalf("provider called %d times for denied requests", providerCalls)
	}
}

func TestResolverAllowsOnlyAfterAuditAndZeroesStorage(t *testing.T) {
	writer := &captureWriter{}
	providerStorage := []byte(sentinel)
	resolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		return providerStorage, nil
	}))}, writer, nil)
	var callbackStorage []byte
	err := resolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
		if writer.count() != 1 {
			t.Fatal("secret became visible before durable audit succeeded")
		}
		callbackStorage = view.Bytes()
		if string(callbackStorage) != sentinel {
			t.Fatal("callback did not receive the expected invocation-local value")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	assertZeroed(t, providerStorage)
	assertZeroed(t, callbackStorage)
	entry := writer.entries[0]
	if entry.Operation != auditOperation || entry.Outcome != observability.OutcomeSucceeded {
		t.Fatalf("unsafe or incorrect audit entry: %+v", entry)
	}
	entries, spans := writer.snapshot()
	encoded, err := json.Marshal(struct {
		Entries []observability.AuditEntry
		Spans   []observability.SpanRecord
	}{Entries: entries, Spans: spans})
	if err != nil {
		t.Fatal(err)
	}
	if containsText(string(encoded), sentinel) {
		t.Fatal("secret sentinel escaped into audit capture")
	}
}

func TestResolverAuditFailureFailsClosed(t *testing.T) {
	providerStorage := []byte(sentinel)
	writer := &captureWriter{err: errors.New("audit backend details")}
	resolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		return providerStorage, nil
	}))}, writer, nil)
	called := false
	err := resolver.Use(context.Background(), validRequest(), func(secretports.SecretView) error {
		called = true
		return nil
	})
	if !errors.Is(err, secretports.ErrUnavailable) || called {
		t.Fatalf("audit failure was not fail-closed: err=%v called=%v", err, called)
	}
	assertZeroed(t, providerStorage)
}

func TestResolverProviderAndCallbackErrorsDoNotLeak(t *testing.T) {
	providerStorage := []byte(sentinel)
	providerResolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		return providerStorage, errors.New(sentinel)
	}))}, &captureWriter{}, nil)
	err := providerResolver.Use(context.Background(), validRequest(), func(secretports.SecretView) error { return nil })
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("provider failure leaked details: %v", err)
	}
	assertZeroed(t, providerStorage)

	panicResolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		panic(sentinel)
	}))}, &captureWriter{}, nil)
	err = panicResolver.Use(context.Background(), validRequest(), func(secretports.SecretView) error { return nil })
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("provider panic leaked details: %v", err)
	}

	callbackWriter := &captureWriter{}
	callbackResolver := newResolver(t, []Binding{validBinding(valueProvider())}, callbackWriter, nil)
	var callbackStorage []byte
	err = callbackResolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
		callbackStorage = view.Bytes()
		return errors.New(sentinel)
	})
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("callback failure leaked details: %v", err)
	}
	assertZeroed(t, callbackStorage)
	assertCallbackFailureAudit(t, callbackWriter)
}

func TestResolverPanicIsSanitizedAndStorageIsZeroed(t *testing.T) {
	writer := &captureWriter{}
	resolver := newResolver(t, []Binding{validBinding(valueProvider())}, writer, nil)
	var storage []byte
	err := resolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
		storage = view.Bytes()
		panic(sentinel)
	})
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("panic was not converted to a safe error: %v", err)
	}
	assertZeroed(t, storage)
	assertCallbackFailureAudit(t, writer)
}

func TestResolverCallbackFailureAuditFailureFailsClosedAndZeroesStorage(t *testing.T) {
	writer := &captureWriter{failAt: 2}
	resolver := newResolver(t, []Binding{validBinding(valueProvider())}, writer, nil)
	var storage []byte
	err := resolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
		storage = view.Bytes()
		return errors.New(sentinel)
	})
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("callback failure audit error was not sanitized: %v", err)
	}
	assertZeroed(t, storage)
	entries, spans := writer.snapshot()
	if len(entries) != 1 || len(spans) != 1 || entries[0].Operation != auditOperation || entries[0].Outcome != observability.OutcomeSucceeded {
		t.Fatalf("unexpected persisted audit prefix after second append failed: entries=%+v spans=%+v", entries, spans)
	}
	assertNoSecretInObservations(t, entries, spans)
}

func TestResolverDeadlineCancellationAndExpiry(t *testing.T) {
	providerCalls := 0
	binding := validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		providerCalls++
		return []byte(sentinel), nil
	}))
	resolver := newResolver(t, []Binding{binding}, &captureWriter{}, nil)

	expired := validRequest()
	expired.Deadline = testNow
	if err := resolver.Use(context.Background(), expired, func(secretports.SecretView) error { return nil }); !errors.Is(err, secretports.ErrDenied) {
		t.Fatalf("expired request returned %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resolver.Use(cancelled, validRequest(), func(secretports.SecretView) error { return nil }); !errors.Is(err, secretports.ErrDenied) {
		t.Fatalf("cancelled request returned %v", err)
	}

	binding.ExpiresAt = testNow
	resolver = newResolver(t, []Binding{binding}, &captureWriter{}, nil)
	if err := resolver.Use(context.Background(), validRequest(), func(secretports.SecretView) error { return nil }); !errors.Is(err, secretports.ErrDenied) {
		t.Fatalf("expired binding returned %v", err)
	}
	if providerCalls != 0 {
		t.Fatalf("provider called %d times on expired paths", providerCalls)
	}

	timeoutBinding := validBinding(ProviderFunc(func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	resolver = newResolver(t, []Binding{timeoutBinding}, &captureWriter{}, nil)
	timed := validRequest()
	timed.Deadline = time.Now().UTC().Add(20 * time.Millisecond)
	if err := resolver.Use(context.Background(), timed, func(secretports.SecretView) error { return nil }); !errors.Is(err, secretports.ErrDenied) {
		t.Fatalf("deadline expiration returned %v", err)
	}
}

func TestResolverRechecksAuthorizationWindowBeforeExposure(t *testing.T) {
	t.Run("caller cancels while provider runs", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		providerStorage := []byte(sentinel)
		binding := validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
			cancel()
			return providerStorage, nil
		}))
		resolver := newResolver(t, []Binding{binding}, &captureWriter{}, nil)
		called := false
		err := resolver.Use(ctx, validRequest(), func(secretports.SecretView) error {
			called = true
			return nil
		})
		if !errors.Is(err, secretports.ErrDenied) || called {
			t.Fatalf("cancellation was not fail-closed: err=%v called=%v", err, called)
		}
		assertZeroed(t, providerStorage)
	})

	t.Run("binding expires while provider runs", func(t *testing.T) {
		clock := &mutableClock{now: testNow}
		providerStorage := []byte(sentinel)
		binding := validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
			clock.set(testNow.Add(2 * time.Hour))
			return providerStorage, nil
		}))
		binding.ExpiresAt = testNow.Add(time.Hour)
		resolver := newResolverWithClock(t, []Binding{binding}, &captureWriter{}, clock)
		request := validRequest()
		request.Deadline = testNow.Add(3 * time.Hour)
		called := false
		err := resolver.Use(context.Background(), request, func(secretports.SecretView) error {
			called = true
			return nil
		})
		if !errors.Is(err, secretports.ErrDenied) || called {
			t.Fatalf("expired binding was not fail-closed: err=%v called=%v", err, called)
		}
		assertZeroed(t, providerStorage)
	})

	t.Run("audit blocks across request deadline", func(t *testing.T) {
		providerStorage := []byte(sentinel)
		writer := &captureWriter{hook: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		resolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
			return providerStorage, nil
		}))}, writer, nil)
		request := validRequest()
		request.Deadline = time.Now().UTC().Add(20 * time.Millisecond)
		called := false
		err := resolver.Use(context.Background(), request, func(secretports.SecretView) error {
			called = true
			return nil
		})
		if !errors.Is(err, secretports.ErrDenied) || called {
			t.Fatalf("audit deadline was not fail-closed: err=%v called=%v", err, called)
		}
		assertZeroed(t, providerStorage)
	})

	t.Run("binding expires while audit runs", func(t *testing.T) {
		clock := &mutableClock{now: testNow}
		providerStorage := []byte(sentinel)
		bindingExpiry := testNow.Add(time.Hour)
		writer := &captureWriter{hook: func(context.Context) error {
			clock.set(bindingExpiry)
			return nil
		}}
		binding := validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
			return providerStorage, nil
		}))
		binding.ExpiresAt = bindingExpiry
		resolver := newResolverWithClock(t, []Binding{binding}, writer, clock)
		request := validRequest()
		request.Deadline = testNow.Add(2 * time.Hour)
		called := false
		err := resolver.Use(context.Background(), request, func(secretports.SecretView) error {
			called = true
			return nil
		})
		if !errors.Is(err, secretports.ErrDenied) || called {
			t.Fatalf("post-audit expiry was not fail-closed: err=%v called=%v", err, called)
		}
		assertZeroed(t, providerStorage)
	})
}

func TestResolverConcurrentCallsUseIsolatedCopies(t *testing.T) {
	var providerCalls atomic.Int64
	resolver := newResolver(t, []Binding{validBinding(ProviderFunc(func(context.Context) ([]byte, error) {
		providerCalls.Add(1)
		return []byte(sentinel), nil
	}))}, &captureWriter{}, nil)
	const count = 16
	addresses := make(chan uintptr, count)
	errorsSeen := make(chan error, count)
	started := make(chan struct{}, count)
	release := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- resolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
				value := view.Bytes()
				if string(value) != sentinel {
					return errors.New("copy was corrupted")
				}
				addresses <- uintptr(unsafe.Pointer(&value[0]))
				started <- struct{}{}
				<-release
				value[0] = 'x'
				return nil
			})
		}()
	}
	for index := 0; index < count; index++ {
		<-started
	}
	close(release)
	wait.Wait()
	close(errorsSeen)
	close(addresses)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent resolve failed: %v", err)
		}
	}
	unique := map[uintptr]struct{}{}
	for address := range addresses {
		unique[address] = struct{}{}
	}
	if len(unique) != count || providerCalls.Load() != count {
		t.Fatalf("copies were shared: unique=%d provider_calls=%d", len(unique), providerCalls.Load())
	}
}

func TestNewRejectsImplicitOrIncompleteBindings(t *testing.T) {
	base := validBinding(valueProvider())
	cases := []Binding{
		{},
		withBinding(base, func(b *Binding) { b.AllowedOperations = nil }),
		withBinding(base, func(b *Binding) { b.AllowedSubjects = nil }),
		withBinding(base, func(b *Binding) { b.AllowedCredentials = nil }),
		withBinding(base, func(b *Binding) { b.RequiredScopes = nil }),
		withBinding(base, func(b *Binding) { b.Provider = nil }),
	}
	for index, binding := range cases {
		if _, err := New(Config{Bindings: []Binding{binding}, Clock: fixedClock{}, IDs: fixedIDs{}, Observability: &captureWriter{}}); err == nil {
			t.Fatalf("case %d accepted an incomplete binding", index)
		}
	}
	if _, err := New(Config{Bindings: []Binding{base, base}, Clock: fixedClock{}, IDs: fixedIDs{}, Observability: &captureWriter{}}); err == nil {
		t.Fatal("duplicate binding accepted")
	}
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return testNow }

type mutableClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (clock *mutableClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *mutableClock) set(now time.Time) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.now = now
}

type fixedIDs struct{}

func (fixedIDs) NewID(_ context.Context, kind platformports.IDKind) (string, error) {
	switch kind {
	case platformports.IDAudit:
		return "aud_01890f28-70d2-7cc0-98c4-dc0c0c07398f", nil
	case platformports.IDSpan:
		return "1110464cfa66635c", nil
	default:
		return "", errors.New("unsupported test ID kind")
	}
}

type captureWriter struct {
	mutex    sync.Mutex
	entries  []observability.AuditEntry
	spans    []observability.SpanRecord
	err      error
	hook     func(context.Context) error
	attempts atomic.Int64
	failAt   int64
}

func (writer *captureWriter) AppendObservation(ctx context.Context, entry observability.AuditEntry, span observability.SpanRecord) error {
	attempt := writer.attempts.Add(1)
	if err := observability.ValidateObservationPair(entry, span); err != nil {
		return err
	}
	if writer.hook != nil {
		if err := writer.hook(ctx); err != nil {
			return err
		}
	}
	if writer.err != nil {
		return writer.err
	}
	if writer.failAt != 0 && attempt == writer.failAt {
		return errors.New("injected observation failure")
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	writer.entries = append(writer.entries, entry)
	writer.spans = append(writer.spans, span)
	return nil
}

func (writer *captureWriter) count() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return len(writer.entries)
}

func (writer *captureWriter) snapshot() ([]observability.AuditEntry, []observability.SpanRecord) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return append([]observability.AuditEntry(nil), writer.entries...), append([]observability.SpanRecord(nil), writer.spans...)
}

func validRequest() secretports.ResolveRequest {
	return secretports.ResolveRequest{
		Reference: "billing-api", Operation: "read", SubjectID: "svc:runtime",
		CredentialID: "cred_01", Scopes: []string{"secret.read", "billing.read"},
		Deadline:     testNow.Add(time.Hour),
		RequestID:    "req_01890f28-70d2-7cc0-98c4-dc0c0c07398f",
		TraceID:      "80e1afed08e019fc1110464cfa66635c",
		ParentSpanID: "0057d9f67cbf6a96",
	}
}

func validBinding(provider Provider) Binding {
	return Binding{
		Reference: "billing-api", AllowedOperations: []string{"read"},
		AllowedSubjects: []string{"svc:runtime"}, AllowedCredentials: []string{"cred_01"},
		RequiredScopes: []string{"secret.read"}, ExpiresAt: testNow.Add(2 * time.Hour),
		Provider: provider,
	}
}

func valueProvider() Provider {
	return ProviderFunc(func(context.Context) ([]byte, error) { return []byte(sentinel), nil })
}

func newResolver(t *testing.T, bindings []Binding, writer *captureWriter, fallback Provider) *Resolver {
	t.Helper()
	if fallback != nil && len(bindings) == 0 {
		// Empty configuration is intentional: fallback proves there is no ambient
		// or default provider discovery.
	}
	resolver, err := New(Config{Bindings: bindings, Clock: fixedClock{}, IDs: fixedIDs{}, Observability: writer})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func newResolverWithClock(t *testing.T, bindings []Binding, writer *captureWriter, clock platformports.Clock) *Resolver {
	t.Helper()
	resolver, err := New(Config{Bindings: bindings, Clock: clock, IDs: fixedIDs{}, Observability: writer})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func withBinding(base Binding, change func(*Binding)) Binding {
	copy := cloneBinding(base)
	change(&copy)
	return copy
}

func assertZeroed(t *testing.T, value []byte) {
	t.Helper()
	for index, item := range value {
		if item != 0 {
			t.Fatalf("secret storage byte %d was not zeroed", index)
		}
	}
}

func assertCallbackFailureAudit(t *testing.T, writer *captureWriter) {
	t.Helper()
	entries, spans := writer.snapshot()
	if len(entries) != 2 || len(spans) != 2 {
		t.Fatalf("callback failure audit sequence length mismatch: entries=%d spans=%d", len(entries), len(spans))
	}
	if entries[0].Operation != auditOperation || entries[0].Outcome != observability.OutcomeSucceeded || entries[0].HTTPStatus != 200 {
		t.Fatalf("callback pre-exposure observation is not stable: %+v", entries[0])
	}
	if entries[1].Operation != auditCallbackFailureOperation || entries[1].Outcome != observability.OutcomeFailed || entries[1].HTTPStatus != 500 {
		t.Fatalf("callback failure observation is not stable: %+v", entries[1])
	}
	assertNoSecretInObservations(t, entries, spans)
}

func assertNoSecretInObservations(t *testing.T, entries []observability.AuditEntry, spans []observability.SpanRecord) {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Entries []observability.AuditEntry
		Spans   []observability.SpanRecord
	}{Entries: entries, Spans: spans})
	if err != nil {
		t.Fatal(err)
	}
	if containsText(string(encoded), sentinel) {
		t.Fatal("secret sentinel escaped into callback observations")
	}
}

func containsText(value, target string) bool {
	for index := 0; index+len(target) <= len(value); index++ {
		if value[index:index+len(target)] == target {
			return true
		}
	}
	return false
}
