package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	callbackResolver := newResolver(t, []Binding{validBinding(valueProvider())}, &captureWriter{}, nil)
	var callbackStorage []byte
	err = callbackResolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
		callbackStorage = view.Bytes()
		return errors.New(sentinel)
	})
	if !errors.Is(err, secretports.ErrUnavailable) || containsText(err.Error(), sentinel) {
		t.Fatalf("callback failure leaked details: %v", err)
	}
	assertZeroed(t, callbackStorage)
}

func TestResolverPanicIsSanitizedAndStorageIsZeroed(t *testing.T) {
	resolver := newResolver(t, []Binding{validBinding(valueProvider())}, &captureWriter{}, nil)
	var storage []byte
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = resolver.Use(context.Background(), validRequest(), func(view secretports.SecretView) error {
			storage = view.Bytes()
			panic(sentinel)
		})
	}()
	if !errors.Is(asError(recovered), secretports.ErrUnavailable) || containsText(fmt.Sprint(recovered), sentinel) {
		t.Fatalf("panic was not sanitized: %v", recovered)
	}
	assertZeroed(t, storage)
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
	mutex   sync.Mutex
	entries []observability.AuditEntry
	spans   []observability.SpanRecord
	err     error
}

func (writer *captureWriter) AppendObservation(_ context.Context, entry observability.AuditEntry, span observability.SpanRecord) error {
	if err := observability.ValidateObservationPair(entry, span); err != nil {
		return err
	}
	if writer.err != nil {
		return writer.err
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

func containsText(value, target string) bool {
	for index := 0; index+len(target) <= len(value); index++ {
		if value[index:index+len(target)] == target {
			return true
		}
	}
	return false
}

func asError(value any) error {
	if err, ok := value.(error); ok {
		return err
	}
	return nil
}
