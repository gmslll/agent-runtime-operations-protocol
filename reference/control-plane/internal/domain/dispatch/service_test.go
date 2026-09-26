package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

var testNow = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)

func TestDispatchBindsAttemptTicketAuditAndReplay(t *testing.T) {
	service, repository, signer, observations := testService(t)
	request := validDispatchRequest()
	ticket, attempt, err := service.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.AttemptNumber != 1 || attempt.FencingToken != 1 || attempt.State != StateIssued || ticket.AttemptID != attempt.AttemptID || ticket.Delivery.DeploymentID != attempt.DeploymentID || ticket.Delivery.Audience != attempt.Audience || ticket.RunToken == "" {
		t.Fatalf("dispatch binding drifted: ticket=%#v attempt=%#v", ticket, attempt)
	}
	key, err := signer.ActiveKey(context.Background(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyToken(ticket.RunToken, key, verifyExpectation(attempt, testNow))
	if err != nil || claims.TokenID != attempt.TokenID || claims.AgentID != "image.generate" || claims.SkillID != "default" {
		t.Fatalf("claims mismatch: %#v %v", claims, err)
	}
	// Reserving the Attempt moves the durable Run from queued/version 1 to
	// dispatching/version 2. That service-owned transition must not turn an
	// otherwise identical idempotent retry into a request conflict.
	view := service.deps.Runs.(fakeRuns).view
	view.State, view.StateVersion = run.StateDispatching, 2
	service.deps.Runs = fakeRuns{view: view}
	replayTicket, replayAttempt, err := service.Dispatch(context.Background(), request)
	if err != nil || replayAttempt.AttemptID != attempt.AttemptID || replayTicket.AttemptID != attempt.AttemptID || repository.reserveCalls != 1 {
		t.Fatalf("idempotent replay failed: %#v %#v calls=%d err=%v", replayTicket, replayAttempt, repository.reserveCalls, err)
	}
	if _, err := signer.Rotate(testNow.Add(time.Second), "key-20260927-next", time.Hour, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	service.deps.Clock = fixedClock{testNow.Add(2 * time.Second)}
	rotatedTicket, rotatedAttempt, err := service.Dispatch(context.Background(), request)
	if err != nil || rotatedAttempt.AttemptID != attempt.AttemptID || rotatedTicket.RunToken == ticket.RunToken || repository.reserveCalls != 1 {
		t.Fatalf("rotated idempotent replay failed: %#v %#v calls=%d err=%v", rotatedTicket, rotatedAttempt, repository.reserveCalls, err)
	}
	active, err := signer.ActiveKey(context.Background(), testNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyToken(rotatedTicket.RunToken, active, verifyExpectation(attempt, testNow.Add(2*time.Second))); err != nil {
		t.Fatalf("rotated replay token rejected: %v", err)
	}
	if _, err := service.JWKS(context.Background()); err != nil {
		t.Fatalf("rotated JWKS unavailable: %v", err)
	}
	if observations.count != 3 {
		t.Fatalf("durable observation count=%d", observations.count)
	}
	keys, err := repository.Keys(context.Background(), testNow)
	if err != nil || len(keys) != 2 {
		t.Fatalf("public signing metadata missing: %#v %v", keys, err)
	}
}

func TestDispatchDoesNotReplayClosedAttemptCapability(t *testing.T) {
	service, repository, _, observations := testService(t)
	request := validDispatchRequest()
	_, attempt, err := service.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	view := service.deps.Runs.(fakeRuns).view
	view.State, view.StateVersion = run.StateDispatching, 2
	service.deps.Runs = fakeRuns{view: view}
	closed := testNow.Add(time.Second)
	repository.mutex.Lock()
	for key, stored := range repository.attempts {
		if stored.AttemptID == attempt.AttemptID {
			stored.State = StateFenced
			stored.ClosedAt = &closed
			stored.FailureCode = "INSTANCE_GENERATION_FENCED"
			repository.attempts[key] = stored
		}
	}
	repository.mutex.Unlock()
	if _, _, err = service.Dispatch(context.Background(), request); err == nil {
		t.Fatal("fenced Attempt replay minted a new capability")
	} else if failure, ok := AsError(err); !ok || failure.Category != CategoryConflict || failure.Reason != ReasonAttemptFenced {
		t.Fatalf("fenced replay error=%#v", err)
	}
	if observations.count != 2 {
		t.Fatalf("durable observations=%d want=2", observations.count)
	}
}

func TestDispatchObservationFailureLatchesReadinessUntilAuditedRecovery(t *testing.T) {
	service, _, _, observations := testService(t)
	observations.failure = true
	invalid := validDispatchRequest()
	invalid.RunID = "invalid"
	if _, _, err := service.Dispatch(context.Background(), invalid); err == nil {
		t.Fatal("observation failure accepted")
	}
	observations.failure = false
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("JWKS success erased the durable observation failure latch")
	}
	if _, _, err := service.Dispatch(context.Background(), validDispatchRequest()); err != nil {
		t.Fatalf("audited recovery dispatch failed: %v", err)
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatalf("readiness did not recover after a successful audited dispatch: %v", err)
	}
}

func TestDispatchSelectionCapacityAndAuthorizationFailClosed(t *testing.T) {
	service, repository, _, observations := testService(t)
	repository.rejectInstance = "preferred-a"
	ticket, attempt, err := service.Dispatch(context.Background(), validDispatchRequest())
	if err != nil || attempt.InstanceID != "fallback-b" || ticket.Delivery.InstanceID != "fallback-b" || repository.reserveCalls != 2 {
		t.Fatalf("candidate fallback failed: %#v %#v calls=%d err=%v", ticket, attempt, repository.reserveCalls, err)
	}
	request := validDispatchRequest()
	request.IdempotencyKey = "different-key"
	service.deps.Candidates = fakeCandidates{}
	_, _, err = service.Dispatch(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryCapacity || failure.Reason != ReasonNoCapacity || !failure.Retryable {
		t.Fatalf("no-capacity error=%#v", err)
	}
	request = validDispatchRequest()
	request.IdempotencyKey = "third-key"
	request.Caller.Scopes = []string{"run:read"}
	_, _, err = service.Dispatch(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryAuthorization || failure.Reason != ReasonDispatchForbidden {
		t.Fatalf("authorization error=%#v", err)
	}
	if observations.count != 3 {
		t.Fatalf("rejection observations=%d", observations.count)
	}
}

func TestSignerRotationOverlapAndTokenVerification(t *testing.T) {
	signer, err := NewProcessSigner(testNow.Add(-time.Minute), "key-before", time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := signer.ActiveKey(context.Background(), testNow)
	deploymentID := uuid("dep_", "06")
	claims := TokenClaims{Issuer: "https://control.example.invalid", Audience: "https://control.example.invalid/deployments/" + deploymentID, Subject: uuid("prn_", "01"), AuthorizedParty: uuid("cred_", "02"), TokenID: uuid("tok_", "03"), RunID: uuid("run_", "04"), AttemptID: uuid("att_", "05"), AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", DeploymentID: deploymentID, InstanceID: "runtime-a", TransportProfile: "direct", Endpoint: "https://runtime.example.invalid/v1/runs", Generation: 1, FencingToken: 1, Scopes: []string{"agent:invoke", "run:stream"}, IssuedAt: testNow.Unix(), NotBefore: testNow.Unix(), ExpiresAt: testNow.Add(5 * time.Minute).Unix()}
	token, err := issueToken(context.Background(), signer, old, claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = issueToken(context.Background(), invalidSignatureSigner{Signer: signer}, old, claims); err == nil {
		t.Fatal("Signer metadata/private-key mismatch issued an unverifiable ticket")
	}
	if _, err = signer.Rotate(testNow.Add(time.Second), "key-after", time.Hour, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	keys, err := signer.VerificationKeys(context.Background(), testNow.Add(2*time.Second))
	if err != nil || len(keys) != 2 || keys[0].Status != KeyActive && keys[1].Status != KeyActive {
		t.Fatalf("rotation set=%#v err=%v", keys, err)
	}
	expected := VerifyExpectation{Issuer: claims.Issuer, Audience: claims.Audience, RunID: claims.RunID, AttemptID: claims.AttemptID, AgentID: claims.AgentID, AgentVersion: claims.AgentVersion, SkillID: claims.SkillID, DeploymentID: claims.DeploymentID, InstanceID: claims.InstanceID, TransportProfile: claims.TransportProfile, Endpoint: claims.Endpoint, Generation: 1, FencingToken: 1, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), RequiredScope: "agent:invoke", Now: testNow.Add(2 * time.Second)}
	if _, err = VerifyToken(token, keys[keyIndex(keys, old.KeyID)], expected); err != nil {
		t.Fatalf("overlap key rejected old ticket: %v", err)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*VerifyExpectation)
	}{
		{"audience", func(value *VerifyExpectation) { value.Audience = "https://wrong.example.invalid" }},
		{"endpoint", func(value *VerifyExpectation) { value.Endpoint = "https://other.example.invalid/v1/runs" }},
		{"transport", func(value *VerifyExpectation) { value.TransportProfile = "proxy" }},
		{"agent", func(value *VerifyExpectation) { value.AgentID = "other.agent" }},
		{"generation", func(value *VerifyExpectation) { value.Generation++ }},
		{"fencing", func(value *VerifyExpectation) { value.FencingToken++ }},
		{"expiry", func(value *VerifyExpectation) { value.Now = value.ExpiresAt }},
	} {
		t.Run("reject-"+scenario.name, func(t *testing.T) {
			mismatch := expected
			scenario.mutate(&mismatch)
			if _, verifyErr := VerifyToken(token, old, mismatch); verifyErr == nil {
				t.Fatalf("%s mismatch accepted", scenario.name)
			}
		})
	}
	parts := strings.Split(token, ".")
	replacement := byte('A')
	if parts[2][0] == replacement {
		replacement = 'B'
	}
	parts[2] = string(replacement) + parts[2][1:]
	tampered := strings.Join(parts, ".")
	expected.Now = testNow
	if _, err = VerifyToken(tampered, old, expected); err == nil {
		t.Fatal("tampered token accepted")
	}
}

type invalidSignatureSigner struct{ Signer }

func (invalidSignatureSigner) Sign(context.Context, string, []byte) ([]byte, error) {
	return make([]byte, 64), nil
}

func TestJWKSContainsOnlyPublicMetadataAndReadinessFailsClosed(t *testing.T) {
	service, repository, _, _ := testService(t)
	jwks, err := service.JWKS(context.Background())
	if err != nil || len(jwks.Keys) != 1 || jwks.Keys[0].X == "" || jwks.Keys[0].Y == "" || jwks.CacheUntil.After(jwks.Keys[0].VerifyUntil) {
		t.Fatalf("JWKS=%#v err=%v", jwks, err)
	}
	repository.failure = true
	if _, err = service.JWKS(context.Background()); err == nil {
		t.Fatal("repository failure accepted")
	}
	if err = service.Check(context.Background()); err == nil {
		t.Fatal("readiness stayed healthy")
	}
}

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type fakeIDs struct{ next int }

func (source *fakeIDs) value(prefix string) string {
	source.next++
	hex := "0123456789abcdef"
	return uuid(prefix, string(hex[source.next%len(hex)]))
}
func (source *fakeIDs) NewAttemptID(context.Context) (string, error) {
	return source.value("att_"), nil
}
func (source *fakeIDs) NewDeploymentID(context.Context) (string, error) {
	return source.value("dep_"), nil
}
func (source *fakeIDs) NewTokenID(context.Context) (string, error) { return source.value("tok_"), nil }
func (source *fakeIDs) NewAuditID(context.Context) (string, error) { return source.value("aud_"), nil }

type fakeUoW struct{}

func (fakeUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return callback(ctx)
}

type fakeObservations struct {
	count   int
	failure bool
}

func (store *fakeObservations) AppendObservation(_ context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if store.failure {
		return errors.New("observation unavailable")
	}
	if err := observability.ValidateObservationPair(audit, span); err != nil {
		return err
	}
	store.count++
	return nil
}

type fakeRuns struct{ view RunView }

func (runs fakeRuns) DispatchableRun(_ context.Context, tenantID, runID string) (RunView, error) {
	if tenantID != runs.view.TenantID || runID != runs.view.RunID {
		return RunView{}, NewError(CategoryNotFound, ReasonRunNotFound)
	}
	return runs.view, nil
}

type fakeCandidates struct{ values []Candidate }

func (source fakeCandidates) Candidates(context.Context, RunView) ([]Candidate, error) {
	return append([]Candidate(nil), source.values...), nil
}

type fakeAuthorizer struct{}

func (fakeAuthorizer) Authorize(_ context.Context, caller Caller, operation Operation, _ run.AgentBinding) error {
	if operation != OperationIssue || !slicesContains(caller.Scopes, "run:dispatch") {
		return NewError(CategoryAuthorization, ReasonDispatchForbidden)
	}
	return nil
}

type fakeRepository struct {
	mutex          sync.Mutex
	attempts       map[string]Attempt
	requestDigests map[string]string
	keys           map[string]KeyMetadata
	reserveCalls   int
	rejectInstance string
	failure        bool
}

func (repository *fakeRepository) GetByIdempotency(_ context.Context, tenantID, key string) (Attempt, string, error) {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if repository.failure {
		return Attempt{}, "", errors.New("storage unavailable")
	}
	attempt, ok := repository.attempts[tenantID+"/"+key]
	if !ok {
		return Attempt{}, "", NewError(CategoryNotFound, ReasonRunNotFound)
	}
	return attempt, repository.requestDigests[tenantID+"/"+key], nil
}

func (repository *fakeRepository) Reserve(_ context.Context, command ReserveCommand) (Attempt, bool, error) {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	repository.reserveCalls++
	if repository.failure {
		return Attempt{}, false, errors.New("storage unavailable")
	}
	if command.Candidate.InstanceID == repository.rejectInstance {
		return Attempt{}, false, NewError(CategoryCapacity, ReasonNoCapacity)
	}
	for _, key := range command.SigningKeys {
		repository.keys[key.KeyID] = key
	}
	attempt := Attempt{TenantID: command.Run.TenantID, RunID: command.Run.RunID, AttemptID: command.AttemptID, TokenID: command.TokenID, AttemptNumber: 1, FencingToken: 1, DeploymentID: command.DeploymentID, InstanceID: command.Candidate.InstanceID, SessionID: command.Candidate.SessionID, ServiceID: command.Candidate.ServiceID, Generation: command.Candidate.Generation, RegistryResourceVersion: command.Candidate.ResourceVersion, State: StateIssued, TransportProfile: command.Candidate.TransportProfile, Endpoint: command.Candidate.InvocationEndpoint(), Audience: "https://control.example.invalid/deployments/" + command.DeploymentID, SigningKeyID: command.SigningKey.KeyID, LeaseExpiresAt: command.LeaseExpires, TicketExpiresAt: command.TicketExpires, CreatedAt: command.Now, Traceparent: command.Run.Traceparent, Tracestate: command.Run.Tracestate, IdempotencyKeyHash: command.KeyDigest, IdempotencyRequestHash: command.RequestDigest}
	repository.attempts[command.Run.TenantID+"/"+command.KeyDigest] = attempt
	repository.requestDigests[command.Run.TenantID+"/"+command.KeyDigest] = command.RequestDigest
	return attempt, false, nil
}

func (repository *fakeRepository) SyncKeys(_ context.Context, keys []KeyMetadata, _ time.Time) error {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if repository.failure {
		return errors.New("storage unavailable")
	}
	for _, key := range keys {
		repository.keys[key.KeyID] = key
	}
	return nil
}

func verifyExpectation(attempt Attempt, now time.Time) VerifyExpectation {
	return VerifyExpectation{Issuer: "https://control.example.invalid", Audience: attempt.Audience, RunID: attempt.RunID, AttemptID: attempt.AttemptID, AgentID: "image.generate", AgentVersion: "1.0.0", SkillID: "default", DeploymentID: attempt.DeploymentID, InstanceID: attempt.InstanceID, TransportProfile: attempt.TransportProfile, Endpoint: attempt.Endpoint, Generation: attempt.Generation, FencingToken: attempt.FencingToken, ExpiresAt: attempt.TicketExpiresAt, RequiredScope: "agent:invoke", Now: now}
}

func (repository *fakeRepository) Keys(_ context.Context, now time.Time) ([]KeyMetadata, error) {
	repository.mutex.Lock()
	defer repository.mutex.Unlock()
	if repository.failure {
		return nil, errors.New("storage unavailable")
	}
	result := make([]KeyMetadata, 0, len(repository.keys))
	for _, key := range repository.keys {
		if key.VerifyUntil.After(now) {
			result = append(result, key)
		}
	}
	return normalizeKeys(result)
}

func testService(t *testing.T) (*Service, *fakeRepository, *ProcessSigner, *fakeObservations) {
	t.Helper()
	signer, err := NewProcessSigner(testNow.Add(-time.Minute), "key-20260927", time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	view := RunView{TenantID: "acme", RunID: uuid("run_", "1"), Agent: run.AgentBinding{ID: "image.generate", Version: "1.0.0", SkillID: "default", ManifestDigest: "sha256:" + strings.Repeat("a", 64)}, State: run.StateQueued, StateVersion: 1, AuthorizationSnapshotHash: "sha256:" + strings.Repeat("b", 64), Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", DeadlineAt: testNow.Add(time.Hour)}
	candidates := []Candidate{
		{InstanceID: "fallback-b", SessionID: uuid("ses_", "2"), ServiceID: "image-service", Generation: 1, ResourceVersion: 1, Priority: 10, Weight: 50, AvailableSlots: 1, TransportProfile: "direct", Endpoint: "https://runtime-b.example.invalid", LeaseExpiresAt: testNow.Add(10 * time.Minute)},
		{InstanceID: "preferred-a", SessionID: uuid("ses_", "3"), ServiceID: "image-service", Generation: 2, ResourceVersion: 1, Priority: 1, Weight: 100, AvailableSlots: 2, TransportProfile: "direct", Endpoint: "https://runtime-a.example.invalid", LeaseExpiresAt: testNow.Add(10 * time.Minute)},
	}
	repository := &fakeRepository{attempts: map[string]Attempt{}, requestDigests: map[string]string{}, keys: map[string]KeyMetadata{}}
	observations := &fakeObservations{}
	service, err := New(Dependencies{Clock: fixedClock{testNow}, IDs: &fakeIDs{}, UoW: fakeUoW{}, Observability: observations, Runs: fakeRuns{view}, Candidates: fakeCandidates{candidates}, Authorizer: fakeAuthorizer{}, Repository: repository, Signer: signer, Issuer: "https://control.example.invalid", TicketTTL: 5 * time.Minute, AttemptLease: 10 * time.Minute, MaxTokenTTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return service, repository, signer, observations
}

func validDispatchRequest() DispatchRequest {
	return DispatchRequest{Caller: Caller{TenantID: "acme", PrincipalID: uuid("prn_", "4"), CredentialID: uuid("cred_", "5"), Scopes: []string{"run:dispatch"}}, RunID: uuid("run_", "1"), IdempotencyKey: "dispatch-key-001", Metadata: platform.RequestMetadata{RequestID: uuid("req_", "6"), TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceFlags: "01"}}
}

func uuid(prefix, suffix string) string {
	value := []byte("018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c")
	copy(value[len(value)-len(suffix):], suffix)
	return prefix + string(value)
}

func keyIndex(keys []KeyMetadata, id string) int {
	for index, key := range keys {
		if key.KeyID == id {
			return index
		}
	}
	return -1
}
