package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

var baseTime = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)

type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

type fakeIDs struct {
	mu sync.Mutex
	n  int
}

func (ids *fakeIDs) next(prefix string) string {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.n++
	return fmt.Sprintf("%s018f6b6e-8a2e-7c3a-8b2a-%012x", prefix, ids.n)
}
func (ids *fakeIDs) NewRunID(context.Context) (string, error)    { return ids.next("run_"), nil }
func (ids *fakeIDs) NewOutboxID(context.Context) (string, error) { return ids.next("out_"), nil }

type txKey struct{}
type fakeUoW struct{}

func (fakeUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	return callback(context.WithValue(ctx, txKey{}, true))
}

type fakeAudit struct{ entries []observability.AuditEntry }

func (audit *fakeAudit) AppendObservation(ctx context.Context, entry observability.AuditEntry, span observability.SpanRecord) error {
	if ctx.Value(txKey{}) != true {
		return errors.New("outside transaction")
	}
	if err := observability.ValidateObservationPair(entry, span); err != nil {
		return err
	}
	audit.entries = append(audit.entries, entry)
	return nil
}

type fakeAuth struct{ deny bool }

func (auth fakeAuth) Authorize(_ context.Context, caller Caller, operation Operation, agent AgentBinding) (AuthorizationSnapshot, error) {
	if auth.deny {
		return AuthorizationSnapshot{}, NewError(CategoryAuthorization, ReasonRunForbidden)
	}
	return AuthorizationSnapshot{TenantID: caller.TenantID, PrincipalID: caller.PrincipalID, CredentialID: caller.CredentialID, Operation: operation, Agent: agent, Scopes: caller.Scopes, BudgetClass: "standard", RiskClass: "controlled"}, nil
}

type idem struct{ runID, digest string }
type effect struct{ runID, digest string }
type memoryRepo struct {
	mu       sync.Mutex
	runs     map[string]Run
	idem     map[string]idem
	commands map[string]string
	effects  map[string]effect
	outbox   []Outbox
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{runs: map[string]Run{}, idem: map[string]idem{}, commands: map[string]string{}, effects: map[string]effect{}}
}

type concurrentReplayRepo struct {
	*memoryRepo
	mu       sync.Mutex
	hideOnce bool
}

func (repo *concurrentReplayRepo) GetByIdempotency(ctx context.Context, tenant, digest string) (Run, string, error) {
	repo.mu.Lock()
	hide := repo.hideOnce
	repo.hideOnce = false
	repo.mu.Unlock()
	if hide {
		return Run{}, "", NewError(CategoryNotFound, ReasonRunNotFound)
	}
	return repo.memoryRepo.GetByIdempotency(ctx, tenant, digest)
}
func key(tenant, id string) string { return tenant + "/" + id }
func (repo *memoryRepo) tx(ctx context.Context) error {
	if ctx.Value(txKey{}) != true {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return nil
}
func (repo *memoryRepo) Create(ctx context.Context, record Run, keyDigest, requestDigest string, outbox Outbox) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.tx(ctx); err != nil {
		return err
	}
	k := key(record.TenantID, keyDigest)
	if _, ok := repo.idem[k]; ok {
		return NewError(CategoryConflict, ReasonIdempotencyConflict)
	}
	repo.runs[key(record.TenantID, record.RunID)] = record
	repo.idem[k] = idem{record.RunID, requestDigest}
	repo.outbox = append(repo.outbox, outbox)
	return nil
}
func (repo *memoryRepo) Get(_ context.Context, tenant, runID string) (Run, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	record, ok := repo.runs[key(tenant, runID)]
	if !ok {
		return Run{}, NewError(CategoryNotFound, ReasonRunNotFound)
	}
	return record, nil
}
func (repo *memoryRepo) GetByIdempotency(_ context.Context, tenant, digest string) (Run, string, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	stored, ok := repo.idem[key(tenant, digest)]
	if !ok {
		return Run{}, "", NewError(CategoryNotFound, ReasonRunNotFound)
	}
	return repo.runs[key(tenant, stored.runID)], stored.digest, nil
}
func (repo *memoryRepo) Cancel(ctx context.Context, tenant, runID string, command Command, keyDigest, commandDigest string, now time.Time, outboxFactory OutboxFactory) (Run, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.tx(ctx); err != nil {
		return Run{}, err
	}
	ck := key(tenant, runID+"/"+keyDigest)
	if prior, ok := repo.commands[ck]; ok {
		if prior != commandDigest {
			return Run{}, NewError(CategoryConflict, ReasonIdempotencyConflict)
		}
		return repo.runs[key(tenant, runID)], nil
	}
	record, ok := repo.runs[key(tenant, runID)]
	if !ok {
		return Run{}, NewError(CategoryNotFound, ReasonRunNotFound)
	}
	if record.StateVersion != command.ExpectedStateVersion {
		return Run{}, NewError(CategoryConflict, ReasonStateVersionConflict)
	}
	if record.State.Terminal() {
		return Run{}, NewError(CategoryConflict, ReasonTerminalStateConflict)
	}
	record.State = StateCancelRequested
	record.StateVersion++
	record.UpdatedAt = now
	record.CancelRequestedAt = &now
	outbox, err := outboxFactory(ctx, record.StateVersion)
	if err != nil {
		return Run{}, err
	}
	repo.runs[key(tenant, runID)] = record
	repo.commands[ck] = commandDigest
	repo.outbox = append(repo.outbox, outbox)
	return record, nil
}
func (repo *memoryRepo) Expire(ctx context.Context, tenant, runID string, version uint64, now time.Time, outbox Outbox) (Run, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.tx(ctx); err != nil {
		return Run{}, err
	}
	record := repo.runs[key(tenant, runID)]
	if record.State.Terminal() {
		return record, nil
	}
	if record.StateVersion != version {
		return Run{}, NewError(CategoryConflict, ReasonStateVersionConflict)
	}
	record.State = StateTimedOut
	record.StateVersion++
	record.UpdatedAt = now
	repo.runs[key(tenant, runID)] = record
	outbox.StateVersion = record.StateVersion
	repo.outbox = append(repo.outbox, outbox)
	return record, nil
}
func (repo *memoryRepo) ReserveEffect(ctx context.Context, reservation EffectReservation) (bool, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if err := repo.tx(ctx); err != nil {
		return false, err
	}
	k := key(reservation.TenantID, reservation.EffectID)
	if prior, ok := repo.effects[k]; ok {
		if prior.runID == reservation.RunID && prior.digest == reservation.SemanticDigest {
			return true, nil
		}
		return false, NewError(CategoryConflict, ReasonEffectConflict)
	}
	repo.effects[k] = effect{reservation.RunID, reservation.SemanticDigest}
	return false, nil
}

func newTestService(t *testing.T) (*Service, *fakeClock, *memoryRepo, *fakeAudit) {
	t.Helper()
	clock := &fakeClock{baseTime}
	repo := newMemoryRepo()
	audit := &fakeAudit{}
	service, err := New(Dependencies{Clock: clock, IDs: &fakeIDs{}, UoW: fakeUoW{}, Observability: audit, Authorizer: fakeAuth{}, Repository: repo})
	if err != nil {
		t.Fatal(err)
	}
	return service, clock, repo, audit
}
func caller() Caller {
	return Caller{TenantID: "acme", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"run:create", "run:read", "run:command"}}
}
func metadata() platform.RequestMetadata {
	return platform.RequestMetadata{RequestID: "req_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", TraceFlags: "01"}
}
func request() CreateRequest {
	return CreateRequest{Caller: caller(), Agent: AgentBinding{ID: "support.agent", Version: "1.2.3", SkillID: "answer", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Input: json.RawMessage(`[{"type":"text","text":"hello"}]`), Labels: json.RawMessage(`{"priority":"high"}`), DeadlineAt: baseTime.Add(time.Hour), Effects: EffectIntent{Level: EffectWrite, EffectID: "eff_invoice-01"}, Metadata: metadata(), Tracestate: "vendor=value", IdempotencyKey: "create-key-0001"}
}

func TestCreateReplayCancelDeadlineAndEffectFencing(t *testing.T) {
	service, clock, repo, audit := newTestService(t)
	created, err := service.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if created.State != StateQueued || created.StateVersion != 1 || len(repo.outbox) != 1 || len(audit.entries) != 1 {
		t.Fatalf("unexpected create: %#v", created)
	}
	if string(created.Labels) != `{"priority":"high"}` || created.Tracestate != "vendor=value" {
		t.Fatalf("run labels or tracestate were not durable: %#v", created)
	}
	replay, err := service.Create(context.Background(), request())
	if err != nil || replay.RunID != created.RunID || len(repo.outbox) != 1 {
		t.Fatalf("replay duplicated durable effects: %#v %v", replay, err)
	}
	changed := request()
	changed.DeadlineAt = changed.DeadlineAt.Add(time.Minute)
	if _, err = service.Create(context.Background(), changed); !hasReason(err, ReasonIdempotencyConflict) {
		t.Fatalf("changed replay accepted: %v", err)
	}
	changed = request()
	changed.Labels = json.RawMessage(`{"priority":"low"}`)
	if _, err = service.Create(context.Background(), changed); !hasReason(err, ReasonIdempotencyConflict) {
		t.Fatalf("label-drift replay accepted: %v", err)
	}
	command := CommandRequest{Caller: caller(), RunID: created.RunID, IdempotencyKey: "command-key-0001", Command: Command{CommandID: "cmd_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", ExpectedStateVersion: 1, Type: "run.cancel", Data: json.RawMessage(`{"reason":"operator"}`)}, Metadata: metadata()}
	cancelled, err := service.Cancel(context.Background(), command)
	if err != nil || cancelled.State != StateCancelRequested || cancelled.StateVersion != 2 {
		t.Fatalf("cancel failed: %#v %v", cancelled, err)
	}
	again, err := service.Cancel(context.Background(), command)
	if err != nil || again.StateVersion != 2 || len(repo.outbox) != 2 {
		t.Fatalf("cancel replay not stable: %#v %v", again, err)
	}
	semantic := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	replayed, err := service.ReserveEffect(context.Background(), caller(), created.RunID, "eff_invoice-01", semantic, metadata())
	if err != nil || replayed {
		t.Fatalf("first effect reservation: %v %v", replayed, err)
	}
	replayed, err = service.ReserveEffect(context.Background(), caller(), created.RunID, "eff_invoice-01", semantic, metadata())
	if err != nil || !replayed {
		t.Fatalf("effect replay: %v %v", replayed, err)
	}
	if _, err = service.ReserveEffect(context.Background(), caller(), created.RunID, "eff_invoice-01", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", metadata()); !hasReason(err, ReasonEffectConflict) {
		t.Fatalf("effect drift accepted: %v", err)
	}
	clock.now = baseTime.Add(2 * time.Hour)
	expired, err := service.Expire(context.Background(), caller(), created.RunID, 2, metadata())
	if err != nil || expired.State != StateTimedOut || expired.Usage != (Usage{}) {
		t.Fatalf("deadline terminal invalid: %#v %v", expired, err)
	}
	again, err = service.Cancel(context.Background(), command)
	if err != nil || again.State != StateTimedOut || len(repo.outbox) != 3 {
		t.Fatalf("durable command replay after terminal transition failed: %#v %v", again, err)
	}
}

func TestConcurrentCreateReplayRecoversCommittedWinner(t *testing.T) {
	service, _, base, _ := newTestService(t)
	winner, err := service.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	repository := &concurrentReplayRepo{memoryRepo: base, hideOnce: true}
	audit := &fakeAudit{}
	replayService, err := New(Dependencies{Clock: &fakeClock{baseTime}, IDs: &fakeIDs{}, UoW: fakeUoW{}, Observability: audit, Authorizer: fakeAuth{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayService.Create(context.Background(), request())
	if err != nil || replayed.RunID != winner.RunID || len(base.outbox) != 1 || len(audit.entries) != 1 {
		t.Fatalf("concurrent idempotent winner was not recovered: %#v %v", replayed, err)
	}
}

func TestCreateRejectsAmbiguousLabels(t *testing.T) {
	service, _, repo, _ := newTestService(t)
	candidate := request()
	candidate.Labels = json.RawMessage(`{"priority":"high","priority":"low"}`)
	if _, err := service.Create(context.Background(), candidate); !hasReason(err, ReasonInvalidRequest) {
		t.Fatalf("duplicate labels accepted: %v", err)
	}
	if len(repo.runs) != 0 || len(repo.outbox) != 0 {
		t.Fatal("invalid labels created durable run state")
	}
}
func TestAuthorizationSnapshotIsDurableAndBound(t *testing.T) {
	service, _, repo, _ := newTestService(t)
	record, err := service.Create(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot AuthorizationSnapshot
	if json.Unmarshal(record.AuthorizationSnapshot, &snapshot) != nil {
		t.Fatal("snapshot not JSON")
	}
	_, digest, _, err := snapshot.Canonical()
	if err != nil || digest != record.AuthorizationSnapshotDigest {
		t.Fatalf("authorization snapshot drift: %s %v", digest, err)
	}
	stored := repo.runs[key(record.TenantID, record.RunID)]
	if stored.AuthorizationSnapshotDigest != digest {
		t.Fatal("snapshot was not persisted")
	}
}
func hasReason(err error, reason ErrorReason) bool {
	typed, ok := AsError(err)
	return ok && typed.Reason == reason
}
