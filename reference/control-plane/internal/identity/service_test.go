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
		if current.SubjectID == record.SubjectID {
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
}

func (unit *fakeUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	records := unit.repo.snapshot()
	audits, spans := unit.observations.snapshot()
	if err := callback(ctx); err != nil {
		unit.repo.restore(records)
		unit.observations.restore(audits, spans)
		return err
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
	}, MaximumTTL: 10 * time.Minute, CacheTTL: time.Minute, AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"agent.invoke", "agent.read"}})
	if err != nil {
		t.Fatal(err)
	}
	return service, clock, faults, repo, observations
}

func metadata() platform.RequestMetadata {
	return platform.RequestMetadata{RequestID: "req_01956e7b-9abc-7def-8abc-000000000001", TraceID: "4bf92f3577b34da6a3ce929d00000001", SpanID: "00f067aa00000001", TraceFlags: "01"}
}
func issueRequest(key string) IssueRequest {
	return IssueRequest{SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.read", "agent.invoke", "agent.read"}, TTL: 5 * time.Minute, IdempotencyKey: key, Metadata: metadata()}
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
	principal, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.invoke"}, Metadata: metadata()})
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
		"wrong-scope":    {request: AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.admin"}, Metadata: metadata()}, target: ErrUnauthorized},
		"wrong-audience": {request: AuthenticateRequest{Credential: issued.Credential, Audience: "other-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}, target: ErrUnauthorized},
		"malformed":      {request: AuthenticateRequest{Credential: "not-a-credential", Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}, target: ErrUnauthenticated},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Authenticate(context.Background(), test.request); !errors.Is(err, test.target) {
				t.Fatalf("got %v", err)
			}
		})
	}
	clock.advance(5 * time.Minute)
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired credential accepted: %v", err)
	}
	clock.set(time.Date(2026, 9, 24, 1, 1, 0, 0, time.UTC))
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("clock rollback revived expired credential: %v", err)
	}
}

func TestRotateAndRevokeInvalidateWarmCache(t *testing.T) {
	service, _, _, _, _ := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-3"))
	if err != nil {
		t.Fatal(err)
	}
	auth := AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}
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
	newAuth := AuthenticateRequest{Credential: replacement.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}
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
			if audits, spans := observations.snapshot(); audits != 0 || spans != 0 {
				t.Fatal("observation survived rollback")
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

func TestCacheInvalidationFailureFailsClosedAndRollsBack(t *testing.T) {
	service, _, _, repo, _ := newIdentityService(t)
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
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); err == nil {
		t.Fatal("unhealthy cache failed open")
	}
	service.RebuildCache()
	if err := service.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationAuditFailureDoesNotReleasePrincipal(t *testing.T) {
	service, _, _, _, observations := newIdentityService(t)
	issued, err := service.Issue(context.Background(), issueRequest("issue-audit-auth"))
	if err != nil {
		t.Fatal(err)
	}
	observations.fail = true
	principal, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()})
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
		_, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()})
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
	if _, err := service.Authenticate(context.Background(), AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"agent.read"}, Metadata: metadata()}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale allow revived: %v", err)
	}
}
