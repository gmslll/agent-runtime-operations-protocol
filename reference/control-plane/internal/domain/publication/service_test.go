package publication

import (
	"context"
	"errors"
	"slices"
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

type publicationAuthorizer struct{ err error }

func (authorizer publicationAuthorizer) Authorize(context.Context, AuthorizationRequest) error {
	return authorizer.err
}

type publicationValidator struct {
	bundle ValidatedBundle
	err    error
}

func (validator publicationValidator) ValidateBundle(context.Context, []byte) (ValidatedBundle, error) {
	return validator.bundle, validator.err
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
	observation bool
}
type publicationPersistence struct {
	records      []Record
	observations int
	failAudit    bool
	failCreate   bool
}
type publicationTransactionKey struct{}

func (persistence *publicationPersistence) Within(ctx context.Context, callback func(context.Context) error) error {
	tx := &publicationTransaction{}
	if err := callback(context.WithValue(ctx, publicationTransactionKey{}, tx)); err != nil {
		return err
	}
	if tx.record != nil {
		persistence.records = append(persistence.records, *tx.record)
	}
	if tx.observation {
		persistence.observations++
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
	for _, record := range persistence.records {
		if record.TenantID == tenant && record.AgentID == agent && record.Version == version {
			return record, nil
		}
	}
	return Record{}, NewError(CategoryNotFound, ReasonNotFound)
}
func (persistence *publicationPersistence) GetByIdempotencyDigest(_ context.Context, tenant, digest string) (Record, error) {
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
	tx.observation = true
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
	if result.AgentID != request.AgentID || len(persistence.records) != 1 || persistence.observations != 1 {
		t.Fatalf("publish was not atomically committed: result=%+v records=%d audit=%d", result, len(persistence.records), persistence.observations)
	}
	replay, err := service.Publish(context.Background(), request)
	if err != nil || replay != result || len(persistence.records) != 1 || persistence.observations != 2 {
		t.Fatalf("stable replay failed: replay=%+v err=%v records=%d audit=%d", replay, err, len(persistence.records), persistence.observations)
	}
}

func TestPublicationAuditFailureRollsBackAndMarksReadiness(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	persistence.failAudit = true
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil {
		t.Fatal("audit failure accepted")
	}
	if len(persistence.records) != 0 || persistence.observations != 0 {
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
	if len(persistence.records) != 0 || persistence.observations != 1 {
		t.Fatalf("rejection mutation=%d observations=%d", len(persistence.records), persistence.observations)
	}
	persistence.failAudit = true
	request.AgentID = "still.different"
	_, err := service.Publish(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryDependency {
		t.Fatalf("audit failure did not fail closed: %v", err)
	}
}

func TestPublicationFaultAfterCreateRollsBack(t *testing.T) {
	t.Parallel()
	service, persistence := newPublicationService(t)
	service.deps.Faults = publicationFault{fail: platformports.CheckpointBeforeUseCase}
	if _, err := service.Publish(context.Background(), validPublishRequest()); err == nil {
		t.Fatal("fault accepted")
	}
	if len(persistence.records) != 0 {
		t.Fatal("record committed across fault")
	}
}

func newPublicationService(t *testing.T) (*Service, *publicationPersistence) {
	t.Helper()
	persistence := &publicationPersistence{}
	bundle := validBundle()
	bundle.CanonicalManifest = []byte(`{"kind":"AgentManifest"}`)
	service, err := New(Dependencies{Clock: publicationClock{time.Unix(100, 0).UTC()}, IDs: publicationIDs{}, Faults: publicationFault{}, UoW: persistence, Observability: persistence, Authorizer: publicationAuthorizer{}, Repository: persistence, Validator: publicationValidator{bundle: bundle}, ManifestDigester: publicationDigester{digest: bundle.ManifestDigest}, Fingerprinter: publicationFingerprinter{digest: strings64("e")}})
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
