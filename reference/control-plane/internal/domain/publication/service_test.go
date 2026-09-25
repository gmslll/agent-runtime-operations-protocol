package publication

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type publicationClock struct{ now time.Time }

func (clock publicationClock) Now() time.Time { return clock.now }

type publicationIDs struct{}

func (publicationIDs) NewID(_ context.Context, kind platformports.IDKind) (string, error) {
	if kind == platformports.IDAudit {
		return "aud_018f0000-0000-7000-8000-000000000010", nil
	}
	return "req_018f0000-0000-7000-8000-000000000011", nil
}

type publicationFault struct{ fail platformports.Checkpoint }

func (fault publicationFault) Check(_ context.Context, point platformports.Checkpoint) error {
	if point == fault.fail {
		return errors.New("fault")
	}
	return nil
}

type publicationAuthorizer struct {
	mu       sync.Mutex
	err      error
	requests []AuthorizationRequest
}

func (authorizer *publicationAuthorizer) Authorize(_ context.Context, request AuthorizationRequest) error {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	authorizer.requests = append(authorizer.requests, request)
	return authorizer.err
}
func (authorizer *publicationAuthorizer) captured() []AuthorizationRequest {
	authorizer.mu.Lock()
	defer authorizer.mu.Unlock()
	return slices.Clone(authorizer.requests)
}

type publicationValidator struct {
	mu        sync.Mutex
	bundle    ValidatedBundle
	err       error
	panicWith error
	seen      []byte
}

func (validator *publicationValidator) ValidateBundle(_ context.Context, archive []byte) (ValidatedBundle, error) {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	validator.seen = archive
	if validator.panicWith != nil {
		panic(validator.panicWith)
	}
	return validator.bundle, validator.err
}
func (validator *publicationValidator) seenBytes() []byte {
	validator.mu.Lock()
	defer validator.mu.Unlock()
	return slices.Clone(validator.seen)
}

type publicationDigester struct{ digest string }

func (digester publicationDigester) DigestManifest(context.Context, []byte) (string, error) {
	return digester.digest, nil
}

type publicationFingerprinter struct{ digest string }

func (fingerprinter publicationFingerprinter) DigestRequest(context.Context, RequestFingerprint) (string, error) {
	return fingerprinter.digest, nil
}

type publicationTransaction struct {
	record      *Record
	observation *observability.AuditEntry
}
type publicationPersistence struct {
	mu           sync.Mutex
	records      []Record
	observations []observability.AuditEntry
	failAudit    bool
	failCreate   bool
	failLookup   bool
}
type publicationTransactionKey struct{}

func (persistence *publicationPersistence) Within(ctx context.Context, callback func(context.Context) error) error {
	tx := &publicationTransaction{}
	if err := callback(context.WithValue(ctx, publicationTransactionKey{}, tx)); err != nil {
		return err
	}
	if tx.record != nil {
		persistence.mu.Lock()
		persistence.records = append(persistence.records, *tx.record)
		persistence.mu.Unlock()
	}
	if tx.observation != nil {
		persistence.mu.Lock()
		persistence.observations = append(persistence.observations, *tx.observation)
		persistence.mu.Unlock()
	}
	return nil
}
func (persistence *publicationPersistence) Create(ctx context.Context, record *Record) error {
	if persistence.failCreate {
		return NewError(CategoryConflict, ReasonImmutableConflict)
	}
	tx, ok := ctx.Value(publicationTransactionKey{}).(*publicationTransaction)
	if !ok {
		return errors.New("create outside transaction")
	}
	copy := *record
	copy.CanonicalManifest = slices.Clone(record.CanonicalManifest)
	tx.record = &copy
	return nil
}
func (persistence *publicationPersistence) Get(_ context.Context, tenant, agent, version string) (Record, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.failLookup {
		return Record{}, errors.New("repository unavailable")
	}
	for _, record := range persistence.records {
		if record.TenantID == tenant && record.AgentID == agent && record.Version == version {
			return record, nil
		}
	}
	return Record{}, NewError(CategoryNotFound, ReasonNotFound)
}
func (persistence *publicationPersistence) GetByIdempotencyDigest(_ context.Context, tenant, digest string) (Record, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.failLookup {
		return Record{}, errors.New("repository unavailable")
	}
	for _, record := range persistence.records {
		if record.TenantID == tenant && record.IdempotencyKeyDigest == digest {
			return record, nil
		}
	}
	return Record{}, NewError(CategoryNotFound, ReasonNotFound)
}
func (persistence *publicationPersistence) AppendObservation(ctx context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if persistence.failAudit {
		return errors.New("audit unavailable")
	}
	if err := observability.ValidateObservationPair(audit, span); err != nil {
		return err
	}
	tx, ok := ctx.Value(publicationTransactionKey{}).(*publicationTransaction)
	if !ok {
		return errors.New("audit outside transaction")
	}
	copy := audit
	tx.observation = &copy
	return nil
}

func TestPublicationMutationAndAuditCommitAtomically(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	request := validPublishRequest()
	result, err := service.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if result.AgentID != request.AgentID || len(persistence.records) != 1 || len(persistence.observations) != 1 || persistence.observations[0].HTTPStatus != 201 {
		t.Fatalf("publish was not atomically committed: result=%+v records=%d audit=%d", result, len(persistence.records), len(persistence.observations))
	}
	replay, err := service.Publish(context.Background(), request)
	if err != nil || replay != result || len(persistence.records) != 1 || len(persistence.observations) != 2 || persistence.observations[1].HTTPStatus != 201 {
		t.Fatalf("stable replay failed: replay=%+v err=%v records=%d audit=%d", replay, err, len(persistence.records), len(persistence.observations))
	}
}

func TestPublicationAuditFailureRollsBackAndMarksReadiness(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	persistence.failAudit = true
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil {
		t.Fatal("audit failure accepted")
	}
	if len(persistence.records) != 0 || len(persistence.observations) != 0 {
		t.Fatal("mutation escaped failed audit transaction")
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("readiness remained healthy after audit failure")
	}
}

func TestPublicationRejectedMutationIsAuditedAndFailClosed(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	request := validPublishRequest()
	request.AgentID = "different.agent"
	if _, err := service.Publish(context.Background(), request); err == nil {
		t.Fatal("agent mismatch accepted")
	}
	if len(persistence.records) != 0 || len(persistence.observations) != 1 {
		t.Fatalf("rejection mutation=%d observations=%d", len(persistence.records), len(persistence.observations))
	}
	persistence.failAudit = true
	request.AgentID = "still.different"
	_, err := service.Publish(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryDependency {
		t.Fatalf("audit failure did not fail closed: %v", err)
	}
	persistence.failAudit = false
	request.AgentID = "third.different"
	if _, err := service.Publish(context.Background(), request); err == nil {
		t.Fatal("invalid publication accepted")
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("successful rejection audit cleared prior audit failure")
	}
	if _, err := service.Publish(context.Background(), validPublishRequest()); err != nil {
		t.Fatal(err)
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatalf("successful publication did not restore health: %v", err)
	}
}

func TestPublicationFaultAfterCreateRollsBack(t *testing.T) {
	for _, checkpoint := range []platformports.Checkpoint{platformports.CheckpointBeforeUseCase, platformports.CheckpointAfterUseCase} {
		checkpoint := checkpoint
		t.Run(string(checkpoint), func(t *testing.T) {
			t.Parallel()
			service, persistence := newPublicationService(t)
			service.deps.Faults = publicationFault{fail: checkpoint}
			if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil {
				t.Fatal("fault accepted")
			}
			if len(persistence.records) != 0 {
				t.Fatal("record committed across fault")
			}
		})
	}
}

func TestPublicationAuthorizationBindsCallerOperationAndTenant(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	authorizer := service.deps.Authorizer.(*publicationAuthorizer)
	request := validPublishRequest()
	if _, err := service.Publish(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(context.Background(), GetRequest{AgentID: request.AgentID, Version: "1.0.0", Caller: request.Caller, Metadata: request.Metadata}); err != nil {
		t.Fatal(err)
	}
	captured := authorizer.captured()
	if len(captured) != 2 || captured[0].Operation != OperationPublish || captured[1].Operation != OperationGet || captured[0].Caller.TenantID != request.Caller.TenantID || !slices.Equal(captured[0].Caller.Scopes, request.Caller.Scopes) {
		t.Fatalf("authorization requests not exact: %+v", captured)
	}
	if len(persistence.observations) != 2 || persistence.observations[1].HTTPStatus != 200 {
		t.Fatalf("get audit missing: %+v", persistence.observations)
	}
	wrongTenant := request.Caller
	wrongTenant.TenantID = "tenant-b"
	if _, err := service.Get(context.Background(), GetRequest{AgentID: request.AgentID, Version: "1.0.0", Caller: wrongTenant, Metadata: request.Metadata}); err == nil {
		t.Fatal("cross-tenant read succeeded")
	}
}

func TestPublicationIdempotencyBindsCallerScopeAndBundle(t *testing.T) {
	t.Parallel()
	mutations := []func(*PublishRequest, *publicationValidator){
		func(request *PublishRequest, _ *publicationValidator) {
			request.Caller.PrincipalID = "prn_018f0000-0000-7000-8000-000000000009"
		},
		func(request *PublishRequest, _ *publicationValidator) {
			request.Caller.Scopes = []string{"agent:publish"}
		},
		func(_ *PublishRequest, validator *publicationValidator) {
			validator.bundle.BundleSemanticDigest = digest("c")
		},
	}
	for index, mutate := range mutations {
		service, _ := newPublicationService(t)
		request := validPublishRequest()
		if _, err := service.Publish(context.Background(), request); err != nil {
			t.Fatalf("seed %d: %v", index, err)
		}
		validator := service.deps.Validator.(*publicationValidator)
		mutate(&request, validator)
		_, err := service.Publish(context.Background(), request)
		failure, ok := AsError(err)
		if !ok || failure.Reason != ReasonIdempotencyConflict {
			t.Fatalf("mutation %d err=%v", index, err)
		}
	}
}

func TestPublicationImmutableAndConcurrentPublish(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	requests := []PublishRequest{validPublishRequest(), validPublishRequest()}
	requests[1].IdempotencyKey = "idempotency-2"
	errorsByIndex := make([]error, 2)
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, errorsByIndex[index] = service.Publish(context.Background(), requests[index])
		}(index)
	}
	wait.Wait()
	successes, conflicts := 0, 0
	for _, err := range errorsByIndex {
		if err == nil {
			successes++
		} else if failure, ok := AsError(err); ok && failure.Reason == ReasonImmutableConflict {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 || len(persistence.records) != 1 {
		t.Fatalf("success=%d conflict=%d records=%d errors=%v", successes, conflicts, len(persistence.records), errorsByIndex)
	}
}

func TestPublicationDependencyHealthIsNotClearedByRejectedAudit(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	persistence.failLookup = true
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil {
		t.Fatal("repository outage accepted")
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("repository outage did not affect readiness")
	}
	persistence.failLookup = false
	request := validPublishRequest()
	request.AgentID = "different.agent"
	if _, err := service.Publish(context.Background(), request); err == nil {
		t.Fatal("invalid publication accepted")
	}
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("rejection audit incorrectly cleared dependency health")
	}
	if _, err := service.Publish(context.Background(), validPublishRequest()); err != nil {
		t.Fatal(err)
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatalf("successful dependency path did not recover readiness: %v", err)
	}
}

func TestPublicationAuthorizationAndValidationFailuresAreRedacted(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	authorizer := service.deps.Authorizer.(*publicationAuthorizer)
	authorizer.err = errors.New("Bearer secret-authorizer-sentinel")
	validator := service.deps.Validator.(*publicationValidator)
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("authorization error leaked: %v", err)
	}
	if len(validator.seenBytes()) != 0 {
		t.Fatal("unauthorized request reached bundle validator")
	}
	authorizer.err = nil
	validator.err = errors.New("archive-secret-sentinel")
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("validation error leaked: %v", err)
	}
	for _, observation := range persistence.observations {
		if strings.Contains(observation.Operation, "sentinel") {
			t.Fatal("audit leaked sentinel")
		}
	}
}

func TestPublicationClearsPrivateBundleCopyOnValidatorPanic(t *testing.T) {
	t.Parallel()
	service, _ := newPublicationService(t)
	validator := service.deps.Validator.(*publicationValidator)
	validator.panicWith = errors.New("panic")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("validator panic was not propagated")
			}
		}()
		_, _ = service.Publish(context.Background(), validPublishRequest())
	}()
	for _, value := range validator.seenBytes() {
		if value != 0 {
			t.Fatal("private bundle copy retained bytes after panic")
		}
	}
}

func newPublicationService(t *testing.T) (*Service, *publicationPersistence) {
	t.Helper()
	persistence := &publicationPersistence{}
	bundle := validBundle()
	bundle.CanonicalManifest = []byte(`{"kind":"AgentManifest"}`)
	service, err := New(Dependencies{Clock: publicationClock{time.Unix(100, 0).UTC()}, IDs: publicationIDs{}, Faults: publicationFault{}, UoW: persistence, Observability: persistence, Authorizer: &publicationAuthorizer{}, Repository: persistence, Validator: &publicationValidator{bundle: bundle}, ManifestDigester: publicationDigester{digest: bundle.ManifestDigest}, Fingerprinter: SHA256RequestFingerprinter{}})
	if err != nil {
		t.Fatal(err)
	}
	return service, persistence
}

func validPublishRequest() PublishRequest {
	return PublishRequest{AgentID: "example.agent", IdempotencyKey: "idempotency-1", Bundle: []byte("archive-sentinel"), Caller: validCaller(), Metadata: platform.RequestMetadata{RequestID: "req_018f0000-0000-7000-8000-000000000001", TraceID: "018f0000000070008000000000000002", SpanID: "018f000000007003"}}
}
func strings64(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}

var _ Repository = (*publicationPersistence)(nil)
var _ platformports.UnitOfWork = (*publicationPersistence)(nil)
var _ observability.ObservationWriter = (*publicationPersistence)(nil)
