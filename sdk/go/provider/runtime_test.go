package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

var runtimeNow = time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC)

func TestRuntimeDurableCreateReplayEffectAndSecretBoundary(t *testing.T) {
	store := newMemoryStore()
	var calls atomic.Int64
	handler := HandlerFunc(func(ctx context.Context, execution *Execution, request runwire.RunRequest) (runwire.RunResult, error) {
		result, err := execution.Effect(ctx, "eff_order_0001", json.RawMessage(`{"amount":1}`), func(context.Context) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"receipt":"r-1"}`), nil
		})
		if err != nil || string(result) != `{"receipt":"r-1"}` {
			return runwire.RunResult{}, errors.New("effect failed")
		}
		return successResult(validClaims().RunID, runtimeNow.Add(time.Second)), nil
	})
	runtime := newTestRuntime(t, store, handler)
	body := validRunRequest(t)
	response := invokeRuntime(runtime, http.MethodPost, "/v1/runs", body, map[string]string{"Authorization": "Bearer secret-token-000000", "Content-Type": "application/json", "Idempotency-Key": validClaims().AttemptID})
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	record := waitTerminal(t, store)
	if record.State != InboxSucceeded || calls.Load() != 1 {
		t.Fatalf("unexpected terminal state=%s effect calls=%d", record.State, calls.Load())
	}
	serialized, _ := json.Marshal(record)
	if bytes.Contains(serialized, []byte("secret-token")) {
		t.Fatal("bearer token reached durable inbox")
	}
	response = invokeRuntime(runtime, http.MethodPost, "/v1/runs", body, map[string]string{"Authorization": "Bearer secret-token-000000", "Content-Type": "application/json", "Idempotency-Key": validClaims().AttemptID})
	if response.Code != http.StatusAccepted || calls.Load() != 1 {
		t.Fatalf("idempotent replay executed again: status=%d calls=%d", response.Code, calls.Load())
	}
	response = invokeRuntime(runtime, http.MethodPost, "/v1/runs", append([]byte(nil), bytes.Replace(body, []byte("hello"), []byte("changed"), 1)...), map[string]string{"Authorization": "Bearer secret-token-000000", "Content-Type": "application/json", "Idempotency-Key": validClaims().AttemptID})
	if response.Code != http.StatusConflict {
		t.Fatalf("mismatched replay status=%d", response.Code)
	}
	outbox, err := store.ListOutbox(context.Background(), record.RunID, record.AttemptID, 0, 10)
	if err != nil || len(outbox) != 3 || outbox[0].Sequence != 1 || outbox[2].Sequence != 3 {
		t.Fatalf("unexpected outbox: %#v %v", outbox, err)
	}
}

func TestRuntimeCancelAndEffectUncertainFailClosed(t *testing.T) {
	store := newMemoryStore()
	started := make(chan struct{})
	handler := HandlerFunc(func(ctx context.Context, execution *Execution, request runwire.RunRequest) (runwire.RunResult, error) {
		close(started)
		<-ctx.Done()
		return runwire.RunResult{}, ctx.Err()
	})
	runtime := newTestRuntime(t, store, handler)
	response := invokeRuntime(runtime, http.MethodPost, "/v1/runs", validRunRequest(t), map[string]string{"Authorization": "Bearer secret-token-000000", "Content-Type": "application/json", "Idempotency-Key": validClaims().AttemptID})
	if response.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d", response.Code)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	record, _ := store.GetInbox(context.Background(), validClaims().RunID, validClaims().AttemptID)
	command := []byte(`{"schema_version":1,"command_id":"cmd_01999999-9999-7999-8999-999999999996","expected_state_version":` + jsonNumber(record.StateVersion) + `,"type":"run.cancel","data":{}}`)
	response = invokeRuntime(runtime, http.MethodPost, "/v1/runs/"+record.RunID+"/commands", command, map[string]string{"Authorization": "Bearer secret-token-000000", "Content-Type": "application/json"})
	if response.Code != http.StatusAccepted {
		t.Fatalf("cancel failed: %d %s", response.Code, response.Body.String())
	}
	record = waitTerminal(t, store)
	if record.State != InboxCancelled || record.CancelRequestedAt == nil {
		t.Fatalf("cancel not durable: %#v", record)
	}

	store = newMemoryStore()
	now := runtimeNow
	inbox := InboxRecord{RunID: validClaims().RunID, AttemptID: validClaims().AttemptID, State: InboxRunning}
	store.effects["eff_order_0002"] = EffectRecord{EffectID: "eff_order_0002", RunID: inbox.RunID, AttemptID: inbox.AttemptID, RequestDigest: sha256Digest([]byte(`{"amount":2}`)), State: EffectStarted, StartedAt: now, UpdatedAt: now}
	execution := &Execution{store: store, record: inbox, clock: func() time.Time { return now }}
	called := false
	_, err := execution.Effect(context.Background(), "eff_order_0002", json.RawMessage(`{"amount":2}`), func(context.Context) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`{}`), nil
	})
	if !errors.Is(err, ErrEffectUncertain) || called {
		t.Fatalf("uncertain effect was not fenced: err=%v called=%v", err, called)
	}
}

func TestRuntimeRejectsAuthenticationHeadersAndExpiredDeadline(t *testing.T) {
	runtime := newTestRuntime(t, newMemoryStore(), HandlerFunc(func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error) {
		return runwire.RunResult{}, nil
	}))
	for _, test := range []struct {
		name    string
		headers http.Header
		body    []byte
		status  int
	}{
		{name: "missing bearer", headers: http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {validClaims().AttemptID}}, body: validRunRequest(t), status: 401},
		{name: "duplicate bearer", headers: http.Header{"Authorization": {"Bearer secret-token-000000", "Bearer second-token-00000"}, "Content-Type": {"application/json"}, "Idempotency-Key": {validClaims().AttemptID}}, body: validRunRequest(t), status: 401},
		{name: "duplicate content type", headers: http.Header{"Authorization": {"Bearer secret-token-000000"}, "Content-Type": {"application/json", "application/json"}, "Idempotency-Key": {validClaims().AttemptID}}, body: validRunRequest(t), status: 415},
		{name: "wrong attempt", headers: http.Header{"Authorization": {"Bearer secret-token-000000"}, "Content-Type": {"application/json"}, "Idempotency-Key": {"att_01999999-9999-7999-8999-999999999990"}}, body: validRunRequest(t), status: 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(test.body))
			request.Header = test.headers
			response := httptest.NewRecorder()
			runtime.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

type staticVerifier struct{ claims Claims }

func (verifier staticVerifier) Verify(_ context.Context, token string, request VerifyRequest) (Claims, error) {
	if token != "secret-token-000000" || request.RequiredScope != "agent:invoke" {
		return Claims{}, errors.New("invalid token")
	}
	return verifier.claims, nil
}

func newTestRuntime(t *testing.T, store DurableStore, handler Handler) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(Config{Store: store, Verifier: staticVerifier{claims: validClaims()}, Handler: handler, Clock: func() time.Time { return runtimeNow }})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func validClaims() Claims {
	return Claims{
		Issuer: "https://control.example.invalid", Audience: "https://control.example.invalid/deployments/dep_01999999-9999-7999-8999-999999999997",
		Subject: "prn_01999999-9999-7999-8999-999999999995", AuthorizedParty: "cred_01999999-9999-7999-8999-999999999994", TokenID: "tok_01999999-9999-7999-8999-999999999993",
		RunID: "run_01999999-9999-7999-8999-999999999999", AttemptID: "att_01999999-9999-7999-8999-999999999998",
		AgentID: "test.agent", AgentVersion: "1.0.0", SkillID: "default", DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a",
		TransportProfile: "direct", Endpoint: "https://runtime.example.invalid/v1/runs", Generation: 1, FencingToken: 1, Scopes: []string{"agent:invoke"},
		IssuedAt: runtimeNow.Add(-time.Minute), ExpiresAt: runtimeNow.Add(4 * time.Minute),
	}
}

func validRunRequest(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"schema_version":1,"agent":{"id":"test.agent","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"text","text":"hello"}],"deadline_at":"2026-09-27T06:30:00Z","effects":{"level":"write","effect_id":"eff_order_0001"},"trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}`)
}

func successResult(runID string, now time.Time) runwire.RunResult {
	return runwire.RunResult{
		SchemaVersion: 1, RunID: runwire.RunId(runID), State: "succeeded", CompletedAt: runwire.DateTime(now.Format(time.RFC3339Nano)),
		Snapshot: &runwire.Snapshot{Revision: 1, Digest: runwire.Sha256Digest("sha256:" + string(bytes.Repeat([]byte("b"), 64))), Content: []runwire.AROPV1ContentPart{{Text: &runwire.AROPV1ContentPartText{Type: "text", Text: "done"}}}},
		Usage:    runwire.Usage{InputTokens: 1, OutputTokens: 1, DurationMs: 1},
	}
}

func invokeRuntime(runtime *Runtime, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	runtime.ServeHTTP(response, request)
	return response
}

func waitTerminal(t *testing.T, store *memoryStore) InboxRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		record, err := store.GetInbox(context.Background(), validClaims().RunID, validClaims().AttemptID)
		if err == nil && record.State.Terminal() {
			return record
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("attempt did not reach terminal state")
	return InboxRecord{}
}

func jsonNumber(value uint64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

type memoryStore struct {
	mu      sync.Mutex
	inbox   map[string]InboxRecord
	effects map[string]EffectRecord
	outbox  map[string][]OutboxRecord
}

func newMemoryStore() *memoryStore {
	return &memoryStore{inbox: map[string]InboxRecord{}, effects: map[string]EffectRecord{}, outbox: map[string][]OutboxRecord{}}
}

func (store *memoryStore) Within(ctx context.Context, callback func(context.Context, Transaction) error) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	backupInbox, backupEffects, backupOutbox := cloneInbox(store.inbox), cloneEffects(store.effects), cloneOutbox(store.outbox)
	if err := callback(ctx, memoryTx{store: store}); err != nil {
		store.inbox, store.effects, store.outbox = backupInbox, backupEffects, backupOutbox
		return err
	}
	return nil
}

func (store *memoryStore) GetInbox(_ context.Context, runID, attemptID string) (InboxRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return memoryTx{store: store}.GetInbox(context.Background(), runID, attemptID)
}
func (store *memoryStore) ListRecoverable(context.Context, time.Time, int) ([]InboxRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	values := []InboxRecord{}
	for _, value := range store.inbox {
		if !value.State.Terminal() {
			values = append(values, value)
		}
	}
	return values, nil
}
func (store *memoryStore) ListOutbox(_ context.Context, runID, attemptID string, after uint64, limit int) ([]OutboxRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	values := store.outbox[runID+"/"+attemptID]
	result := []OutboxRecord{}
	for _, value := range values {
		if value.Sequence > after && len(result) < limit {
			result = append(result, value)
		}
	}
	return result, nil
}
func (*memoryStore) Ready(context.Context) error { return nil }

type memoryTx struct{ store *memoryStore }

func (transaction memoryTx) GetInbox(_ context.Context, runID, attemptID string) (InboxRecord, error) {
	value, ok := transaction.store.inbox[runID+"/"+attemptID]
	if !ok {
		return InboxRecord{}, ErrNotFound
	}
	return value, nil
}
func (transaction memoryTx) CreateInbox(_ context.Context, value InboxRecord) error {
	key := value.RunID + "/" + value.AttemptID
	if _, exists := transaction.store.inbox[key]; exists {
		return ErrConflict
	}
	transaction.store.inbox[key] = value
	return nil
}
func (transaction memoryTx) UpdateInbox(_ context.Context, value InboxRecord, expected uint64) error {
	key := value.RunID + "/" + value.AttemptID
	current, exists := transaction.store.inbox[key]
	if !exists {
		return ErrNotFound
	}
	if current.StateVersion != expected {
		return ErrConflict
	}
	transaction.store.inbox[key] = value
	return nil
}
func (transaction memoryTx) GetEffect(_ context.Context, effectID string) (EffectRecord, error) {
	value, exists := transaction.store.effects[effectID]
	if !exists {
		return EffectRecord{}, ErrNotFound
	}
	return value, nil
}
func (transaction memoryTx) CreateEffect(_ context.Context, value EffectRecord) error {
	if _, exists := transaction.store.effects[value.EffectID]; exists {
		return ErrConflict
	}
	transaction.store.effects[value.EffectID] = value
	return nil
}
func (transaction memoryTx) CompleteEffect(_ context.Context, effectID, digest string, result json.RawMessage, now time.Time) error {
	value, exists := transaction.store.effects[effectID]
	if !exists || value.RequestDigest != digest || value.State != EffectStarted {
		return ErrConflict
	}
	value.State, value.Result, value.UpdatedAt = EffectCompleted, bytes.Clone(result), now
	transaction.store.effects[effectID] = value
	return nil
}
func (transaction memoryTx) AppendOutbox(_ context.Context, value OutboxRecord) (uint64, error) {
	key := value.RunID + "/" + value.AttemptID
	value.Sequence = uint64(len(transaction.store.outbox[key]) + 1)
	transaction.store.outbox[key] = append(transaction.store.outbox[key], value)
	return value.Sequence, nil
}
func (transaction memoryTx) MarkOutboxDelivered(_ context.Context, runID, attemptID string, sequence uint64, delivered time.Time) error {
	key := runID + "/" + attemptID
	if sequence == 0 || int(sequence) > len(transaction.store.outbox[key]) {
		return ErrNotFound
	}
	transaction.store.outbox[key][sequence-1].DeliveredAt = &delivered
	return nil
}

func cloneInbox(source map[string]InboxRecord) map[string]InboxRecord {
	result := map[string]InboxRecord{}
	for key, value := range source {
		result[key] = value
	}
	return result
}
func cloneEffects(source map[string]EffectRecord) map[string]EffectRecord {
	result := map[string]EffectRecord{}
	for key, value := range source {
		result[key] = value
	}
	return result
}
func cloneOutbox(source map[string][]OutboxRecord) map[string][]OutboxRecord {
	result := map[string][]OutboxRecord{}
	for key, value := range source {
		result[key] = append([]OutboxRecord(nil), value...)
	}
	return result
}
