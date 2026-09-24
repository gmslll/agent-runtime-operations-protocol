package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type identityClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *identityClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}
func (clock *identityClock) advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}
func (clock *identityClock) set(value time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = value
}

type identityIDs struct {
	mu   sync.Mutex
	next map[platformports.IDKind]int
}

func (ids *identityIDs) NewID(_ context.Context, kind platformports.IDKind) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.next == nil {
		ids.next = map[platformports.IDKind]int{}
	}
	ids.next[kind]++
	n := ids.next[kind]
	switch kind {
	case platformports.IDRequest:
		return fmt.Sprintf("req_01956e7b-9abc-7def-8abc-%012x", n), nil
	case platformports.IDAudit:
		return fmt.Sprintf("aud_01956e7b-9abc-7def-8abc-%012x", n), nil
	default:
		return "", errors.New("unexpected ID kind")
	}
}

type identityFault struct {
	mu   sync.Mutex
	fail platformports.Checkpoint
}

func (fault *identityFault) Check(_ context.Context, point platformports.Checkpoint) error {
	fault.mu.Lock()
	defer fault.mu.Unlock()
	if point == fault.fail {
		return errors.New("injected identity fault")
	}
	return nil
}

type fakeRepository struct {
	mu                     sync.Mutex
	records                map[string]CredentialRecord
	getStarted, releaseGet chan struct{}
	getError               error
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{records: map[string]CredentialRecord{}}
}
func (repo *fakeRepository) snapshot() map[string]CredentialRecord {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	result := map[string]CredentialRecord{}
	for key, value := range repo.records {
		value.Scopes = cloneScopes(value.Scopes)
		result[key] = value
	}
	return result
}
func (repo *fakeRepository) restore(values map[string]CredentialRecord) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.records = values
}
func (repo *fakeRepository) Create(_ context.Context, record *CredentialRecord) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if _, exists := repo.records[record.CredentialID]; exists {
		return ErrCredentialConflict
	}
	for _, current := range repo.records {
		if current.IdempotencyDigest == record.IdempotencyDigest {
			return ErrCredentialConflict
		}
		if current.TenantID == record.TenantID && current.SubjectID == record.SubjectID {
			record.PrincipalID = current.PrincipalID
		}
	}
	repo.records[record.CredentialID] = *record
	return nil
}
func (repo *fakeRepository) Get(_ context.Context, id string) (CredentialRecord, error) {
	if repo.getStarted != nil {
		select {
		case repo.getStarted <- struct{}{}:
		default:
		}
		<-repo.releaseGet
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.getError != nil {
		return CredentialRecord{}, repo.getError
	}
	value, ok := repo.records[id]
	if !ok {
		return CredentialRecord{}, ErrCredentialNotFound
	}
	value.Scopes = cloneScopes(value.Scopes)
	return value, nil
}
func (repo *fakeRepository) GetByIdempotencyDigest(_ context.Context, digest string) (CredentialRecord, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, value := range repo.records {
		if value.IdempotencyDigest == digest {
			return value, nil
		}
	}
	return CredentialRecord{}, ErrCredentialNotFound
}
func (repo *fakeRepository) Replace(_ context.Context, old CredentialRecord, replacement *CredentialRecord, at time.Time) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	current, ok := repo.records[old.CredentialID]
	if !ok || current.Status != CredentialActive || current.Revision != old.Revision {
		return ErrCredentialConflict
	}
	current.Status, current.ReplacedAt, current.ReplacementID, current.Revision = CredentialReplaced, at, replacement.CredentialID, current.Revision+1
	repo.records[current.CredentialID] = current
	repo.records[replacement.CredentialID] = *replacement
	return nil
}
func (repo *fakeRepository) Revoke(_ context.Context, old CredentialRecord, at time.Time) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	current, ok := repo.records[old.CredentialID]
	if !ok || current.Revision != old.Revision {
		return ErrCredentialConflict
	}
	current.Status, current.RevokedAt, current.ReplacedAt, current.ReplacementID, current.Revision = CredentialRevoked, at, time.Time{}, "", current.Revision+1
	repo.records[current.CredentialID] = current
	return nil
}

type fakeObservability struct {
	mu     sync.Mutex
	audits []observability.AuditEntry
	spans  []observability.SpanRecord
	fail   bool
}

func (store *fakeObservability) AppendObservation(_ context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail {
		return errors.New("injected audit failure")
	}
	if err := observability.ValidateObservationPair(audit, span); err != nil {
		return err
	}
	store.audits = append(store.audits, audit)
	store.spans = append(store.spans, span)
	return nil
}
func (store *fakeObservability) snapshot() (int, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.audits), len(store.spans)
}
func (store *fakeObservability) restore(audits, spans int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.audits, store.spans = store.audits[:audits], store.spans[:spans]
}

type fakeUoW struct {
	repo         *fakeRepository
	observations *fakeObservability
	commitErr    error
}

func (unit *fakeUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	records := unit.repo.snapshot()
	audits, spans := unit.observations.snapshot()
	if err := callback(ctx); err != nil {
		unit.repo.restore(records)
		unit.observations.restore(audits, spans)
		return err
	}
	if unit.commitErr != nil {
		unit.repo.restore(records)
		unit.observations.restore(audits, spans)
		return unit.commitErr
	}
	return nil
}

func newIdentityService(t *testing.T) (*Service, *identityClock, *identityFault, *fakeRepository, *fakeObservability) {
	t.Helper()
	clock := &identityClock{now: time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)}
	faults := &identityFault{}
	repo := newFakeRepository()
	observations := &fakeObservability{}
	unit := &fakeUoW{repo: repo, observations: observations}
	entropy := byte(1)
	service, err := New(Dependencies{Clock: clock, IDs: &identityIDs{}, Faults: faults, UoW: unit, Observability: observations, Repository: repo, Random: func(value []byte) (int, error) {
		for index := range value {
			value[index] = entropy
		}
		entropy++
		return len(value), nil
	}, MaximumTTL: 10 * time.Minute, CacheTTL: time.Minute, AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"agent.invoke", "agent.read"}, AllowedTenants: []string{"tenant-a", "tenant-b"}})
	if err != nil {
		t.Fatal(err)
	}
	return service, clock, faults, repo, observations
}

func metadata() platform.RequestMetadata {
	return platform.RequestMetadata{RequestID: "req_01956e7b-9abc-7def-8abc-000000000001", TraceID: "4bf92f3577b34da6a3ce929d00000001", SpanID: "00f067aa00000001", TraceFlags: "01"}
}
func issueRequest(key string) IssueRequest {
	return IssueRequest{TenantID: "tenant-a", SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.read", "agent.invoke", "agent.read"}, TTL: 5 * time.Minute, IdempotencyKey: key, Metadata: metadata()}
}

func TestIssueAuthenticateAndSecretNonPersistence(t *testing.T) {
	service, _, _, repo, observations := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-1"))
	if err != nil {
		t.Fatal(err)
	}
	if issued.Credential == "" || !strings.HasPrefix(issued.Credential, credentialPrefix) {
		t.Fatal("opaque credential was not returned")
	}
	record, err := repo.Get(context.Background(), issued.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", record), issued.Credential) || record.SecretVerifier == issued.Credential || len(record.SecretVerifier) != 64 {
		t.Fatal("raw credential crossed persistence boundary")
	}
	principal, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.invoke"}, Metadata: metadata()})
	if err != nil {
		t.Fatal(err)
	}
	if principal.SubjectID != "developer-1" || principal.CredentialID != issued.CredentialID {
		t.Fatalf("unexpected principal: %+v", principal)
	}
	if audits, spans := observations.snapshot(); audits != 2 || spans != 2 {
		t.Fatalf("observations=%d/%d", audits, spans)
	}
	if replay, err := service.Issue(context.Background(), issueRequest("issue-1")); !errors.Is(err, ErrSecretUnavailable) || replay.Credential != "" || replay.CredentialID != issued.CredentialID {
		t.Fatalf("unsafe idempotent replay: %+v %v", replay, err)
	}
	mismatched := issueRequest("issue-1")
	mismatched.SubjectID = "developer-2"
	if _, err := service.Issue(context.Background(), mismatched); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("mismatched idempotency replay accepted: %v", err)
	}
	mismatched = issueRequest("issue-1")
	mismatched.TenantID = "tenant-b"
	if _, err := service.Issue(context.Background(), mismatched); !errors.Is(err, ErrCredentialConflict) {
		t.Fatalf("cross-tenant idempotency replay accepted: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-b", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cross-tenant authentication was not generic: %v", err)
	}
}

func TestAuthenticateThenCallsNextOnlyAfterAuthentication(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("middleware"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	request := AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}
	if err := service.AuthenticateThen(context.Background(), request, func(_ context.Context, principal Principal) error {
		called = true
		if principal.TenantID != "tenant-a" || principal.CredentialID != issued.CredentialID {
			t.Fatalf("unexpected principal: %+v", principal)
		}
		return nil
	}); err != nil || !called {
		t.Fatalf("authenticated operation failed: called=%v err=%v", called, err)
	}
	called = false
	request.TenantID = "tenant-b"
	if err := service.AuthenticateThen(context.Background(), request, func(context.Context, Principal) error { called = true; return nil }); !errors.Is(err, ErrUnauthenticated) || called {
		t.Fatalf("unauthenticated callback ran: called=%v err=%v", called, err)
	}
}

func TestAuthenticationExpiryScopeAndGenericFailure(t *testing.T) {
	service, clock, _, _, _ := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-2"))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		request AuthenticateRequest
		target  error
	}{
		"wrong-scope":    {request: AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.admin"}, Metadata: metadata()}, target: ErrUnauthorized},
		"wrong-audience": {request: AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "other-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}, target: ErrUnauthorized},
		"malformed":      {request: AuthenticateRequest{TenantID: "tenant-a", Credential: "not-a-credential", Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}, target: ErrUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Authenticate(context.Background(), test.request); !errors.Is(err, test.target) {
				t.Fatalf("got %v", err)
			}
		})
	}
	clock.advance(5 * time.Minute)
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired credential accepted: %v", err)
	}
	clock.set(time.Date(2026, 9, 24, 1, 1, 0, 0, time.UTC))
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("clock rollback revived expired credential: %v", err)
	}
}

func TestRotateAndRevokeInvalidateWarmCache(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-3"))
	if err != nil {
		t.Fatal(err)
	}
	auth := AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}
	if _, err := service.Authenticate(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	replacement, err := service.Rotate(context.Background(), RotateRequest{CredentialID: issued.CredentialID, IdempotencyKey: "rotate-3", TTL: 4 * time.Minute, Metadata: metadata()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), auth); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replaced cache entry accepted: %v", err)
	}
	newAuth := AuthenticateRequest{TenantID: "tenant-a", Credential: replacement.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}
	if _, err := service.Authenticate(context.Background(), newAuth); err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(context.Background(), RevokeRequest{CredentialID: replacement.CredentialID, Metadata: metadata()}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), newAuth); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked cache entry accepted: %v", err)
	}
}

func TestMutationAndAuditRollbackAtEveryCheckpoint(t *testing.T) {
	for _, checkpoint := range []platformports.Checkpoint{platformports.CheckpointBeforeUseCase, platformports.CheckpointAfterUseCase} {
		t.Run(string(checkpoint), func(t *testing.T) {
			service, _, faults, repo, observations := newIdentityService(t)
			faults.fail = checkpoint
			if _, err := service.Issue(context.Background(), issueRequest("fault-"+string(checkpoint))); err == nil {
				t.Fatal("fault was ignored")
			}
			if len(repo.snapshot()) != 0 {
				t.Fatal("credential survived rollback")
			}
			if audits, spans := observations.snapshot(); audits != 1 || spans != 1 {
				t.Fatalf("failed operation was not durably observed: %d/%d", audits, spans)
			}
		})
	}
	service, _, _, repo, observations := newIdentityService(t)
	observations.fail = true
	if _, err := service.Issue(context.Background(), issueRequest("audit-fault")); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if len(repo.snapshot()) != 0 {
		t.Fatal("credential committed without audit")
	}
}

func TestMutationCommitAndLifecycleAuditFailuresRollback(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		service, _, _, repo, observations := newIdentityService(t)
		service.deps.UoW.(*fakeUoW).commitErr = errors.New("injected commit failure")
		if _, err := service.Issue(context.Background(), issueRequest("commit-fault")); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("commit failure classification: %v", err)
		}
		if len(repo.snapshot()) != 0 {
			t.Fatal("credential survived commit failure")
		}
		if audits, spans := observations.snapshot(); audits != 0 || spans != 0 {
			t.Fatal("observation survived commit failure")
		}
	})
	for _, operation := range []string{"rotate", "revoke"} {
		t.Run(operation+"-audit", func(t *testing.T) {
			service, _, _, repo, observations := newIdentityService(t)
			issued, err := service.Issue(context.Background(), issueRequest("base-"+operation))
			if err != nil {
				t.Fatal(err)
			}
			observations.fail = true
			if operation == "rotate" {
				_, err = service.Rotate(context.Background(), RotateRequest{CredentialID: issued.CredentialID, IdempotencyKey: "rotate-audit", TTL: time.Minute, Metadata: metadata()})
			} else {
				err = service.Revoke(context.Background(), RevokeRequest{CredentialID: issued.CredentialID, Metadata: metadata()})
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("audit failure classification: %v", err)
			}
			record, loadErr := repo.Get(context.Background(), issued.CredentialID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if record.Status != CredentialActive {
				t.Fatalf("mutation committed without audit: %s", record.Status)
			}
		})
	}
	for _, failure := range []string{"after-audit", "commit"} {
		for _, operation := range []string{"rotate", "revoke"} {
			t.Run(operation+"-"+failure, func(t *testing.T) {
				service, _, faults, repo, _ := newIdentityService(t)
				issued, err := service.Issue(context.Background(), issueRequest("base-"+operation+failure))
				if err != nil {
					t.Fatal(err)
				}
				if failure == "after-audit" {
					faults.fail = platformports.CheckpointAfterUseCase
				} else {
					service.deps.UoW.(*fakeUoW).commitErr = errors.New("injected commit failure")
				}
				if operation == "rotate" {
					_, err = service.Rotate(context.Background(), RotateRequest{CredentialID: issued.CredentialID, IdempotencyKey: "rotate-" + failure, TTL: time.Minute, Metadata: metadata()})
				} else {
					err = service.Revoke(context.Background(), RevokeRequest{CredentialID: issued.CredentialID, Metadata: metadata()})
				}
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("%s failure classification: %v", failure, err)
				}
				record, loadErr := repo.Get(context.Background(), issued.CredentialID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if record.Status != CredentialActive {
					t.Fatalf("%s survived %s failure", operation, failure)
				}
			})
		}
	}
}

func TestDependencyErrorsAreUnavailableAndRejectedRequestsAreAudited(t *testing.T) {
	service, _, _, repo, observations := newIdentityService(t)
	if _, err := service.Issue(context.Background(), IssueRequest{Metadata: metadata()}); err == nil {
		t.Fatal("invalid issue accepted")
	}
	observations.mu.Lock()
	outcome := observations.audits[len(observations.audits)-1].Outcome
	observations.mu.Unlock()
	if outcome != observability.OutcomeRejected {
		t.Fatalf("invalid issue outcome=%s", outcome)
	}
	repo.getError = errors.New("database offline")
	_, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: credentialPrefix + "cred_01956e7b-9abc-7def-8abc-000000000001.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE", Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("dependency error classification: %v", err)
	}
}

func TestConcurrentDuplicateIdempotencyDoesNotReissueSecret(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	type outcome struct {
		result IssuedCredential
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := service.Issue(context.Background(), issueRequest("concurrent-key"))
			results <- outcome{result, err}
		}()
	}
	var secrets, replays int
	for range 2 {
		current := <-results
		if current.err == nil && current.result.Credential != "" {
			secrets++
		} else if errors.Is(current.err, ErrSecretUnavailable) && current.result.Credential == "" {
			replays++
		} else {
			t.Fatalf("unexpected duplicate result: %+v %v", current.result, current.err)
		}
	}
	if secrets != 1 || replays != 1 {
		t.Fatalf("secrets=%d replays=%d", secrets, replays)
	}
}

func TestCacheInvalidationFailureFailsClosedAndRollsBack(t *testing.T) {
	service, _, _, repo, observations := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-4"))
	if err != nil {
		t.Fatal(err)
	}
	service.cache.invalidateHook = func(string) error { return errors.New("cache backend unavailable") }
	if err := service.Revoke(context.Background(), RevokeRequest{CredentialID: issued.CredentialID, Metadata: metadata()}); err == nil {
		t.Fatal("cache invalidation failure was ignored")
	}
	record, _ := repo.Get(context.Background(), issued.CredentialID)
	if record.Status != CredentialActive {
		t.Fatal("revocation committed despite invalidation failure")
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("readiness stayed green")
	}
	before, _ := observations.snapshot()
	if principal, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnavailable) || principal.CredentialID != "" {
		t.Fatal("unhealthy cache failed open")
	}
	after, _ := observations.snapshot()
	if after != before+1 {
		t.Fatalf("cache dependency failure was not observed: before=%d after=%d", before, after)
	}
	observations.mu.Lock()
	outcome := observations.audits[len(observations.audits)-1].Outcome
	observations.mu.Unlock()
	if outcome != observability.OutcomeFailed {
		t.Fatalf("cache dependency outcome=%s", outcome)
	}
	observations.fail = true
	if principal, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnavailable) || principal.CredentialID != "" {
		t.Fatalf("audit failure restored authentication: %+v %v", principal, err)
	}
	service.RebuildCache()
	observations.fail = false
	if _, err := service.Issue(context.Background(), IssueRequest{Metadata: metadata()}); err == nil {
		t.Fatal("invalid recovery request accepted")
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditFailureLatchesReadinessUntilSuccessfulAudit(t *testing.T) {
	service, _, _, _, observations := newIdentityService(t)
	observations.fail = true
	if _, err := service.Issue(context.Background(), issueRequest("audit-health")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("audit failure classification: %v", err)
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("audit failure left readiness green")
	}
	observations.fail = false
	if _, err := service.Issue(context.Background(), IssueRequest{Metadata: metadata()}); err == nil {
		t.Fatal("invalid recovery request accepted")
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatalf("successful safe audit did not recover readiness: %v", err)
	}
}

func TestValidationCacheCapacityAndExpiryEviction(t *testing.T) {
	now := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC)
	cache := newValidationCache(time.Minute, 2)
	principal := Principal{TenantID: "tenant-a", CredentialID: "cred_one"}
	cache.put("oldest", principal, now.Add(time.Minute), now)
	cache.put("newer", principal, now.Add(time.Minute), now)
	cache.put("newest", principal, now.Add(time.Minute), now)
	if _, ok := cache.get("oldest", now); ok {
		t.Fatal("oldest cache entry was not evicted")
	}
	if _, ok := cache.get("newer", now); !ok {
		t.Fatal("newer cache entry was evicted")
	}

	cache = newValidationCache(time.Minute, 2)
	cache.put("expired", principal, now.Add(time.Second), now)
	cache.put("live", principal, now.Add(time.Minute), now)
	cache.put("replacement", principal, now.Add(time.Minute), now.Add(2*time.Second))
	if _, ok := cache.get("live", now.Add(2*time.Second)); !ok {
		t.Fatal("live cache entry was evicted before expired entry")
	}
	if _, ok := cache.get("expired", now.Add(2*time.Second)); ok {
		t.Fatal("expired cache entry survived")
	}
}

func TestIdentityConfigurationRejectsUnsafeCacheCapacityAndTenant(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	dependencies := service.deps
	dependencies.CacheCapacity = maximumCacheCapacity + 1
	if _, err := New(dependencies); err == nil {
		t.Fatal("unsafe cache capacity accepted")
	}
	request := issueRequest("tenant-validation")
	request.TenantID = "Tenant:Unsafe"
	if _, err := service.Issue(context.Background(), request); err == nil {
		t.Fatal("invalid tenant accepted")
	}
}

func TestValidationCacheConcurrentBoundedAccess(t *testing.T) {
	cache := newValidationCache(time.Minute, 8)
	now := time.Date(2026, 9, 24, 4, 0, 0, 0, time.UTC)
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				key := fmt.Sprintf("%02d-%03d", worker, iteration)
				cache.put(key, Principal{TenantID: "tenant-a", CredentialID: key}, now.Add(time.Minute), now)
				_, _ = cache.get(key, now)
			}
		}(worker)
	}
	wait.Wait()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) > cache.capacity {
		t.Fatalf("cache exceeded capacity: %d > %d", len(cache.entries), cache.capacity)
	}
}

func TestCacheEvictionDoesNotChangeAuthorization(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	service.cache.capacity = 1
	first, err := service.Issue(context.Background(), issueRequest("capacity-first"))
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := issueRequest("capacity-second")
	secondRequest.SubjectID = "developer-2"
	second, err := service.Issue(context.Background(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func(credential string) {
		t.Helper()
		if _, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); err != nil {
			t.Fatal(err)
		}
	}
	authenticate(first.Credential)
	authenticate(second.Credential)
	authenticate(first.Credential)
}

func TestAuthenticationAuditFailureDoesNotReleasePrincipal(t *testing.T) {
	service, _, _, _, observations := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-audit-auth"))
	if err != nil {
		t.Fatal(err)
	}
	observations.fail = true
	principal, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()})
	if err == nil {
		t.Fatal("audit failure was ignored")
	}
	if principal.PrincipalID != "" || principal.CredentialID != "" || len(principal.Scopes) != 0 {
		t.Fatalf("principal escaped failed audit: %+v", principal)
	}
}

func TestConcurrentAuthenticateCannotRefillAfterRevoke(t *testing.T) {
	service, _, _, repo, _ := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-5"))
	if err != nil {
		t.Fatal(err)
	}
	repo.getStarted, repo.releaseGet = make(chan struct{}, 1), make(chan struct{})
	authDone := make(chan error, 1)
	go func() {
		_, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()})
		authDone <- err
	}()
	<-repo.getStarted
	revokeDone := make(chan error, 1)
	go func() {
		revokeDone <- service.Revoke(context.Background(), RevokeRequest{CredentialID: issued.CredentialID, Metadata: metadata()})
	}()
	close(repo.releaseGet)
	if err := <-authDone; err != nil {
		t.Fatal(err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{TenantID: "tenant-a", Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale allow revived: %v", err)
	}
}
